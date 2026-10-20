package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Tool call arguments are persisted in several places (toolcalls.args, the
// vector store, Langfuse spans, the Graphiti knowledge graph, log fields) and
// are exposed through the GraphQL and REST APIs. They routinely carry API keys
// and passwords supplied by the user, so the persisted copy has to be masked
// while the copy handed to the tool handler stays intact.
//
// Two layers are applied:
//
//   - a key-name layer, which masks values whose JSON key identifies a secret
//     ("api_key", "password", "token", ...);
//   - a value-shape layer, which masks a credential written inside a free-text
//     value, either anchored to a credential-shaped key or in a well known
//     provider format.
//
// The value-shape layer deliberately does NOT reuse the process-wide
// anonymizer patterns. Those also match IP addresses, URLs, CVE numbers and
// command line password flags, so applying them to tool arguments destroys the
// audit trail of a penetration test - "nmap -sV -p 443 192.168.1.10" becomes
// "nmap -sV -p <password flag> <ipv4>" - without protecting a single secret.
//
// Known limits of the value-shape layer: a credential that is only recognisable
// by its position, such as the password in "mysql -pSecret", is not caught. The
// key-name layer is the reliable one for structured arguments.

// redactedMarker replaces a secret value. The JSON type of the original value
// is preserved so that a client validating the arguments against the tool's
// declared schema still passes.
const redactedMarker = "[redacted]"

// secretKeyMarkers are matched against the normalized (lowercase,
// alphanumeric-only) form of a JSON key. A key whose normalized form contains
// any of these is treated as naming a secret.
var secretKeyMarkers = []string{
	"apikey",
	"secret",
	"password",
	"passwd",
	"passphrase",
	"pswd",
	"token",
	"cred",
	"authorization",
	"authentication",
	"bearer",
	"privatekey",
	"accesskey",
	"sessionkey",
	"sessionid",
	"authkey",
	"sshkey",
	"cookie",
	"jwt",
}

// secretKeyExact holds normalized key names that name a secret on their own.
// They are matched exactly because a substring match would produce false
// positives ("pass" in "bypass", "pin" in "shipping").
var secretKeyExact = map[string]struct{}{
	"auth": {},
	"pass": {},
	"pwd":  {},
	"psw":  {},
	"otp":  {},
	"pin":  {},
}

// nonSecretKeys are normalized key names that look like secrets but carry a
// harmless numeric setting, such as an LLM token budget. They suppress the
// key-name layer only for numeric values, so a credential hidden under one of
// these names is still masked.
var nonSecretKeys = map[string]struct{}{
	"maxtoken":            {},
	"maxtokens":           {},
	"maxinputtokens":      {},
	"maxoutputtokens":     {},
	"maxcompletiontokens": {},
	"maxnewtokens":        {},
	"numtokens":           {},
	"tokencount":          {},
	"totaltokens":         {},
	"tokensused":          {},
}

// valueSecretPatterns match a credential inside a free-text value. The first
// one is anchored to a credential-shaped key; the rest are the well known
// provider formats.
var valueSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)([a-z0-9_\-]*(?:api[_-]?key|secret|password|passwd|psw|pwd|token|credential|creds|auth|bearer)[a-z0-9_\-]*)(\s*[=:]\s*)((?:bearer\s+|basic\s+)?[^\s,;&"']+)`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{16,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{16,}\b`),
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9\-]{10,}\b`),
	regexp.MustCompile(`(?i)-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`),
}

// RedactToolArgs returns a copy of the tool call arguments with secret values
// masked. The input is never modified, so the caller can still hand the real
// arguments to the tool handler.
//
// The copy preserves the key order and the escaping of the original document,
// so what is persisted is still the document the model produced, with only the
// secret values replaced. Arguments that are not a single complete JSON
// document are returned unchanged apart from the value-shape layer, which
// works on the raw text.
func RedactToolArgs(args json.RawMessage) json.RawMessage {
	if len(args) == 0 {
		return args
	}

	redacted, err := redactJSON(args)
	if err != nil {
		// A genuine parse failure leaves no safe document to persist, so
		// fall back to the raw-text value layer. A doubled or trailing
		// document is handled inside redactJSON, which keeps the first
		// complete document and drops the extra bytes.
		if !isTrailingDataError(err) {
			return redactRaw(args)
		}
	}

	return redacted
}

// errTrailingData marks input that held a valid document followed by extra
// bytes. The first document is still returned, only the trailing part is
// dropped, so it must not be treated like a parse failure.
var errTrailingData = errors.New("unexpected trailing data after document")

func isTrailingDataError(err error) bool {
	return errors.Is(err, errTrailingData)
}

// redactRaw applies the value-shape layer to arguments that are not a single
// complete JSON document.
func redactRaw(args json.RawMessage) json.RawMessage {
	return json.RawMessage(redactString(string(args)))
}

// redactJSON rewrites the document token by token so that the key order and
// the escaping of everything that is not masked survive untouched.
func redactJSON(src []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(src))
	decoder.UseNumber()

	var buf bytes.Buffer
	if err := writeRedactedValue(decoder, &buf, ""); err != nil {
		return nil, err
	}

	// The decoder accepts a valid prefix and silently drops whatever follows
	// it, so a doubled document must not be persisted as if it were the whole
	// thing. After the walk the input offset points just past the first
	// document; anything other than whitespace after it is trailing data.
	// The first document was rewritten correctly, so return it alongside the
	// marker rather than discarding it.
	if offset := decoder.InputOffset(); hasNonWhitespace(src[offset:]) {
		return buf.Bytes(), errTrailingData
	}

	return buf.Bytes(), nil
}

// hasNonWhitespace reports whether the slice contains anything other than JSON
// whitespace.
func hasNonWhitespace(rest []byte) bool {
	for _, b := range rest {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return true
		}
	}
	return false
}

// writeRedactedValue reads one value and writes its redacted form.
func writeRedactedValue(decoder *json.Decoder, buf *bytes.Buffer, key string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}

	switch typed := token.(type) {
	case json.Delim:
		switch typed {
		case '{':
			return writeRedactedObject(decoder, buf, key)
		case '[':
			return writeRedactedArray(decoder, buf, key)
		}
		return fmt.Errorf("unexpected delimiter %v", typed)
	case string:
		if isSecretKey(key) {
			writeJSONString(buf, redactedMarker)
			return nil
		}
		writeJSONString(buf, redactString(typed))
		return nil
	case json.Number:
		if isSecretKey(key) && !isBudgetKey(key) {
			buf.WriteString("0")
			return nil
		}
		buf.WriteString(typed.String())
		return nil
	case bool:
		if isSecretKey(key) {
			buf.WriteString("false")
			return nil
		}
		buf.WriteString(strconv.FormatBool(typed))
		return nil
	case nil:
		buf.WriteString("null")
		return nil
	}

	return fmt.Errorf("unexpected token %v", token)
}

// writeRedactedObject reads an object and writes its redacted form.
func writeRedactedObject(decoder *json.Decoder, buf *bytes.Buffer, key string) error {
	if isSecretKey(key) {
		return writeMaskedComposite(decoder, buf, '{')
	}

	buf.WriteByte('{')
	first := true
	for decoder.More() {
		if !first {
			buf.WriteByte(',')
		}
		first = false

		nameToken, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := nameToken.(string)
		if !ok {
			return fmt.Errorf("unexpected object key %v", nameToken)
		}
		writeJSONString(buf, name)
		buf.WriteByte(':')

		if err := writeRedactedValue(decoder, buf, name); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	buf.WriteByte('}')

	return nil
}

// writeRedactedArray reads an array and writes its redacted form.
func writeRedactedArray(decoder *json.Decoder, buf *bytes.Buffer, key string) error {
	if isSecretKey(key) {
		return writeMaskedComposite(decoder, buf, '[')
	}

	buf.WriteByte('[')
	first := true
	for decoder.More() {
		if !first {
			buf.WriteByte(',')
		}
		first = false

		if err := writeRedactedValue(decoder, buf, key); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	buf.WriteByte(']')

	return nil
}

// writeMaskedComposite consumes the body of an object or array whose opening
// delimiter has already been read, and writes a single marker in its place, so
// that a secret holding a nested structure leaks neither its values nor its
// shape.
func writeMaskedComposite(decoder *json.Decoder, buf *bytes.Buffer, open json.Delim) error {
	close_ := json.Delim('}')
	if open == '[' {
		close_ = ']'
	}

	for {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		if delim, ok := token.(json.Delim); ok && delim == close_ {
			break
		}
	}

	writeJSONString(buf, redactedMarker)
	return nil
}

// writeJSONString writes a JSON string without escaping HTML characters, so
// that a URL keeps its query separators and stays searchable.
func writeJSONString(buf *bytes.Buffer, value string) {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		buf.WriteString(`""`)
		return
	}
	buf.Write(bytes.TrimRight(encoded.Bytes(), "\n"))
}

// redactString applies the value-shape layer to a single string.
func redactString(value string) string {
	for idx, pattern := range valueSecretPatterns {
		if idx == 0 {
			// The anchored pattern keeps the key and the separator, only the
			// credential that follows them is replaced.
			value = pattern.ReplaceAllString(value, "${1}${2}"+redactedMarker)
			continue
		}
		value = pattern.ReplaceAllString(value, redactedMarker)
	}
	return value
}

// isSecretKey reports whether the JSON key names a secret.
func isSecretKey(key string) bool {
	normalized := normalizeKey(key)
	if normalized == "" {
		return false
	}
	if _, ok := secretKeyExact[normalized]; ok {
		return true
	}
	for _, marker := range secretKeyMarkers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

// isBudgetKey reports whether the key is one of the numeric settings that
// only look like a secret.
func isBudgetKey(key string) bool {
	_, ok := nonSecretKeys[normalizeKey(key)]
	return ok
}

// normalizeKey lowercases the key and drops everything that is not a letter or
// a digit, so "toolbench_rapidapi_key" and "toolbenchRapidapiKey" both become
// "toolbenchrapidapikey".
func normalizeKey(key string) string {
	var builder strings.Builder
	builder.Grow(len(key))
	for _, char := range strings.ToLower(key) {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			builder.WriteRune(char)
		}
	}
	return builder.String()
}

// TruncateRunes returns at most limit bytes of s, backing up past any partial
// multi-byte rune so the result is always valid UTF-8. It is used for log
// snippets, where a byte slice can split a replacement marker in half.
func TruncateRunes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	clipped := s[:limit]
	for len(clipped) > 0 && !utf8.ValidString(clipped) {
		clipped = clipped[:len(clipped)-1]
	}
	return clipped
}

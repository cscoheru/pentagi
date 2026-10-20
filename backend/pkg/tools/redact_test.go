package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactToolArgsMasksSecretKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     string
		wantKeys []string
		wantGone []string
	}{
		{
			name:     "api key field",
			args:     `{"api_key":"sk-super-secret-value"}`,
			wantKeys: []string{"api_key"},
			wantGone: []string{"sk-super-secret-value"},
		},
		{
			name:     "rapidapi style key",
			args:     `{"toolbench_rapidapi_key":"abc123"}`,
			wantKeys: []string{"toolbench_rapidapi_key"},
			wantGone: []string{"abc123"},
		},
		{
			name:     "password field",
			args:     `{"username":"user","password":"synthetic-test-password"}`,
			wantKeys: []string{"username", "password"},
			wantGone: []string{"synthetic-test-password"},
		},
		{
			name:     "short password key",
			args:     `{"psw":"synthetic-test-password"}`,
			wantKeys: []string{"psw"},
			wantGone: []string{"synthetic-test-password"},
		},
		{
			name:     "credentials field",
			args:     `{"creds":{"user":"u","pass":"synthetic-test-password"}}`,
			wantKeys: []string{"creds"},
			wantGone: []string{"synthetic-test-password"},
		},
		{
			name:     "token field",
			args:     `{"token":"ghp_abcdefghijklmnop"}`,
			wantKeys: []string{"token"},
			wantGone: []string{"ghp_abcdefghijklmnop"},
		},
		{
			name:     "authorization field",
			args:     `{"Authorization":"Bearer eyJhbGciOi"}`,
			wantKeys: []string{"Authorization"},
			wantGone: []string{"eyJhbGciOi"},
		},
		{
			name:     "auth field",
			args:     `{"auth":"hunter2"}`,
			wantKeys: []string{"auth"},
			wantGone: []string{"hunter2"},
		},
		{
			name:     "session id field",
			args:     `{"session_id":"abc123"}`,
			wantKeys: []string{"session_id"},
			wantGone: []string{"abc123"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			redacted := string(RedactToolArgs(json.RawMessage(tt.args)))

			for _, key := range tt.wantKeys {
				if !strings.Contains(redacted, key) {
					t.Errorf("redacted args %q lost key %q", redacted, key)
				}
			}
			for _, secret := range tt.wantGone {
				if strings.Contains(redacted, secret) {
					t.Errorf("redacted args %q still contains secret %q", redacted, secret)
				}
			}
		})
	}
}

func TestRedactToolArgsKeepsHarmlessFields(t *testing.T) {
	t.Parallel()

	args := `{"query":"普洱茶 最新资讯","max_tokens":4096,"author":"someone","tokens_used":12}`
	redacted := string(RedactToolArgs(json.RawMessage(args)))

	for _, keep := range []string{"普洱茶 最新资讯", "4096", "someone", "12"} {
		if !strings.Contains(redacted, keep) {
			t.Errorf("redacted args %q dropped harmless value %q", redacted, keep)
		}
	}
}

// A key that only looks like a secret must not be masked, otherwise the audit
// trail loses ordinary fields.
func TestRedactToolArgsDoesNotMaskLookalikeKeys(t *testing.T) {
	t.Parallel()

	args := `{"bypass":true,"passthrough":"on","shipping":"fast"}`
	redacted := string(RedactToolArgs(json.RawMessage(args)))

	for _, keep := range []string{"bypass", "passthrough", "shipping"} {
		if !strings.Contains(redacted, keep) {
			t.Errorf("redacted args %q dropped lookalike key %q", redacted, keep)
		}
	}
}

// A budget key only stays readable while it holds a number. A credential
// hidden under the same name must still be masked.
func TestRedactToolArgsBudgetKeyOnlyExemptsNumbers(t *testing.T) {
	t.Parallel()

	numeric := string(RedactToolArgs(json.RawMessage(`{"max_tokens":4096}`)))
	if !strings.Contains(numeric, "4096") {
		t.Errorf("redacted args %q dropped the numeric budget", numeric)
	}

	credential := string(RedactToolArgs(json.RawMessage(`{"max_tokens":"synthetic-secret"}`)))
	if strings.Contains(credential, "synthetic-secret") {
		t.Errorf("redacted args %q kept a credential under a budget key", credential)
	}
}

func TestRedactToolArgsMasksNestedValues(t *testing.T) {
	t.Parallel()

	args := `{"config":{"api_key":"nested-secret","url":"https://example.test"},"list":[{"password":"deep-secret"}]}`
	redacted := string(RedactToolArgs(json.RawMessage(args)))

	for _, secret := range []string{"nested-secret", "deep-secret"} {
		if strings.Contains(redacted, secret) {
			t.Errorf("redacted args %q still contains secret %q", redacted, secret)
		}
	}
	if !strings.Contains(redacted, "https://example.test") {
		t.Errorf("redacted args %q dropped the non-secret url", redacted)
	}
}

// The persisted copy has to stay the document the model produced, so the key
// order and the escaping of everything that is not masked survive.
func TestRedactToolArgsPreservesDocumentShape(t *testing.T) {
	t.Parallel()

	args := `{"zebra":"last","alpha":"first","url":"https://x.test/?a=1&b=2"}`
	redacted := string(RedactToolArgs(json.RawMessage(args)))

	if !strings.Contains(redacted, `"zebra":"last","alpha":"first"`) {
		t.Errorf("redacted args %q did not preserve the key order", redacted)
	}
	if !strings.Contains(redacted, "a=1&b=2") {
		t.Errorf("redacted args %q escaped the query separator", redacted)
	}
}

func TestRedactToolArgsPreservesNumberPrecision(t *testing.T) {
	t.Parallel()

	args := `{"big":12345678901234567890,"float":1.5}`
	redacted := string(RedactToolArgs(json.RawMessage(args)))

	if !strings.Contains(redacted, "12345678901234567890") {
		t.Errorf("redacted args %q lost number precision", redacted)
	}
	if !strings.Contains(redacted, "1.5") {
		t.Errorf("redacted args %q changed the float value", redacted)
	}
}

func TestRedactToolArgsLeavesInputUntouched(t *testing.T) {
	t.Parallel()

	original := json.RawMessage(`{"api_key":"synthetic-secret"}`)
	RedactToolArgs(original)

	if !strings.Contains(string(original), "synthetic-secret") {
		t.Fatal("RedactToolArgs() modified the input arguments")
	}
}

// Arguments that are not a single complete JSON document must not be
// persisted as if they were.
func TestRedactToolArgsHandlesInvalidJSON(t *testing.T) {
	t.Parallel()

	redacted := RedactToolArgs(json.RawMessage(`{invalid`))
	if !strings.HasPrefix(string(redacted), "{invalid") {
		t.Fatalf("RedactToolArgs() = %q, want the input preserved", redacted)
	}
}

func TestRedactToolArgsRejectsTrailingData(t *testing.T) {
	t.Parallel()

	redacted := string(RedactToolArgs(json.RawMessage(`{"a":1}{"b":2}`)))
	if strings.Contains(redacted, `"b":2`) {
		t.Errorf("RedactToolArgs() = %q, want the trailing document dropped", redacted)
	}
}

func TestRedactToolArgsEmptyInput(t *testing.T) {
	t.Parallel()

	if got := RedactToolArgs(nil); len(got) != 0 {
		t.Fatalf("RedactToolArgs(nil) = %q, want empty", got)
	}
}

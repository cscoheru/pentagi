package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"pentagi/pkg/providers/pconfig"
	"pentagi/pkg/providers/provider"
)

// Intent-check tunables. Kept local because callers outside this file should
// not depend on the exact retry behavior — the only thing that matters to them
// is "fail open on transient errors". If the upstream callWithSetupRetries
// (pkg/providers/providers.go:1077) is ever made reusable, swap this for a
// shared helper. TODO: dedupe with pkg/providers/providers.go callWithSetupRetries.
const (
	intentCheckMaxRetries  = 3
	intentCheckRetryDelay  = 500 * time.Millisecond
)

type intentDecision string

const (
	intentDecisionPass    intentDecision = "pass"
	intentDecisionClarify intentDecision = "clarify"
	intentDecisionReject  intentDecision = "reject"
)

type intentResult struct {
	Decision intentDecision `json:"decision"`
	Reason   string         `json:"reason"`
}

// fenceRegex matches a leading ```json (or just ```) line and trailing ```,
// with optional whitespace around them. Used as a permissive fallback when a
// model wraps its JSON reply in markdown fences despite the prompt's "no
// fences" instruction.
var fenceRegex = regexp.MustCompile("(?s)^\\s*```(?:json)?\\s*|\\s*```\\s*$")

// intentCheck runs a pre-flight LLM classification on the user's createFlow
// input. It is fail-open: any error, malformed JSON, or panic returns nil so a
// transient LLM outage never blocks flow creation. Only an explicit
// "clarify" or "reject" decision returns a non-nil error, which the resolver
// propagates so the frontend toast surfaces the LLM-authored reason.
func (r *Resolver) intentCheck(ctx context.Context, prv provider.Provider, renderedPrompt string) error {
	defer func() {
		if rec := recover(); rec != nil {
			r.Logger.WithField("panic", rec).Warn("intent_check_fail_open: recovered from panic in classifier; allowing flow creation")
		}
	}()

	if prv == nil {
		r.Logger.Warn("intent_check_fail_open: nil provider; allowing flow creation")
		return nil
	}

	raw, err := callClassifierLLM(ctx, prv, renderedPrompt)
	if err != nil {
		r.Logger.WithError(err).WithField("input_len", len(renderedPrompt)).
			Warn("intent_check_fail_open: classifier LLM call failed; allowing flow creation")
		return nil
	}

	res, parseErr := parseIntentResult(raw)
	if parseErr != nil {
		r.Logger.WithError(parseErr).WithField("raw_preview", previewForLog(raw)).
			Warn("intent_check_fail_open: classifier returned unparseable JSON; allowing flow creation")
		return nil
	}

	switch res.Decision {
	case intentDecisionPass:
		return nil
	case intentDecisionClarify, intentDecisionReject:
		return fmt.Errorf("input rejected by intent classifier: %s", strings.TrimSpace(res.Reason))
	default:
		r.Logger.WithField("decision", res.Decision).
			Warn("intent_check_fail_open: classifier returned unknown decision; allowing flow creation")
		return nil
	}
}

// callClassifierLLM is a local copy of pkg/providers/providers.go:1077
// callWithSetupRetries — short retry loop around prv.Call. Kept local to avoid
// exporting the helper across packages. TODO: dedupe with the original.
func callClassifierLLM(ctx context.Context, prv provider.Provider, prompt string) (string, error) {
	var (
		result string
		err    error
	)

	for idx := 0; idx <= intentCheckMaxRetries; idx++ {
		if idx == intentCheckMaxRetries {
			return "", fmt.Errorf("failed to call classifier llm after %d retries: %w", idx, err)
		}

		result, err = prv.Call(ctx, pconfig.OptionsTypeSimpleJSON, prompt)
		if err == nil {
			return result, nil
		}

		if errors.Is(err, context.Canceled) {
			return "", err
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(intentCheckRetryDelay):
		}
	}

	return "", err
}

// parseIntentResult tries strict JSON first; on failure, strips markdown
// fences / surrounding whitespace and retries once. Returns an error only if
// both attempts fail — the caller treats that as a fail-open trigger.
func parseIntentResult(raw string) (intentResult, error) {
	var res intentResult

	if err := json.Unmarshal([]byte(raw), &res); err == nil {
		return res, nil
	}

	stripped := strings.TrimSpace(fenceRegex.ReplaceAllString(raw, ""))
	if stripped != raw {
		if err := json.Unmarshal([]byte(stripped), &res); err == nil {
			return res, nil
		}
	}

	return res, fmt.Errorf("strict and permissive JSON parse both failed")
}

// previewForLog trims long classifier responses so warn logs stay scannable.
func previewForLog(s string) string {
	const maxLen = 200
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "...(truncated)"
}

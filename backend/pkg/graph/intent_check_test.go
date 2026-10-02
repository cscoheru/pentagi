package graph

import (
	"context"
	"strings"
	"testing"

	"pentagi/pkg/providers/pconfig"
	"pentagi/pkg/providers/provider"
	"pentagi/pkg/providers/tester/mock"
	"pentagi/pkg/templates"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestResolver() *Resolver {
	return &Resolver{
		Logger:          logrus.NewEntry(logrus.New()),
		DefaultPrompter: templates.NewDefaultPrompter(),
	}
}

func TestIntentCheck(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("pass returns nil", func(t *testing.T) {
		t.Parallel()
		prv := mock.NewProvider(provider.ProviderCustom, "test", "test-model")
		prv.SetDefaultResponse(`{"decision":"pass","reason":"looks like pentest"}`)

		r := newTestResolver()
		err := r.intentCheck(ctx, prv, "ignored prompt")
		require.NoError(t, err)
	})

	t.Run("clarify returns error containing reason", func(t *testing.T) {
		t.Parallel()
		prv := mock.NewProvider(provider.ProviderCustom, "test", "test-model")
		prv.SetDefaultResponse(`{"decision":"clarify","reason":"请补充目标地址"}`)

		r := newTestResolver()
		err := r.intentCheck(ctx, prv, "ignored prompt")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "请补充目标地址")
	})

	t.Run("reject returns error containing reason", func(t *testing.T) {
		t.Parallel()
		prv := mock.NewProvider(provider.ProviderCustom, "test", "test-model")
		prv.SetDefaultResponse(`{"decision":"reject","reason":"请输入具体的渗透测试目标"}`)

		r := newTestResolver()
		err := r.intentCheck(ctx, prv, "ignored prompt")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "请输入具体的渗透测试目标")
	})

	t.Run("markdown-fenced JSON is accepted by permissive parse", func(t *testing.T) {
		t.Parallel()
		prv := mock.NewProvider(provider.ProviderCustom, "test", "test-model")
		prv.SetDefaultResponse("```json\n{\"decision\":\"reject\",\"reason\":\"请输入目标\"}\n```")

		r := newTestResolver()
		err := r.intentCheck(ctx, prv, "ignored prompt")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "请输入目标")
	})

	t.Run("pure prose falls open to nil", func(t *testing.T) {
		t.Parallel()
		prv := mock.NewProvider(provider.ProviderCustom, "test", "test-model")
		prv.SetDefaultResponse("this is not json at all, just prose")

		r := newTestResolver()
		err := r.intentCheck(ctx, prv, "ignored prompt")
		require.NoError(t, err)
	})

	t.Run("provider call error falls open to nil", func(t *testing.T) {
		t.Parallel()
		prv := mock.NewProvider(provider.ProviderCustom, "test", "test-model")
		// Don't set any response: mock returns an error on Call by default.

		r := newTestResolver()
		err := r.intentCheck(ctx, prv, "ignored prompt")
		require.NoError(t, err)
	})

	t.Run("unknown decision value falls open to nil", func(t *testing.T) {
		t.Parallel()
		prv := mock.NewProvider(provider.ProviderCustom, "test", "test-model")
		prv.SetDefaultResponse(`{"decision":"maybe","reason":"unsure"}`)

		r := newTestResolver()
		err := r.intentCheck(ctx, prv, "ignored prompt")
		require.NoError(t, err)
	})

	t.Run("nil provider fails open to nil", func(t *testing.T) {
		t.Parallel()
		r := newTestResolver()
		err := r.intentCheck(ctx, nil, "ignored prompt")
		require.NoError(t, err)
	})
}

func TestParseIntentResult(t *testing.T) {
	t.Parallel()

	t.Run("strict JSON succeeds", func(t *testing.T) {
		t.Parallel()
		res, err := parseIntentResult(`{"decision":"pass","reason":"ok"}`)
		require.NoError(t, err)
		assert.Equal(t, intentDecisionPass, res.Decision)
		assert.Equal(t, "ok", res.Reason)
	})

	t.Run("permissive parse strips fences", func(t *testing.T) {
		t.Parallel()
		res, err := parseIntentResult("```json\n{\"decision\":\"reject\",\"reason\":\"x\"}\n```")
		require.NoError(t, err)
		assert.Equal(t, intentDecisionReject, res.Decision)
	})

	t.Run("pure prose returns error", func(t *testing.T) {
		t.Parallel()
		_, err := parseIntentResult("no json here")
		require.Error(t, err)
		assert.True(t, strings.Contains(err.Error(), "parse both failed"))
	})

	t.Run("empty string returns error", func(t *testing.T) {
		t.Parallel()
		_, err := parseIntentResult("")
		require.Error(t, err)
	})
}

// guard against accidentally calling the wrong options type in the future
func TestIntentCheckUsesSimpleJSONOption(t *testing.T) {
	t.Parallel()
	prv := mock.NewProvider(provider.ProviderCustom, "test", "test-model")
	prv.SetDefaultResponse(`{"decision":"pass","reason":"ok"}`)

	captured := struct {
		opt pconfig.ProviderOptionsType
	}{}

	wrapped := &optCapturingProvider{
		Provider: prv,
		capture:  &captured,
	}

	r := newTestResolver()
	err := r.intentCheck(context.Background(), wrapped, "ignored prompt")
	require.NoError(t, err)
	assert.Equal(t, pconfig.OptionsTypeSimpleJSON, captured.opt)
}

// optCapturingProvider wraps a Provider and remembers the last opt type seen.
type optCapturingProvider struct {
	*mock.Provider
	capture *struct {
		opt pconfig.ProviderOptionsType
	}
}

func (p *optCapturingProvider) Call(ctx context.Context, opt pconfig.ProviderOptionsType, prompt string) (string, error) {
	p.capture.opt = opt
	return p.Provider.Call(ctx, opt, prompt)
}

// TestIntentCheckPromptScope pins the scope decision of the createFlow guard.
//
// The prompt is free text, so no behavioural test can see it: a mocked provider
// decides the verdict, not the wording. Without an assertion on the wording the
// scope can be silently widened or narrowed. Both directions matter — the guard
// must stay able to reject greetings, and research/retrieval work must be
// explicitly in scope (owner decision 2026-10-02).
func TestIntentCheckPromptScope(t *testing.T) {
	t.Parallel()

	rendered, err := templates.NewDefaultPrompter().RenderTemplate(
		templates.PromptTypeIntentCheck,
		map[string]any{"Input": "研究荷香是如何产生的并写一份报告"},
	)
	require.NoError(t, err)

	t.Run("research and retrieval work is in scope", func(t *testing.T) {
		for _, want := range []string{
			"open-source research",
			"web/literature search",
			"article collection",
			"research topic",
		} {
			assert.Contains(t, rendered, want)
		}
	})

	t.Run("guard still rejects greetings and small talk", func(t *testing.T) {
		assert.Contains(t, rendered, "greeting, small talk")
		assert.Contains(t, rendered, `"reject"`)
		// A research request must not be classified as something to redirect
		// away from: the reject branch may only ask for a concrete subject.
		assert.NotContains(t, rendered, "concrete pentest target")
	})

	t.Run("user input is rendered verbatim", func(t *testing.T) {
		assert.Contains(t, rendered, "研究荷香是如何产生的并写一份报告")
	})
}

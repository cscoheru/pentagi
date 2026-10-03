package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"pentagi/pkg/providers/pconfig"
	"pentagi/pkg/providers/provider"
	"pentagi/pkg/tools"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vxcontrol/langchaingo/llms"
	"github.com/vxcontrol/langchaingo/llms/streaming"
)

func TestSynthesizeDoneCallUsesFinalizeTool(t *testing.T) {
	t.Parallel()

	call := synthesizeDoneCall("lotus fragrance comes from methoxybenzenes")

	require.NotNil(t, call.FunctionCall)
	assert.Equal(t, tools.FinalyToolName, call.FunctionCall.Name)
	assert.Equal(t, "function", call.Type)
	assert.NotEmpty(t, call.ID)
}

func TestSynthesizeDoneCallCarriesContent(t *testing.T) {
	t.Parallel()

	content := "渥堆发酵中的微生物分解大分子物质，产生甲氧基苯化合物。"
	call := synthesizeDoneCall(content)

	var done tools.Done
	require.NoError(t, json.Unmarshal([]byte(call.FunctionCall.Arguments), &done))
	assert.Equal(t, content, done.Result)
	assert.NotEmpty(t, done.Message)
}

// The model never declared the objective reached, so the framework must not
// report the forced stop as a success. A false success in a pentest run is an
// all-clear the evidence never gave.
func TestSynthesizeDoneCallDoesNotClaimSuccess(t *testing.T) {
	t.Parallel()

	for _, content := range []string{"findings", ""} {
		call := synthesizeDoneCall(content)

		var done tools.Done
		require.NoError(t, json.Unmarshal([]byte(call.FunctionCall.Arguments), &done))
		assert.False(t, bool(done.Success), "content %q", content)
	}
}

// The id has to be namespaced so a synthesized call cannot be mistaken for one
// the model issued when the chain is replayed or audited later.
func TestSynthesizeDoneCallIDIsFrameworkNamespaced(t *testing.T) {
	t.Parallel()

	call := synthesizeDoneCall("findings")

	assert.True(t, strings.HasPrefix(call.ID, "framework-done-"), call.ID)
}

func TestSynthesizeDoneCallAcceptsEmptyContent(t *testing.T) {
	t.Parallel()

	call := synthesizeDoneCall("")

	var done tools.Done
	require.NoError(t, json.Unmarshal([]byte(call.FunctionCall.Arguments), &done))
	assert.Empty(t, done.Result)
}

// The barrier handler stores the result as the subtask outcome, so the argument
// has to unmarshal into tools.Done even for content with quotes and newlines.
func TestSynthesizeDoneCallRoundTripsAwkwardContent(t *testing.T) {
	t.Parallel()

	content := "line one\n\"quoted\" and \\backslash\\ \t and 'single'\n\nline two"
	call := synthesizeDoneCall(content)

	var done tools.Done
	require.NoError(t, json.Unmarshal([]byte(call.FunctionCall.Arguments), &done))
	assert.Equal(t, content, done.Result)
}

// The finalized chain must keep the text it collected: losing it would recreate
// the bug where knowledge existed only inside an unbounded reflector loop.
func TestSynthesizeDoneCallKeepsCollectedText(t *testing.T) {
	t.Parallel()

	parts := []string{"first finding", "second finding", "third finding"}
	call := synthesizeDoneCall(strings.Join(parts, "\n\n"))

	var done tools.Done
	require.NoError(t, json.Unmarshal([]byte(call.FunctionCall.Arguments), &done))
	for _, part := range parts {
		assert.Contains(t, done.Result, part)
	}
}

// ---------------------------------------------------------------------------
// chainFailureTerminal / callWithRetries — budget, cancel and timeout must not
// be retried as if they were transient provider errors.
// ---------------------------------------------------------------------------

// timeoutErrorOnly mimics net/http's internal *httpError: it reports Timeout()
// but deliberately does not satisfy errors.Is(err, context.DeadlineExceeded).
// That is exactly how an http.Client.Timeout (LLM_CLIENT_TIMEOUT) failure
// arrives, so classifying timeouts off the sentinel alone would keep retrying a
// provider that just spent the call budget.
type timeoutErrorOnly struct{ msg string }

func (e *timeoutErrorOnly) Error() string { return e.msg }
func (e *timeoutErrorOnly) Timeout() bool { return true }

func TestChainFailureTerminalClassifiesStopReasons(t *testing.T) {
	t.Parallel()

	// An already-expired framework budget is authoritative regardless of what
	// the provider reported underneath it.
	expired, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelDeadline()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	clientTimeout := &url.Error{
		Op:  "Post",
		URL: "https://llm.example/v1/chat/completions",
		Err: &timeoutErrorOnly{msg: "context deadline exceeded (Client.Timeout exceeded while awaiting headers)"},
	}

	tests := []struct {
		name         string
		ctx          context.Context
		err          error
		wantTerminal bool
		wantReason   string
	}{
		{
			name:         "framework budget already spent",
			ctx:          expired,
			err:          errors.New("upstream call cut short by a generic io failure"),
			wantTerminal: true,
			wantReason:   "budget_exhausted",
		},
		{
			name:         "external cancel is not an execution failure",
			ctx:          canceled,
			err:          errors.New("upstream call cut short by a generic io failure"),
			wantTerminal: true,
			wantReason:   "canceled",
		},
		{
			name:         "provider deadline with a live run context",
			ctx:          context.Background(),
			err:          errors.Join(errors.New("llm call"), context.DeadlineExceeded),
			wantTerminal: true,
			wantReason:   "provider_deadline",
		},
		{
			name:         "provider cancel with a live run context",
			ctx:          context.Background(),
			err:          errors.Join(errors.New("llm call"), context.Canceled),
			wantTerminal: true,
			wantReason:   "canceled",
		},
		{
			name:         "client timeout with a live run context",
			ctx:          context.Background(),
			err:          clientTimeout,
			wantTerminal: true,
			wantReason:   "provider_timeout",
		},
		{
			name:         "transient gateway error must stay retryable",
			ctx:          context.Background(),
			err:          errors.New("API returned unexpected status code: 502: bad gateway"),
			wantTerminal: false,
		},
		{
			name:         "malformed model output must stay retryable",
			ctx:          context.Background(),
			err:          errors.New("no content and tool calls in response: stop reason 'length'"),
			wantTerminal: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			terminal, reason := chainFailureTerminal(tt.ctx, tt.err)
			assert.Equal(t, tt.wantTerminal, terminal)
			assert.Equal(t, tt.wantReason, reason)
		})
	}
}

// fakeChainCallProvider overrides only CallWithTools; embedding
// provider.Provider means every other interface method is unimplemented and
// would panic on use, which is fine since callWithRetries only reaches
// CallWithTools here — the reflector path is entered only after the full retry
// budget, which none of these tests exhaust.
type fakeChainCallProvider struct {
	provider.Provider
	callCount int
	failTimes int
	err       error
}

func (f *fakeChainCallProvider) Type() provider.ProviderType { return provider.ProviderOpenAI }

func (f *fakeChainCallProvider) CallWithTools(
	ctx context.Context,
	_ pconfig.ProviderOptionsType,
	_ []llms.MessageContent,
	_ []llms.Tool,
	_ streaming.Callback,
) (*llms.ContentResponse, error) {
	f.callCount++
	if f.callCount <= f.failTimes {
		return nil, f.err
	}
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: "recovered"}}}, nil
}

// stubChainExecutor satisfies just what callWithRetries touches: Tools() is the
// only method these tests reach.
type stubChainExecutor struct {
	tools.ContextToolsExecutor
}

func (stubChainExecutor) Tools() []llms.Tool { return nil }

func newCallWithRetriesProvider(err error, failTimes int) (*flowProvider, *fakeChainCallProvider) {
	prv := &fakeChainCallProvider{err: err, failTimes: failTimes}
	fp := newFlowProvider()
	fp.Provider = prv
	return fp, prv
}

// A spent budget is already gone: a second full chain run cannot buy it back,
// it only delays the failure and buries the real stop reason behind a generic
// retry error.
func TestCallWithRetriesBudgetExhaustedStopsWithoutSecondChainRun(t *testing.T) {
	t.Parallel()

	fp, prv := newCallWithRetriesProvider(context.DeadlineExceeded, maxRetriesToCallAgentChain+1)

	expired, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelDeadline()

	_, err := fp.callWithRetries(
		expired, pconfig.OptionsTypePrimaryAgent, 1, nil, nil, nil, stubChainExecutor{}, "execution context",
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded, "upstream must still be able to tell a budget stop apart")
	assert.Equal(t, 1, prv.callCount, "retrying a spent budget only spends more of a budget already gone")
	assert.Contains(t, err.Error(), "budget_exhausted")
	assert.NotContains(t, err.Error(), "while waiting for retry",
		"a budget stop must not read like an interrupted backoff")
}

// An external cancel is not an execution failure (audit §2), so it must both
// stop retrying and stay discoverable as a cancellation downstream — even when
// the underlying call error says nothing about the cancel.
func TestCallWithRetriesExternalCancelIsDiscoverableDownstream(t *testing.T) {
	t.Parallel()

	fp, prv := newCallWithRetriesProvider(errors.New("connection reset by peer"), maxRetriesToCallAgentChain+1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fp.callWithRetries(
		ctx, pconfig.OptionsTypePrimaryAgent, 1, nil, nil, nil, stubChainExecutor{}, "execution context",
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled, "a cancel must not be recorded as an ordinary failure")
	assert.Equal(t, 1, prv.callCount, "nothing can be retried once the run context is gone")
	assert.Contains(t, err.Error(), "agent chain aborted (canceled)")
}

// The http.Client.Timeout path arrives with a *live* run context, so it would
// otherwise burn all maxRetriesToCallAgentChain full chain runs at up to
// LLM_CLIENT_TIMEOUT each — the wasteful shape this fix exists to remove.
func TestCallWithRetriesClientTimeoutNotRetriedWhileContextIsLive(t *testing.T) {
	t.Parallel()

	clientTimeout := &url.Error{
		Op:  "Post",
		URL: "https://llm.example/v1/chat/completions",
		Err: &timeoutErrorOnly{msg: "Client.Timeout exceeded while awaiting headers"},
	}
	fp, prv := newCallWithRetriesProvider(clientTimeout, maxRetriesToCallAgentChain+1)

	start := time.Now()
	_, err := fp.callWithRetries(
		context.Background(), pconfig.OptionsTypePrimaryAgent, 1, nil, nil, nil, stubChainExecutor{}, "execution context",
	)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "provider_timeout")
	assert.Equal(t, 1, prv.callCount, "a call that just spent the LLM budget must not be run again")
	assert.Less(t, elapsed, delayBetweenRetries,
		"it must return at once instead of sitting in the retry backoff")
}

// Regression guard: real transient failures are what the retry loop exists for,
// so the terminal short-circuit must not swallow them. This pays one real
// delayBetweenRetries wait, the same way TestCallWithSetupRetries_
// RetriesOnTransientErrorThenSucceeds already does in this package.
func TestCallWithRetriesTransientErrorStillRetries(t *testing.T) {
	t.Parallel()

	fp, prv := newCallWithRetriesProvider(
		errors.New("API returned unexpected status code: 502: bad gateway"), 1,
	)

	result, err := fp.callWithRetries(
		context.Background(), pconfig.OptionsTypePrimaryAgent, 1, nil, nil, nil, stubChainExecutor{}, "execution context",
	)

	require.NoError(t, err)
	assert.Equal(t, 2, prv.callCount, "one transient failure must still trigger exactly one retry")
	require.NotNil(t, result)
	assert.Equal(t, "recovered", result.content)
}

// The backoff-wait abort branch is only reachable when the failure itself was
// transient and the cancel arrived during the 5s wait — every other test
// either pre-cancels (terminal check fires first, never entering the select)
// or uses a live Background ctx (ctx.Done never fires). Pin both message
// variants and the %w wrap so the wait path stays distinguishable downstream.
func TestCallWithRetriesCancelDuringBackoffWait(t *testing.T) {
	t.Parallel()

	fp, prv := newCallWithRetriesProvider(
		errors.New("API returned unexpected status code: 502: bad gateway"), 10,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := fp.callWithRetries(
		ctx, pconfig.OptionsTypePrimaryAgent, 1, nil, nil, nil, stubChainExecutor{}, "execution context",
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled, "a cancel during the wait must stay discoverable")
	assert.Contains(t, err.Error(), "retry wait aborted (canceled)")
	assert.NotContains(t, err.Error(), "agent chain aborted",
		"the wait path is a different stop than the terminal short-circuit")
	assert.Equal(t, 1, prv.callCount, "the transient failure must reach the wait, not a second call")
}

// Symmetric deadline variant: the run context dies while waiting out the
// backoff, and the message must carry the canonical budget_exhausted token —
// the same vocabulary as the terminal path — not a prose variant nobody greps.
func TestCallWithRetriesDeadlineDuringBackoffWaitEmitsBudgetToken(t *testing.T) {
	t.Parallel()

	fp, prv := newCallWithRetriesProvider(
		errors.New("API returned unexpected status code: 502: bad gateway"), 10,
	)

	// Generous margin: the stub call returns in microseconds, so the deadline
	// must land inside the 5s wait, not during the (already classified) call.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(250*time.Millisecond))
	defer cancel()

	_, err := fp.callWithRetries(
		ctx, pconfig.OptionsTypePrimaryAgent, 1, nil, nil, nil, stubChainExecutor{}, "execution context",
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "retry wait aborted (budget_exhausted)",
		"the wait path must emit the same stop_reason token as the terminal path")
	assert.Equal(t, 1, prv.callCount)
}

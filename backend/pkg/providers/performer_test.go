package providers

import (
	"encoding/json"
	"strings"
	"testing"

	"pentagi/pkg/tools"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

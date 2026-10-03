package providers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"pentagi/pkg/database"
	"pentagi/pkg/providers/pconfig"
	"pentagi/pkg/providers/provider"
	"pentagi/pkg/schema"
	"pentagi/pkg/tools"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vxcontrol/langchaingo/llms"
	"github.com/vxcontrol/langchaingo/llms/streaming"
)

// ---------------------------------------------------------------------------
// Stubs for reaching performSearcher itself. The chain it drives is real —
// performAgentChain, callWithRetries, restoreChain all run untouched — so the
// only things faked are the database rows and the model, which is exactly what
// a unit test must replace to observe this behaviour deterministically.
// ---------------------------------------------------------------------------

// searcherTestDB answers the task/subtask lookups the pre-chain setup needs,
// recreates the msg chain a live DB would, and serves the search logs that are
// B's actual recovery source.
type searcherTestDB struct {
	database.Querier

	taskID      int64
	logs        []database.Searchlog
	taskLogs    []database.Searchlog
	logsErr     error
	subtaskHits int
	taskHits    int
	readCtxErr  error
}

func (db *searcherTestDB) GetTask(_ context.Context, id int64) (database.Task, error) {
	return database.Task{ID: id, FlowID: 1}, nil
}

func (db *searcherTestDB) GetSubtask(_ context.Context, id int64) (database.Subtask, error) {
	// Context non-empty and TaskID matching short-circuits
	// getExecutionContextBySubtask, so no prompter is needed for these tests.
	return database.Subtask{ID: id, TaskID: db.taskID, Context: "the subtask's stored execution context"}, nil
}

func (db *searcherTestDB) GetSubtaskSearchLogs(ctx context.Context, _ sql.NullInt64) ([]database.Searchlog, error) {
	db.subtaskHits++
	db.readCtxErr = ctx.Err()
	return db.logs, db.logsErr
}

func (db *searcherTestDB) GetTaskSearchLogs(ctx context.Context, _ sql.NullInt64) ([]database.Searchlog, error) {
	db.taskHits++
	db.readCtxErr = ctx.Err()
	return db.taskLogs, db.logsErr
}

func (db *searcherTestDB) GetFlowTaskTypeLastMsgChain(
	context.Context, database.GetFlowTaskTypeLastMsgChainParams,
) (database.Msgchain, error) {
	return database.Msgchain{}, errors.New("no restored chain in this test db")
}

func (db *searcherTestDB) CreateMsgChain(context.Context, database.CreateMsgChainParams) (database.Msgchain, error) {
	return database.Msgchain{ID: 7}, nil
}

func (db *searcherTestDB) UpdateMsgChain(context.Context, database.UpdateMsgChainParams) (database.Msgchain, error) {
	return database.Msgchain{ID: 7}, nil
}

func (db *searcherTestDB) UpdateMsgChainUsage(context.Context, database.UpdateMsgChainUsageParams) (database.Msgchain, error) {
	return database.Msgchain{ID: 7}, nil
}

// searcherTestProvider scripts the model: either every call fails (or every
// call after failAfterCall), or each call replays the next scripted response.
type searcherTestProvider struct {
	provider.Provider

	failWith      error
	failAfterCall int
	responses     []*llms.ContentResponse
	callCount     int
}

func (p *searcherTestProvider) Type() provider.ProviderType { return provider.ProviderOpenAI }

func (p *searcherTestProvider) Model(pconfig.ProviderOptionsType) string {
	return "searcher-test-model"
}

// GetUsage returns a zero usage so updateMsgChainUsage short-circuits before it
// would touch price calculation, which these tests have no reason to model.
func (p *searcherTestProvider) GetUsage(map[string]any) pconfig.CallUsage { return pconfig.CallUsage{} }

func (p *searcherTestProvider) CallWithTools(
	_ context.Context,
	_ pconfig.ProviderOptionsType,
	_ []llms.MessageContent,
	_ []llms.Tool,
	_ streaming.Callback,
) (*llms.ContentResponse, error) {
	p.callCount++
	if p.failWith != nil && (p.failAfterCall == 0 || p.callCount > p.failAfterCall) {
		return nil, p.failWith
	}
	if p.callCount > len(p.responses) {
		return nil, errors.New("scripted model responses exhausted")
	}
	return p.responses[p.callCount-1], nil
}

// searcherTestFlowExecutor hands out the searcher context executor; the
// search_result callback lives on that context executor's own cfg copy.
type searcherTestFlowExecutor struct {
	tools.FlowToolsExecutor
}

func (e *searcherTestFlowExecutor) GetSearcherExecutor(
	cfg tools.SearcherExecutorConfig,
) (tools.ContextToolsExecutor, error) {
	return &searcherTestContextExecutor{cfg: cfg}, nil
}

type searcherTestContextExecutor struct {
	tools.ContextToolsExecutor
	cfg tools.SearcherExecutorConfig
}

func (e *searcherTestContextExecutor) Tools() []llms.Tool           { return nil }
func (e *searcherTestContextExecutor) IsFunctionExists(string) bool { return false }
func (e *searcherTestContextExecutor) IsBarrierFunction(name string) bool {
	return name == tools.FinalyToolName
}
func (e *searcherTestContextExecutor) GetBarrierToolNames() []string {
	return []string{tools.FinalyToolName}
}
func (e *searcherTestContextExecutor) GetBarrierTools() []tools.FunctionInfo { return nil }

func (e *searcherTestContextExecutor) GetToolSchema(string) (*schema.Schema, error) {
	return nil, errors.New("schema lookup is only reached after a tool execution failure")
}

func (e *searcherTestContextExecutor) Execute(
	ctx context.Context, _ int64, _, name, _, _ string, args json.RawMessage,
) (string, error) {
	switch name {
	case "search_result":
		// The agent submitting its result is what fills performSearcher's
		// searchResult — the only path by which B's success branch has text.
		return e.cfg.SearchResult(ctx, "search_result", args)
	case tools.FinalyToolName:
		return "search chain finished", nil
	}
	return "", fmt.Errorf("unexpected tool call %q", name)
}

func newSearcherTestProvider(
	prv *searcherTestProvider, db *searcherTestDB,
) *flowProvider {
	fp := newFlowProvider()
	fp.Provider = prv
	fp.db = db
	fp.executor = &searcherTestFlowExecutor{}
	return fp
}

func searcherToolCallResponse(name, args string) *llms.ContentResponse {
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{
		ToolCalls: []llms.ToolCall{{
			ID:           "call-scripted-1",
			Type:         "function",
			FunctionCall: &llms.FunctionCall{Name: name, Arguments: args},
		}},
	}}}
}

// ---------------------------------------------------------------------------
// performSearcher — entry-level behaviour (audit §3.3: cover the run loop, not
// only the helpers it calls).
// ---------------------------------------------------------------------------

// The chain is the thing that died; the material it already wrote to the
// searchlogs table is not dead with it. Dropping it — the pre-fix behaviour —
// throws away every engine result the subtask budget was able to buy.
func TestPerformSearcherChainDeathReturnsRecoveredSearchLogs(t *testing.T) {
	t.Parallel()

	taskID, subtaskID := int64(11), int64(21)
	db := &searcherTestDB{
		taskID: taskID,
		logs: []database.Searchlog{
			{
				Engine: database.SearchengineTypeSearxng,
				Query:  "荷香 普洱 香型 来源",
				Result: "methoxybenzenes are produced during pile fermentation",
			},
			{
				Engine: database.SearchengineTypeTavily,
				Query:  "pu-erh lotus fragrance scenting process",
				Result: "lotus petals are layered with tea leaves during scenting",
			},
		},
	}
	prv := &searcherTestProvider{failWith: errors.New("search upstream refused the request")}
	fp := newSearcherTestProvider(prv, db)

	// A dead run context is what actually kills these chains in production: the
	// subtask budget expires mid-search. Every stub here ignores ctx so the
	// setup steps still complete and only the model call sees the cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	text, err := fp.performSearcher(
		ctx, &taskID, &subtaskID, "system template", "user template", "荷香普洱的香从哪来",
	)

	require.NoError(t, err, "a dead chain must hand the caller material, not an error")
	require.NotEmpty(t, text)
	assert.Contains(t, text, "[INCOMPLETE SEARCH]", "the incompleteness must be stated up front")
	assert.Contains(t, text, "stop reason: canceled",
		"the stop reason must survive into the banner an operator reads")
	assert.Contains(t, text, "荷香 普洱 香型 来源")
	assert.Contains(t, text, "pu-erh lotus fragrance scenting process")
	assert.Contains(t, text, "verbatim external search output",
		"recovered rows must be declared as quoted data, not instructions")
	assert.Contains(t, text, "do not present it as complete coverage",
		"recovered material must not read as a finished search")
	assert.Equal(t, 1, prv.callCount, "a spent run context must not buy a second chain attempt")
	assert.Equal(t, 1, db.subtaskHits, "exactly one recovery read, scoped to this subtask")
	assert.NoError(t, db.readCtxErr,
		"the recovery read must not inherit the context that killed the chain")
}

// The success branch must stay byte-identical to what the searcher submitted:
// any leak of the incomplete banner into a finished search would be as wrong as
// dropping a dead one.
func TestPerformSearcherSuccessReturnsSubmittedResultUnchanged(t *testing.T) {
	t.Parallel()

	taskID, subtaskID := int64(12), int64(22)
	db := &searcherTestDB{
		taskID: taskID,
		logs: []database.Searchlog{
			{Engine: database.SearchengineTypeSearxng, Query: "leftover from an earlier attempt", Result: "stale"},
		},
	}
	prv := &searcherTestProvider{responses: []*llms.ContentResponse{
		searcherToolCallResponse("search_result",
			`{"result":"completed search result text","message":"searching complete"}`),
		searcherToolCallResponse("done",
			`{"result":"completed search result text","message":"searching complete"}`),
	}}
	fp := newSearcherTestProvider(prv, db)

	text, err := fp.performSearcher(
		context.Background(), &taskID, &subtaskID, "system template", "user template", "question",
	)

	require.NoError(t, err)
	assert.Equal(t, "completed search result text", text)
	assert.NotContains(t, text, "[INCOMPLETE SEARCH]",
		"a finished search must not carry the incomplete-search marker")
	assert.Equal(t, 0, db.subtaskHits,
		"a successful chain must not go looking for recovery material")
}

// The chain-death entry test above always dies on call 1, so searchResult is
// still zero there — it never exercises the call site forwarding an already
// submitted summary. That forwarding is the exact helper-correct /
// wiring-broken pairing behind the 2026-10-01 incident: every
// recoverPartialSearch test would stay green while the entry point dropped it.
func TestPerformSearcherDeathAfterSubmittedSummaryKeepsIt(t *testing.T) {
	t.Parallel()

	taskID, subtaskID := int64(13), int64(23)
	db := &searcherTestDB{
		taskID: taskID,
		logs: []database.Searchlog{
			{Engine: database.SearchengineTypeTavily, Query: "late log", Result: "late result"},
		},
	}
	// Call 1 succeeds and submits search_result (filling searchResult); call 2
	// dies terminally — context.Canceled is terminal even with a live run ctx.
	prv := &searcherTestProvider{
		failWith:      context.Canceled,
		failAfterCall: 1,
		responses: []*llms.ContentResponse{
			searcherToolCallResponse("search_result",
				`{"result":"summary submitted before death","message":"searching"}`),
		},
	}
	fp := newSearcherTestProvider(prv, db)

	text, err := fp.performSearcher(
		context.Background(), &taskID, &subtaskID, "system template", "user template", "question",
	)

	require.NoError(t, err)
	assert.Contains(t, text, "[INCOMPLETE SEARCH]")
	assert.Contains(t, text, "summary submitted before death",
		"the call site must forward searchResult.Result, not an empty string")
	assert.Contains(t, text, "late log")
}

// ---------------------------------------------------------------------------
// recoverPartialSearch — data-path coverage.
// ---------------------------------------------------------------------------

func newRecoveryProvider(db *searcherTestDB) *flowProvider {
	fp := newFlowProvider()
	fp.db = db
	return fp
}

// The submitted summary is the searcher's own final synthesis; when it exists it
// is the most intelligible thing recovered, so it must precede raw log rows.
func TestRecoverPartialSearchKeepsSubmittedSummaryAheadOfLogs(t *testing.T) {
	t.Parallel()

	taskID, subtaskID := int64(31), int64(41)
	db := &searcherTestDB{logs: []database.Searchlog{
		{Engine: database.SearchengineTypeTavily, Query: "lotus scenting", Result: "petals layered with leaves"},
	}}
	fp := newRecoveryProvider(db)

	text := fp.recoverPartialSearch(
		context.Background(), &taskID, &subtaskID, "the question",
		"summary the searcher submitted", errors.New("agent chain aborted (budget_exhausted): deadline"),
	)

	require.NotEmpty(t, text)
	assert.Contains(t, text, "[INCOMPLETE SEARCH]")
	assert.Contains(t, text, "summary the searcher submitted")
	assert.Contains(t, text, "lotus scenting")
	assert.Less(t,
		strings.Index(text, "summary the searcher submitted"),
		strings.Index(text, "lotus scenting"),
		"the searcher's own synthesis reads better than raw rows, so it goes first")
}

// Silence would be read as "there is nothing out there" — a claim this run never
// established. An empty recovery must still explain itself.
func TestRecoverPartialSearchWhenNothingWasCollectedStillSpeaks(t *testing.T) {
	t.Parallel()

	taskID, subtaskID := int64(32), int64(42)
	fp := newRecoveryProvider(&searcherTestDB{})

	text := fp.recoverPartialSearch(
		context.Background(), &taskID, &subtaskID, "the question", "",
		errors.New("agent chain aborted (canceled): context canceled"),
	)

	require.NotEmpty(t, text, "an empty string would surface as an empty tool result")
	assert.Contains(t, text, "[INCOMPLETE SEARCH]")
	assert.Contains(t, text, "terminated before it recorded any query or summary")
	assert.Contains(t, text, "says nothing about whether sources exist")
}

// A failed recovery read must not become a failed search: the banner and the
// stop reason are still worth returning on their own. The banner must state the
// failure without echoing the driver's raw error text — that text can embed DSN
// fragments or host names, and the banner travels into the agent log the LLM
// re-reads. An expired parent context mirrors production (recovery runs after
// the budget that killed the chain) and is what supplies the stop reason.
func TestRecoverPartialSearchSurvivesSearchLogReadError(t *testing.T) {
	t.Parallel()

	taskID, subtaskID := int64(33), int64(43)
	fp := newRecoveryProvider(&searcherTestDB{logsErr: errors.New("connection refused to db.internal:5432")})

	expired, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelDeadline()

	text := fp.recoverPartialSearch(
		expired, &taskID, &subtaskID, "the question", "",
		errors.New("agent chain aborted (budget_exhausted): deadline exceeded"),
	)

	require.NotEmpty(t, text)
	assert.Contains(t, text, "[INCOMPLETE SEARCH]")
	assert.Contains(t, text, "budget_exhausted")
	assert.Contains(t, text, "recovered from searchlogs: read failed")
	assert.NotContains(t, text, "db.internal",
		"raw driver error detail must stay server-side, never in the banner")
}

// Binding invariant for context.WithoutCancel: the context that killed the chain
// is already dead, so a recovery read performed with it would fail on the spot
// and return nothing — the exact outcome this helper exists to prevent.
// It also pins the task-level scope used by the task-level searcher, which
// passes no subtaskID.
func TestRecoverPartialSearchReadsWithoutCancelWhenParentContextIsDead(t *testing.T) {
	t.Parallel()

	taskID, subtaskID := int64(34), int64(44)
	db := &searcherTestDB{
		logs:     []database.Searchlog{{Engine: database.SearchengineTypeSearxng, Query: "subtask query", Result: "r"}},
		taskLogs: []database.Searchlog{{Engine: database.SearchengineTypeTavily, Query: "task query", Result: "r"}},
	}
	fp := newRecoveryProvider(db)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	subtaskText := fp.recoverPartialSearch(
		ctx, &taskID, &subtaskID, "the question", "",
		errors.New("agent chain aborted (budget_exhausted): deadline exceeded"),
	)
	assert.NoError(t, db.readCtxErr,
		"the subtask-scoped read must not inherit the context that killed the chain")
	assert.Equal(t, 1, db.subtaskHits)
	assert.Contains(t, subtaskText, "subtask query")

	taskText := fp.recoverPartialSearch(
		ctx, &taskID, nil, "the question", "",
		errors.New("agent chain aborted (budget_exhausted): deadline exceeded"),
	)
	assert.NoError(t, db.readCtxErr,
		"the task-scoped read must not inherit the context that killed the chain either")
	assert.Equal(t, 1, db.taskHits,
		"the task-level searcher passes no subtaskID, so it must fall back to task logs")
	assert.Equal(t, 1, db.subtaskHits, "task scope must not fall through to subtask logs")
	assert.Contains(t, taskText, "task query")
}

// Audit §3.3 requires upper-limit boundary coverage. Nothing else in the suite
// produces text near partialSearchResultLimit, so a dropped budget decrement or
// an inverted break condition would pass every other test. This drives BOTH
// clipping paths: the per-entry truncation (40 x 4KB entries overflow the
// budget mid-loop) and clipToPartialSearchLimit (the 70K-rune question makes
// the header alone exceed the limit, so the final cut lands mid-rune).
func TestRecoverPartialSearchTruncatesToLimitRuneSafe(t *testing.T) {
	t.Parallel()

	taskID, subtaskID := int64(51), int64(61)
	logs := make([]database.Searchlog, 40)
	for i := range logs {
		logs[i] = database.Searchlog{
			Engine: database.SearchengineTypeSearxng,
			Query:  "q",
			Result: strings.Repeat("x", 4096),
		}
	}
	fp := newRecoveryProvider(&searcherTestDB{logs: logs})

	text := fp.recoverPartialSearch(
		context.Background(), &taskID, &subtaskID,
		"荷"+strings.Repeat("普", 70000), "",
		errors.New("chain aborted (budget_exhausted): deadline exceeded"),
	)

	assert.LessOrEqual(t, len(text), partialSearchResultLimit+64,
		"only the clip notice itself may push past the limit")
	assert.True(t, utf8.ValidString(text), "the clip must never land mid-rune")
	assert.Contains(t, text, "truncated", "hitting the limit must announce the truncation")
}

// The truncation notice's byte figure must count everything the loop actually
// discards — every remaining entry in full, since the loop never writes a
// partial entry. The old arithmetic reported only the first dropped entry's
// overflow past the leftover budget (~15KB here) while ~160KB was really gone,
// a notice that contradicts itself in front of the operator.
func TestRecoverPartialSearchTruncationNoticeCountsAllDroppedBytes(t *testing.T) {
	t.Parallel()

	taskID, subtaskID := int64(52), int64(62)
	logs := make([]database.Searchlog, 5)
	for i := range logs {
		logs[i] = database.Searchlog{
			Engine: database.SearchengineTypeSearxng,
			Query:  "q",
			Result: strings.Repeat("x", 40000),
		}
	}
	fp := newRecoveryProvider(&searcherTestDB{logs: logs})

	text := fp.recoverPartialSearch(
		context.Background(), &taskID, &subtaskID, "short question", "",
		errors.New("chain aborted (budget_exhausted): deadline exceeded"),
	)

	// Header + first entry fit (~40KB each), the second overflows: entries 2-5
	// are all discarded, so the notice must report 4 dropped, not 1.
	assert.Contains(t, text, "truncated 4 of 5 search logs",
		"the count must include every entry behind the first overflow")
	m := regexp.MustCompile(`truncated 4 of 5 search logs \((\d+) bytes\)`).FindStringSubmatch(text)
	require.NotNil(t, m, "the notice must state a byte figure: %s", text)
	dropped, err := strconv.Atoi(m[1])
	require.NoError(t, err)
	assert.Greater(t, dropped, 100_000,
		"four full 40KB entries were discarded; a figure near the first entry's overflow is the old undercount")
}

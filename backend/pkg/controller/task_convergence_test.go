package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"pentagi/pkg/database"
	"pentagi/pkg/graph/subscriptions"
	"pentagi/pkg/providers"
	"pentagi/pkg/tools"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSubtask is a minimal SubtaskWorker that records the status it was given.
type fakeSubtask struct {
	id            int64
	status        database.SubtaskStatus
	statusErr     error
	runErr        error
	blockUntilEnd bool
	onRun         func()
	setStatuses   []database.SubtaskStatus
	// setStatusErrAtCall is the context error observed while SetStatus ran, not
	// afterwards: the caller cancels its write context on return, so reading the
	// context later would always look dead.
	setStatusErrAtCall   []error
	setStatusHasDeadline []bool
}

func (f *fakeSubtask) GetMsgChainID() int64                      { return 0 }
func (f *fakeSubtask) GetSubtaskID() int64                       { return f.id }
func (f *fakeSubtask) GetTaskID() int64                          { return 0 }
func (f *fakeSubtask) GetFlowID() int64                          { return 0 }
func (f *fakeSubtask) GetUserID() int64                          { return 0 }
func (f *fakeSubtask) GetTitle() string                          { return "fake" }
func (f *fakeSubtask) GetDescription() string                    { return "fake" }
func (f *fakeSubtask) IsCompleted() bool                         { return false }
func (f *fakeSubtask) IsWaiting() bool                           { return false }
func (f *fakeSubtask) GetResult(context.Context) (string, error) { return "", nil }
func (f *fakeSubtask) SetResult(context.Context, string) error   { return nil }
func (f *fakeSubtask) PutInput(context.Context, string) error    { return nil }
func (f *fakeSubtask) Finish(context.Context) error              { return nil }

func (f *fakeSubtask) GetStatus(context.Context) (database.SubtaskStatus, error) {
	return f.status, f.statusErr
}

func (f *fakeSubtask) SetStatus(ctx context.Context, status database.SubtaskStatus) error {
	f.setStatusErrAtCall = append(f.setStatusErrAtCall, ctx.Err())
	_, hasDeadline := ctx.Deadline()
	f.setStatusHasDeadline = append(f.setStatusHasDeadline, hasDeadline)
	f.setStatuses = append(f.setStatuses, status)
	f.status = status
	return nil
}

func (f *fakeSubtask) Run(ctx context.Context) error {
	if f.onRun != nil {
		f.onRun()
	}
	if f.blockUntilEnd {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.runErr
}

// ---- run-level fakes --------------------------------------------------------

// runFakeQuerier answers only the task-row calls the run makes. Anything else
// panics through the embedded nil interface, flagging a gap instead of silently
// returning zero values.
type runFakeQuerier struct {
	database.Querier

	statuses   []database.TaskStatus
	results    []string
	planned    []database.Subtask
	statusErr  error
	resultErr  error
	plannedErr error
}

func (q *runFakeQuerier) UpdateTaskStatus(
	_ context.Context, arg database.UpdateTaskStatusParams,
) (database.Task, error) {
	if q.statusErr != nil {
		return database.Task{}, q.statusErr
	}
	q.statuses = append(q.statuses, arg.Status)
	return database.Task{ID: arg.ID, Status: arg.Status}, nil
}

func (q *runFakeQuerier) GetTaskSubtasks(context.Context, int64) ([]database.Subtask, error) {
	return nil, nil
}

func (q *runFakeQuerier) UpdateTaskResult(
	_ context.Context, arg database.UpdateTaskResultParams,
) (database.Task, error) {
	if q.resultErr != nil {
		return database.Task{}, q.resultErr
	}
	q.results = append(q.results, arg.Result)
	return database.Task{ID: arg.ID, Result: arg.Result}, nil
}

func (q *runFakeQuerier) GetTaskPlannedSubtasks(context.Context, int64) ([]database.Subtask, error) {
	if q.plannedErr != nil {
		return nil, q.plannedErr
	}
	return q.planned, nil
}

type runFakeProvider struct {
	providers.FlowProvider

	result *tools.TaskResult
	err    error
	calls  int
}

func (p *runFakeProvider) GetTaskResult(context.Context, int64) (*tools.TaskResult, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return p.result, nil
}

type runFakePublisher struct {
	subscriptions.FlowPublisher
}

func (p *runFakePublisher) TaskUpdated(context.Context, database.Task, []database.Subtask) {}

type runFakeMsgLog struct {
	FlowMsgLogWorker

	reports []string
}

func (m *runFakeMsgLog) PutTaskMsgResult(
	_ context.Context, _ database.MsglogType, _ int64, _, _, result string, _ database.MsglogResultFormat,
) (int64, error) {
	m.reports = append(m.reports, result)
	return 0, nil
}

type runFakeUpdater struct {
	flowStatuses []database.FlowStatus
}

func (u *runFakeUpdater) SetStatus(_ context.Context, status database.FlowStatus) error {
	u.flowStatuses = append(u.flowStatuses, status)
	return nil
}

// runFakeSTC hands out queued subtasks in order and then reports an empty plan.
type runFakeSTC struct {
	SubtaskController

	queue     []SubtaskWorker
	popped    int
	refines   int
	refineErr error
	popErr    error
}

func (c *runFakeSTC) PopSubtask(context.Context, TaskUpdater) (SubtaskWorker, error) {
	if c.popErr != nil {
		return nil, c.popErr
	}
	if c.popped >= len(c.queue) {
		return nil, nil
	}
	st := c.queue[c.popped]
	c.popped++
	return st, nil
}

func (c *runFakeSTC) RefineSubtasks(context.Context) error {
	c.refines++
	return c.refineErr
}

func (c *runFakeSTC) ListSubtasks(context.Context) []SubtaskWorker { return nil }

// newRunWorker wires a taskWorker to the fakes. The fake querier is the only
// component the run writes to, so the terminal state asserted in tests is the
// state a real database would end up in.
func newRunWorker(
	q *runFakeQuerier, p *runFakeProvider, stc SubtaskController,
) (*taskWorker, *runFakeUpdater) {
	updater := &runFakeUpdater{}
	tw := &taskWorker{
		mx:  &sync.RWMutex{},
		stc: stc,
		taskCtx: &TaskContext{
			TaskID:    42,
			TaskTitle: "task",
			FlowContext: FlowContext{
				DB:        q,
				Provider:  p,
				Publisher: &runFakePublisher{},
				MsgLog:    &runFakeMsgLog{},
			},
		},
		updater: updater,
	}
	return tw, updater
}

// lastStatus returns the final task status written, or "" when none was.
func lastStatus(q *runFakeQuerier) database.TaskStatus {
	if len(q.statuses) == 0 {
		return ""
	}
	return q.statuses[len(q.statuses)-1]
}

// ---- runSubtask tests -------------------------------------------------------

// A timed out subtask must end terminally. The subtask's own interrupt handler
// parks it in Waiting, which would stall the whole flow with no way back.
func TestRunSubtaskForcesTerminalStateOnDeadline(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	st := &fakeSubtask{id: 270, status: database.SubtaskStatusRunning, blockUntilEnd: true}
	err := (&taskWorker{}).runSubtask(ctx, st)

	require.Error(t, err)
	assert.ErrorIs(t, err, errRunBudget)
	require.NotEmpty(t, st.setStatuses)
	assert.Equal(t, database.SubtaskStatusFailed, st.setStatuses[len(st.setStatuses)-1])
}

// The forced terminal write must not inherit the run's context: when the parent
// is already dead that write is the only thing left standing between the subtask
// and a row that reloads back into the plan it just timed out of.
func TestRunSubtaskTerminalWriteUsesLiveContext(t *testing.T) {
	t.Parallel()

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	st := &fakeSubtask{id: 1, runErr: context.DeadlineExceeded}
	(&taskWorker{}).runSubtask(cancelled, st)

	require.NotEmpty(t, st.setStatusErrAtCall)
	for i, err := range st.setStatusErrAtCall {
		assert.NoError(t, err, "terminal write %d ran on a dead context", i)
		assert.True(t, st.setStatusHasDeadline[i], "terminal write %d context must be bounded", i)
	}
}

// The budget marker must stay distinguishable from a user cancel, otherwise
// handleInterrupting would undo the terminal state we just set.
func TestRunSubtaskBudgetErrorIsNotDeadlineExceeded(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	st := &fakeSubtask{id: 1, blockUntilEnd: true}
	err := (&taskWorker{}).runSubtask(ctx, st)

	require.Error(t, err)
	assert.False(t, errors.Is(err, context.DeadlineExceeded))
	assert.True(t, errors.Is(err, errRunBudget))
}

// A provider with its own inner deadline returns the same sentinel while our
// budget is still healthy. Counting that as a budget stop would end the run for
// a timeout it never caused, and force a terminal status it did not earn.
func TestRunSubtaskForeignDeadlineIsNotABudgetStop(t *testing.T) {
	t.Parallel()

	st := &fakeSubtask{id: 1, runErr: context.DeadlineExceeded}

	err := (&taskWorker{}).runSubtask(context.Background(), st)

	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.False(t, errors.Is(err, errRunBudget))
	assert.Empty(t, st.setStatuses, "a foreign timeout must not force the subtask terminal")
}

func TestRunSubtaskPassesThroughOtherErrors(t *testing.T) {
	t.Parallel()

	want := errors.New("boom")
	st := &fakeSubtask{id: 1, runErr: want}

	err := (&taskWorker{}).runSubtask(context.Background(), st)

	assert.ErrorIs(t, err, want)
	assert.False(t, errors.Is(err, errRunBudget))
	assert.Empty(t, st.setStatuses)
}

func TestRunSubtaskSuccessSetsNothing(t *testing.T) {
	t.Parallel()

	st := &fakeSubtask{id: 1, status: database.SubtaskStatusFinished}

	assert.NoError(t, (&taskWorker{}).runSubtask(context.Background(), st))
	assert.Empty(t, st.setStatuses)
}

// ---- status read-back tests -------------------------------------------------

// A failed subtask returns no error from Run, so the outcome is read from status:
// this is what lets the run count consecutive failures and stop re-planning.
func TestSubtaskFailedOnlyOnFailedStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status database.SubtaskStatus
		want   bool
	}{
		{"failed", database.SubtaskStatusFailed, true},
		{"finished", database.SubtaskStatusFinished, false},
		{"running", database.SubtaskStatusRunning, false},
		{"waiting", database.SubtaskStatusWaiting, false},
		{"created", database.SubtaskStatusCreated, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			st := &fakeSubtask{id: 1, status: tc.status}
			assert.Equal(t, tc.want, subtaskFailed(context.Background(), st))
		})
	}
}

func TestSubtaskFailedIgnoresStatusError(t *testing.T) {
	t.Parallel()

	st := &fakeSubtask{id: 1, status: database.SubtaskStatusFailed, statusErr: errors.New("db down")}

	assert.False(t, subtaskFailed(context.Background(), st))
}

// Guards the convergence budget itself: a zero or negative bound would either
// run nothing or never stop, which is exactly the failure mode being fixed.
// Not parallel: it reads the budgets that other tests replace.
func TestConvergenceBudgetsAreSane(t *testing.T) {
	assert.Greater(t, maxSubtasksPerRun, 0)
	assert.Greater(t, maxConsecutiveSubtaskFailures, 0)
	assert.LessOrEqual(t, maxConsecutiveSubtaskFailures, maxSubtasksPerRun)
	assert.Greater(t, maxTaskRunDuration, maxSubtaskRunDuration)
	assert.Greater(t, maxSubtaskRunDuration, time.Duration(0))
}

// ---- Run loop tests ---------------------------------------------------------

// The flagship path: a subtask that eats the run budget must still reach
// finalizeRun and leave a terminal row. Parking it in Waiting is the exact
// non-terminal state this change exists to eliminate.
func TestRunFinalizesWhenSubtaskConsumesTheRunBudget(t *testing.T) {
	// Not parallel: this test tightens the package level budgets.
	restore := tightenBudgets(30*time.Millisecond, 30*time.Millisecond)
	defer restore()

	q := &runFakeQuerier{}
	p := &runFakeProvider{result: &tools.TaskResult{Success: tools.Bool(true), Result: "findings"}}
	tw, _ := newRunWorker(q, p, &runFakeSTC{
		queue: []SubtaskWorker{&fakeSubtask{id: 1, blockUntilEnd: true}},
	})

	err := tw.Run(context.Background())

	require.NoError(t, err)
	assert.Positive(t, p.calls, "the run must report on what it collected")
	assert.Equal(t, database.TaskStatusFailed, lastStatus(q),
		"a run cut off by its budget did not reach its objective")
	for _, status := range q.statuses {
		assert.NotEqual(t, database.TaskStatusWaiting, status,
			"a budget stop must never park the task in Waiting")
	}
}

// A timed out subtask parks the task in Waiting on its way out. That parking
// must not beat the budget check, or the run would abandon the terminal write it
// just spent its whole budget earning.
func TestRunBudgetStopSurvivesAParkedTask(t *testing.T) {
	// Not parallel: this test tightens the package level budgets.
	restore := tightenBudgets(40*time.Millisecond, 40*time.Millisecond)
	defer restore()

	q := &runFakeQuerier{}
	p := &runFakeProvider{result: &tools.TaskResult{Success: tools.Bool(true), Result: "findings"}}

	var tw *taskWorker
	st := &fakeSubtask{id: 1, blockUntilEnd: true}
	st.onRun = func() {
		// what subtaskWorker.Run does on a chain error before returning
		_ = tw.SetStatus(context.Background(), database.TaskStatusWaiting)
	}
	tw, _ = newRunWorker(q, p, &runFakeSTC{queue: []SubtaskWorker{st}})

	require.NoError(t, tw.Run(context.Background()))
	assert.Positive(t, p.calls, "the run must report on what it collected")
	assert.Equal(t, database.TaskStatusFailed, lastStatus(q),
		"the parked task must still end terminally once the budget is spent")
	assert.False(t, tw.IsWaiting(), "the run continued, so it must not stay parked")
}

// A plan of exactly maxSubtasksPerRun subtasks that all succeed is a completed
// run. Reporting it as a truncation would fail every successful short task.
func TestRunReportsExhaustedPlanAsSuccess(t *testing.T) {
	restore := tightenBudgets(time.Minute, time.Minute)
	defer restore()

	queue := make([]SubtaskWorker, 0, maxSubtasksPerRun)
	for i := 0; i < maxSubtasksPerRun; i++ {
		queue = append(queue, &fakeSubtask{id: int64(i + 1), status: database.SubtaskStatusFinished})
	}

	q := &runFakeQuerier{} // nothing left planned
	p := &runFakeProvider{result: &tools.TaskResult{Success: tools.Bool(true), Result: "done"}}
	tw, _ := newRunWorker(q, p, &runFakeSTC{queue: queue})

	require.NoError(t, tw.Run(context.Background()))
	assert.Equal(t, database.TaskStatusFinished, lastStatus(q))
}

// The same four subtasks with work still queued afterwards is a truncation and
// must not be reported as a success.
func TestRunReportsTruncatedPlanAsFailure(t *testing.T) {
	restore := tightenBudgets(time.Minute, time.Minute)
	defer restore()

	queue := make([]SubtaskWorker, 0, maxSubtasksPerRun)
	for i := 0; i < maxSubtasksPerRun; i++ {
		queue = append(queue, &fakeSubtask{id: int64(i + 1), status: database.SubtaskStatusFinished})
	}

	q := &runFakeQuerier{planned: []database.Subtask{{ID: 99, Status: database.SubtaskStatusCreated}}}
	p := &runFakeProvider{result: &tools.TaskResult{Success: tools.Bool(true), Result: "done"}}
	tw, _ := newRunWorker(q, p, &runFakeSTC{queue: queue})

	require.NoError(t, tw.Run(context.Background()))
	assert.Equal(t, database.TaskStatusFailed, lastStatus(q))
}

// Two failures in a row stop the run instead of re-planning forever.
func TestRunStopsAfterConsecutiveFailures(t *testing.T) {
	restore := tightenBudgets(time.Minute, time.Minute)
	defer restore()

	queue := []SubtaskWorker{
		&fakeSubtask{id: 1, status: database.SubtaskStatusFailed},
		&fakeSubtask{id: 2, status: database.SubtaskStatusFailed},
		&fakeSubtask{id: 3, status: database.SubtaskStatusFinished},
	}

	q := &runFakeQuerier{}
	p := &runFakeProvider{result: &tools.TaskResult{Success: tools.Bool(true), Result: "partial"}}
	stc := &runFakeSTC{queue: queue}
	tw, _ := newRunWorker(q, p, stc)

	require.NoError(t, tw.Run(context.Background()))
	assert.Equal(t, 2, stc.popped, "the third subtask must never run")
	assert.Equal(t, database.TaskStatusFailed, lastStatus(q))
}

// The terminal write is the whole point of the guard, so a failing reporter must
// not be able to prevent it. The run reports on a stub instead.
func TestFinalizeRunWritesTerminalStatusWhenReporterFails(t *testing.T) {
	restore := tightenBudgets(time.Minute, time.Minute)
	defer restore()

	q := &runFakeQuerier{}
	p := &runFakeProvider{err: errors.New("provider quota exhausted")}
	tw, _ := newRunWorker(q, p, &runFakeSTC{})

	require.NoError(t, tw.finalizeRun("task run budget of 15m0s is exhausted"))
	assert.Positive(t, p.calls)
	assert.Equal(t, database.TaskStatusFailed, lastStatus(q))
	require.NotEmpty(t, q.results)
	assert.Contains(t, q.results[0], "quota exhausted", "the stub has to say why no report exists")
}

// Even a reporter that returns nothing at all must leave a terminal row behind.
func TestFinalizeRunSurvivesANilResult(t *testing.T) {
	q := &runFakeQuerier{}
	p := &runFakeProvider{} // result stays nil, err stays nil
	tw, _ := newRunWorker(q, p, &runFakeSTC{})

	require.NoError(t, tw.finalizeRun(""))
	assert.Equal(t, database.TaskStatusFailed, lastStatus(q))
}

// A clean run whose reporter declares success is finished, not failed.
func TestFinalizeRunReportsCleanRunAsFinished(t *testing.T) {
	q := &runFakeQuerier{}
	p := &runFakeProvider{result: &tools.TaskResult{Success: tools.Bool(true), Result: "findings"}}
	tw, _ := newRunWorker(q, p, &runFakeSTC{})

	require.NoError(t, tw.finalizeRun(""))
	assert.Equal(t, database.TaskStatusFinished, lastStatus(q))
}

// A stopping reason always overrides the reporter: a truncated run has not met
// its objective no matter how optimistic the report is.
func TestFinalizeRunStopReasonOverridesSuccess(t *testing.T) {
	q := &runFakeQuerier{}
	p := &runFakeProvider{result: &tools.TaskResult{Success: tools.Bool(true), Result: "partial findings"}}
	tw, _ := newRunWorker(q, p, &runFakeSTC{})

	require.NoError(t, tw.finalizeRun("2 subtasks in a row failed or timed out"))
	assert.Equal(t, database.TaskStatusFailed, lastStatus(q))
}

// tightenBudgets swaps in short budgets and returns a restore function. Tests
// that call it must not use t.Parallel(), since the budgets are package state.
func tightenBudgets(task, subtask time.Duration) func() {
	prevTask, prevSubtask := maxTaskRunDuration, maxSubtaskRunDuration
	maxTaskRunDuration, maxSubtaskRunDuration = task, subtask
	return func() {
		maxTaskRunDuration, maxSubtaskRunDuration = prevTask, prevSubtask
	}
}

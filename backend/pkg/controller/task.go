package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"pentagi/pkg/database"
	obs "pentagi/pkg/observability"
	"pentagi/pkg/tools"

	"github.com/sirupsen/logrus"
)

type FlowUpdater interface {
	SetStatus(ctx context.Context, status database.FlowStatus) error
}

type TaskWorker interface {
	GetTaskID() int64
	GetFlowID() int64
	GetUserID() int64
	GetTitle() string
	IsCompleted() bool
	IsWaiting() bool
	GetStatus(ctx context.Context) (database.TaskStatus, error)
	SetStatus(ctx context.Context, status database.TaskStatus) error
	GetResult(ctx context.Context) (string, error)
	SetResult(ctx context.Context, result string) error
	PutInput(ctx context.Context, input string) error
	Run(ctx context.Context) error
	Finish(ctx context.Context) error
	InvalidateSubtasks(subtaskIDs []int64)
}

type taskWorker struct {
	mx        *sync.RWMutex
	stc       SubtaskController
	taskCtx   *TaskContext
	updater   FlowUpdater
	completed bool
	waiting   bool
	// runStart is set when a run begins and used only to report elapsed time in
	// the termination logs. A worker that never ran reports no elapsed value.
	runStart time.Time
}

func NewTaskWorker(
	ctx context.Context,
	flowCtx *FlowContext,
	input string,
	outputPath string,
	updater FlowUpdater,
) (TaskWorker, error) {
	ctx, span := obs.Observer.NewSpan(ctx, obs.SpanKindInternal, "controller.NewTaskWorker")
	defer span.End()

	ctx = tools.PutAgentContext(ctx, database.MsgchainTypePrimaryAgent)

	title, err := flowCtx.Provider.GetTaskTitle(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to get task title: %w", err)
	}

	task, err := flowCtx.DB.CreateTask(ctx, database.CreateTaskParams{
		Status:     database.TaskStatusCreated,
		Title:      title,
		Input:      input,
		OutputPath: sql.NullString{String: outputPath, Valid: outputPath != ""},
		FlowID:     flowCtx.FlowID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create task in DB: %w", err)
	}

	flowCtx.Publisher.TaskCreated(ctx, task, []database.Subtask{})

	taskCtx := &TaskContext{
		FlowContext: *flowCtx,
		TaskID:      task.ID,
		TaskTitle:   title,
		TaskInput:   input,
		OutputPath:  outputPath,
	}
	stc := NewSubtaskController(taskCtx)

	_, err = taskCtx.MsgLog.PutTaskMsg(
		ctx,
		database.MsglogTypeInput,
		taskCtx.TaskID,
		"", // thinking is empty because this is input
		input,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to put input for task %d: %w", taskCtx.TaskID, err)
	}

	err = stc.GenerateSubtasks(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to generate subtasks: %w", err)
	}

	subtasks, err := flowCtx.DB.GetTaskSubtasks(ctx, task.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to get subtasks for task %d: %w", task.ID, err)
	}

	flowCtx.Publisher.TaskUpdated(ctx, task, subtasks)

	return &taskWorker{
		mx:        &sync.RWMutex{},
		stc:       stc,
		taskCtx:   taskCtx,
		updater:   updater,
		completed: false,
		waiting:   false,
	}, nil
}

func LoadTaskWorker(
	ctx context.Context,
	task database.Task,
	flowCtx *FlowContext,
	updater FlowUpdater,
) (TaskWorker, error) {
	ctx, span := obs.Observer.NewSpan(ctx, obs.SpanKindInternal, "controller.LoadTaskWorker")
	defer span.End()

	ctx = tools.PutAgentContext(ctx, database.MsgchainTypePrimaryAgent)
	taskCtx := &TaskContext{
		FlowContext: *flowCtx,
		TaskID:      task.ID,
		TaskTitle:   task.Title,
		TaskInput:   task.Input,
		// A NULL output_path is loaded as "" (not declared), matching what
		// NewTaskWorker stores for an empty declaration.
		OutputPath: task.OutputPath.String,
	}

	stc := NewSubtaskController(taskCtx)
	var completed, waiting bool
	switch task.Status {
	case database.TaskStatusFinished, database.TaskStatusFailed:
		completed = true
	case database.TaskStatusWaiting:
		waiting = true
	case database.TaskStatusRunning:
	case database.TaskStatusCreated:
		return nil, fmt.Errorf("task %d has created yet: loading aborted: %w", task.ID, ErrNothingToLoad)
	}

	tw := &taskWorker{
		mx:        &sync.RWMutex{},
		stc:       stc,
		taskCtx:   taskCtx,
		updater:   updater,
		completed: completed,
		waiting:   waiting,
	}

	if err := tw.stc.LoadSubtasks(ctx, task.ID, tw); err != nil {
		return nil, fmt.Errorf("failed to load subtasks for task %d: %w", task.ID, err)
	}

	return tw, nil
}

func (tw *taskWorker) GetTaskID() int64 {
	return tw.taskCtx.TaskID
}

func (tw *taskWorker) GetFlowID() int64 {
	return tw.taskCtx.FlowID
}

func (tw *taskWorker) GetUserID() int64 {
	return tw.taskCtx.UserID
}

func (tw *taskWorker) GetTitle() string {
	return tw.taskCtx.TaskTitle
}

func (tw *taskWorker) IsCompleted() bool {
	tw.mx.RLock()
	defer tw.mx.RUnlock()

	return tw.completed
}

func (tw *taskWorker) IsWaiting() bool {
	tw.mx.RLock()
	defer tw.mx.RUnlock()

	return tw.waiting
}

// elapsed reports how long the current run has been going, for the termination
// logs. It is empty for a worker that never entered Run.
func (tw *taskWorker) elapsed() string {
	tw.mx.RLock()
	defer tw.mx.RUnlock()

	if tw.runStart.IsZero() {
		return ""
	}
	return time.Since(tw.runStart).Round(time.Millisecond).String()
}

func (tw *taskWorker) GetStatus(ctx context.Context) (database.TaskStatus, error) {
	task, err := tw.taskCtx.DB.GetTask(ctx, tw.taskCtx.TaskID)
	if err != nil {
		return database.TaskStatusFailed, err
	}

	return task.Status, nil
}

// this function is exclusively change task internal properties "completed" and "waiting"
func (tw *taskWorker) SetStatus(ctx context.Context, status database.TaskStatus) error {
	task, err := tw.taskCtx.DB.UpdateTaskStatus(ctx, database.UpdateTaskStatusParams{
		Status: status,
		ID:     tw.taskCtx.TaskID,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Replacement can leave this worker behind after deleting its row.
			// Treat it as complete so flow shutdown remains idempotent.
			logrus.WithContext(ctx).WithFields(logrus.Fields{
				"task_id":          tw.taskCtx.TaskID,
				"requested_status": status,
			}).Warn("task no longer exists in the database, treating as already finished")

			tw.mx.Lock()
			tw.completed = true
			tw.waiting = false
			tw.mx.Unlock()

			return nil
		}

		return fmt.Errorf("failed to set task %d status: %w", tw.taskCtx.TaskID, err)
	}

	subtasks, err := tw.taskCtx.DB.GetTaskSubtasks(ctx, tw.taskCtx.TaskID)
	if err != nil {
		return fmt.Errorf("failed to get task %d subtasks: %w", tw.taskCtx.TaskID, err)
	}

	tw.taskCtx.Publisher.TaskUpdated(ctx, task, subtasks)

	tw.mx.Lock()
	defer tw.mx.Unlock()

	switch status {
	case database.TaskStatusRunning:
		tw.completed = false
		tw.waiting = false
		err = tw.updater.SetStatus(ctx, database.FlowStatusRunning)
	case database.TaskStatusWaiting:
		tw.completed = false
		tw.waiting = true
		err = tw.updater.SetStatus(ctx, database.FlowStatusWaiting)
	case database.TaskStatusFinished, database.TaskStatusFailed:
		tw.completed = true
		tw.waiting = false
		// the last task was done, set flow status to Waiting new user input
		err = tw.updater.SetStatus(ctx, database.FlowStatusWaiting)
	default:
		// status Created is not possible to set by this call
		return fmt.Errorf("unsupported task status: %s", status)
	}
	if err != nil {
		return fmt.Errorf("failed to set flow status in back propagation: %w", err)
	}

	return nil
}

func (tw *taskWorker) GetResult(ctx context.Context) (string, error) {
	task, err := tw.taskCtx.DB.GetTask(ctx, tw.taskCtx.TaskID)
	if err != nil {
		return "", err
	}

	return task.Result, nil
}

func (tw *taskWorker) SetResult(ctx context.Context, result string) error {
	_, err := tw.taskCtx.DB.UpdateTaskResult(ctx, database.UpdateTaskResultParams{
		Result: result,
		ID:     tw.taskCtx.TaskID,
	})
	if err != nil {
		return fmt.Errorf("failed to set task %d result: %w", tw.taskCtx.TaskID, err)
	}

	return nil
}

func (tw *taskWorker) PutInput(ctx context.Context, input string) error {
	if !tw.IsWaiting() {
		return fmt.Errorf("task is not waiting")
	}

	for _, st := range tw.stc.ListSubtasks(ctx) {
		if !st.IsCompleted() && st.IsWaiting() {
			if err := st.PutInput(ctx, input); err != nil {
				return fmt.Errorf("failed to put input to subtask %d: %w", st.GetSubtaskID(), err)
			} else {
				break
			}
		}
	}

	return nil
}

// Convergence guards: a task run has to reach a terminal state within a bounded
// budget instead of re-planning subtasks until something outside the process
// intervenes. Without these a stalled run never reports an outcome at all.
const (
	// maxSubtasksPerRun caps how many subtasks a task run executes before it
	// reports on what it already has and stops re-planning.
	maxSubtasksPerRun = 4
	// maxConsecutiveSubtaskFailures stops the run once this many subtasks in a
	// row failed or timed out.
	maxConsecutiveSubtaskFailures = 2
	// resultWriteRetryTimeout bounds the one retry of a result write that failed on
	// the finalize budget. It has to stay bounded: an open-ended retry on a dead
	// database would block before SetStatus and strand the row in Running, which is
	// the very non-terminal state the convergence guards exist to prevent.
	resultWriteRetryTimeout = 30 * time.Second
)

// The two wall clock budgets are vars so tests can tighten them to milliseconds;
// production always uses these defaults. Read them once per run, not per call.
var (
	// maxTaskRunDuration caps the wall clock time of one task run.
	maxTaskRunDuration = 15 * time.Minute
	// maxSubtaskRunDuration caps the wall clock time of one subtask run.
	maxSubtaskRunDuration = 5 * time.Minute
)

// errRunBudget marks a stop caused by our own convergence budget. It is
// deliberately not context.DeadlineExceeded so the parking handler leaves the
// task terminal instead of waiting for input that cannot help a timeout.
var errRunBudget = errors.New("task run budget exhausted")

func (tw *taskWorker) Run(ctx context.Context) error {
	ctx = tools.PutAgentContext(ctx, database.MsgchainTypePrimaryAgent)

	tw.mx.Lock()
	tw.runStart = time.Now()
	tw.mx.Unlock()

	runCtx, cancel := context.WithTimeout(ctx, maxTaskRunDuration)
	defer cancel()

	executed, consecutiveFailures := 0, 0
	var stopReason string

	for executed < maxSubtasksPerRun {
		// A cancelled parent is a user decision, not a budget stop: hand it back so
		// the task stays resumable instead of being reported as a failed run.
		if err := ctx.Err(); err != nil {
			tw.handleInterrupting(err)
			return err
		}

		if runCtx.Err() != nil {
			stopReason = fmt.Sprintf("task run budget of %s is exhausted", maxTaskRunDuration)
			break
		}

		st, err := tw.stc.PopSubtask(runCtx, tw)
		if err != nil {
			return tw.parkForInput(fmt.Errorf("failed to pop subtask for the task %d: %w", tw.taskCtx.TaskID, err))
		}

		// empty queue for subtasks means the plan is exhausted and the task is done
		if st == nil {
			break
		}

		executed++

		runErr := tw.runSubtask(runCtx, st)
		budgeted := errors.Is(runErr, errRunBudget) || budgetExhausted(ctx, runCtx)

		// pass through if task is waiting from back status propagation. A stop we
		// caused ourselves is excluded: a timed out subtask parks the task on its
		// way out, and that parking must not survive a run with a spent budget.
		if tw.IsWaiting() {
			if !budgeted {
				return nil
			}
			// The run is still going, so the row should say so rather than advertise
			// an input prompt nobody is waiting on.
			if err := tw.SetStatus(ctx, database.TaskStatusRunning); err != nil {
				return tw.parkForInput(err)
			}
		}

		if runErr != nil && !errors.Is(runErr, errRunBudget) {
			if errors.Is(runErr, context.Canceled) {
				tw.handleInterrupting(runErr)
				return runErr
			}
			return tw.parkForInput(fmt.Errorf("subtask %d failed: %w", st.GetSubtaskID(), runErr))
		}

		// A failed subtask returns no error, so the outcome is read back from its
		// status: a task that keeps failing has to stop rather than re-plan forever.
		if runErr != nil || subtaskFailed(runCtx, st) {
			consecutiveFailures++
			if consecutiveFailures >= maxConsecutiveSubtaskFailures {
				stopReason = fmt.Sprintf("%d subtasks in a row failed or timed out", consecutiveFailures)
				break
			}
		} else {
			consecutiveFailures = 0
		}

		// The budget can be spent by the very subtask that just ran. Stopping here
		// is what keeps the run out of the re-planning paths below, which would
		// park the task in Waiting on an expired context instead of finalizing it.
		if budgetExhausted(ctx, runCtx) {
			stopReason = fmt.Sprintf("task run budget of %s is exhausted", maxTaskRunDuration)
			break
		}

		if executed >= maxSubtasksPerRun {
			// A plan that is already exhausted is a completed run, not a truncated
			// one. Only a cut-off with work still queued is reported as incomplete.
			if tw.planHasRemainingSubtasks(runCtx) {
				stopReason = fmt.Sprintf("subtask budget of %d is exhausted", maxSubtasksPerRun)
			}
			break
		}

		if err := tw.stc.RefineSubtasks(runCtx); err != nil {
			// A stop from outside is still resumable; only our own spent budget turns
			// this into a terminal cut-off.
			if ctx.Err() != nil {
				tw.handleInterrupting(ctx.Err())
				return ctx.Err()
			}
			if budgetExhausted(ctx, runCtx) {
				stopReason = fmt.Sprintf("task run budget of %s is exhausted", maxTaskRunDuration)
				break
			}
			if errors.Is(err, context.Canceled) {
				ctx = context.Background()
			}
			// Re-planning failed on a live run: park for user input rather than
			// finalizing, since the plan may still be recoverable by steering.
			return tw.parkForInput(fmt.Errorf("failed to refine subtasks list for the task %d: %w", tw.taskCtx.TaskID, err))
		}
	}

	return tw.finalizeRun(stopReason)
}

// runSubtask bounds a single subtask run so one stuck agent chain cannot consume
// the whole task budget. A timed out subtask is forced terminal: the subtask's own
// interrupt handler would otherwise leave it Waiting with no way back to running.
func (tw *taskWorker) runSubtask(ctx context.Context, st SubtaskWorker) error {
	subtaskCtx, cancel := context.WithTimeout(ctx, maxSubtaskRunDuration)
	defer cancel()

	subtaskStart := time.Now()
	err := st.Run(subtaskCtx)

	// Only our own budget counts as a run-budget stop. A provider with an inner
	// deadline returns the same sentinel without breaching the subtask budget, and
	// labelling it as one would stop the run for a timeout it never caused.
	if !errors.Is(err, context.DeadlineExceeded) || subtaskCtx.Err() == nil {
		return err
	}

	setCtx, setCancel := context.WithTimeout(context.Background(), maxSubtaskRunDuration)
	defer setCancel()
	if setErr := st.SetStatus(setCtx, database.SubtaskStatusFailed); setErr != nil {
		// Without this write the subtask row stays Running and reload resets it to
		// Created, which re-queues the subtask that just timed out.
		logrus.WithContext(ctx).WithError(setErr).WithFields(logrus.Fields{
			"task_id":    tw.taskCtx.TaskID,
			"subtask_id": st.GetSubtaskID(),
			"budget":     "subtask_run",
		}).Error("failed to force the timed out subtask into a terminal state")
	}

	// The provider's own cause is reported but deliberately not wrapped: errRunBudget
	// has to stay the only sentinel on the chain so the caller can tell our budget
	// apart from a user cancel, which is a different state entirely.
	logrus.WithContext(ctx).WithError(err).WithFields(logrus.Fields{
		"task_id":      tw.taskCtx.TaskID,
		"subtask_id":   st.GetSubtaskID(),
		"budget":       "subtask_run",
		"budget_limit": maxSubtaskRunDuration.String(),
		"elapsed":      time.Since(subtaskStart).Round(time.Millisecond).String(),
		"stop_reason":  "subtask_run_budget_exhausted",
		"final_status": database.SubtaskStatusFailed,
	}).Warn("subtask exceeded its run budget and was forced terminal")

	return fmt.Errorf("subtask %d exceeded run budget %s: %w (cause: %v)",
		st.GetSubtaskID(), maxSubtaskRunDuration, errRunBudget, err)
}

func subtaskFailed(ctx context.Context, st SubtaskWorker) bool {
	status, err := st.GetStatus(ctx)
	return err == nil && status == database.SubtaskStatusFailed
}

// budgetExhausted reports that our own run budget ran out. A cancelled parent is
// a user decision, and checking ctx first is what keeps a Stop from being written
// up as a budget cut-off — two states that must never share a stop reason.
func budgetExhausted(ctx, runCtx context.Context) bool {
	return runCtx.Err() != nil && ctx.Err() == nil
}

// planHasRemainingSubtasks reports whether planned subtasks are still queued. It
// separates a run that exhausted its plan, which reached its objective, from one
// the subtask budget truncated mid-plan, which did not.
func (tw *taskWorker) planHasRemainingSubtasks(ctx context.Context) bool {
	remaining, err := tw.taskCtx.DB.GetTaskPlannedSubtasks(ctx, tw.taskCtx.TaskID)
	if err != nil {
		// The plan cannot be read, so report a cut-off rather than claim success.
		logrus.WithContext(ctx).WithError(err).WithField("task_id", tw.taskCtx.TaskID).
			Warn("failed to read remaining subtasks, treating the run as truncated")
		return true
	}

	return len(remaining) > 0
}

// finalizeRun reports the task outcome on its own budget so the task always
// reaches a terminal state. The terminal write is deliberately independent of the
// reporter call: the convergence guard is worthless if the one write that closes
// the run is gated on the most failure-prone dependency in the stack.
func (tw *taskWorker) finalizeRun(stopReason string) error {
	if stopReason != "" {
		logrus.WithContext(context.Background()).WithFields(logrus.Fields{
			"task_id":     tw.taskCtx.TaskID,
			"budget":      "task_run",
			"elapsed":     tw.elapsed(),
			"stop_reason": stopReason,
		}).Warn("task run stopped before its plan was exhausted")
	}

	ctx, cancel := context.WithTimeout(context.Background(), maxSubtaskRunDuration)
	defer cancel()
	ctx = tools.PutAgentContext(ctx, database.MsgchainTypePrimaryAgent)

	jobResult, err := tw.taskCtx.Provider.GetTaskResult(ctx, tw.taskCtx.TaskID)
	if err != nil || jobResult == nil {
		if err == nil {
			err = errors.New("task result provider returned nothing")
		}
		logrus.WithContext(ctx).WithError(err).WithField("task_id", tw.taskCtx.TaskID).
			Warn("failed to build the task report, falling back to a stub")
		jobResult = &tools.TaskResult{
			Success: tools.Bool(false),
			Result:  fmt.Sprintf("The task run stopped before a report could be produced: %s", err),
			Message: "failed to generate the task report",
		}
	}

	// a truncated run did not reach its objective, so it must not report success
	if stopReason != "" {
		jobResult.Success = tools.Bool(false)
		if jobResult.Message == "" {
			jobResult.Message = stopReason
		}
	}

	// The result and the status are what make the run legible later, so both are
	// written before the report message and neither depends on the other.
	emptyResult := jobResult.Result == ""
	if emptyResult {
		// A blank result is its own failure mode: it has to be visible without
		// having to notice that the report message is missing.
		logrus.WithContext(ctx).WithField("task_id", tw.taskCtx.TaskID).
			Warn("task report is empty, storing an explanatory stub instead")
		jobResult.Result = fmt.Sprintf("The task run stopped before a report could be produced: %s", stopReason)
	}

	// The deliverable is written before the terminal state, so the file the task points
	// at holds the same text the row does. A write failure is folded into the result and
	// forces Success = false: the evidence is incomplete without the artifact, and a quiet
	// "success" over a missing report is precisely the failure this exists to remove. It
	// must not stop the terminal write either — that is what closes the run.
	outputPath := taskOutputPath(tw.taskCtx.TaskID, tw.taskCtx.OutputPath)
	hostPath := ""
	writeFailure := ""
	if tw.taskCtx.ResultSink != nil {
		hostPath, err = tw.taskCtx.ResultSink.WriteResult(ctx, outputPath, jobResult.Result)
		if err != nil {
			writeFailure = "file_write"
			logrus.WithContext(ctx).WithError(err).WithFields(logrus.Fields{
				"task_id":     tw.taskCtx.TaskID,
				"output_path": outputPath,
				"failure":     "file_write",
				"stop_reason": stopReason,
			}).Error("failed to write the task result file")
			jobResult.Result += fmt.Sprintf("\n\nfailure: file_write: %s (output path: %s)", err, outputPath)
			jobResult.Success = tools.Bool(false)
		}
	}

	if err := tw.SetResult(ctx, jobResult.Result); err != nil {
		// The result row is the record of truth for the deliverable: terminal and empty
		// is the exact failure this run was built to remove. Retry once on a fresh
		// bounded context, because budget spent earlier in the finalize must not cost
		// the result as well.
		logrus.WithContext(ctx).WithError(err).WithFields(logrus.Fields{
			"task_id":     tw.taskCtx.TaskID,
			"failure":     "result_write",
			"stop_reason": stopReason,
		}).Error("failed to store the task result")
		retryCtx, retryCancel := context.WithTimeout(context.Background(), resultWriteRetryTimeout)
		if retryErr := tw.SetResult(retryCtx, jobResult.Result); retryErr != nil {
			logrus.WithContext(retryCtx).WithError(retryErr).WithFields(logrus.Fields{
				"task_id":     tw.taskCtx.TaskID,
				"failure":     "result_write",
				"stop_reason": stopReason,
			}).Error("failed to store the task result on retry")
		}
		retryCancel()
	}

	var taskStatus database.TaskStatus
	if jobResult.Success {
		taskStatus = database.TaskStatusFinished
	} else {
		taskStatus = database.TaskStatusFailed
	}

	if err := tw.SetStatus(ctx, taskStatus); err != nil {
		// Last resort: the row must not stay Running with nothing left to run.
		logrus.WithContext(ctx).WithError(err).WithFields(logrus.Fields{
			"task_id":      tw.taskCtx.TaskID,
			"failure":      "status_write",
			"final_status": taskStatus,
		}).Error("failed to set the terminal task status")
		_ = tw.SetStatus(context.Background(), taskStatus)
	}

	format := database.MsglogResultFormatMarkdown
	_, err = tw.taskCtx.MsgLog.PutTaskMsgResult(
		ctx,
		database.MsglogTypeReport,
		tw.taskCtx.TaskID,
		"", // thinking is empty because agent can't return it
		tw.taskCtx.TaskTitle,
		jobResult.Result,
		format,
	)
	if err != nil {
		logrus.WithContext(ctx).WithError(err).WithFields(logrus.Fields{
			"task_id": tw.taskCtx.TaskID,
			"failure": "report_msg",
		}).Error("failed to put the task report message")
	}

	// One line that answers why the run ended and where it landed. Everything the
	// audit asks to be able to reconstruct is on this entry.
	logrus.WithContext(ctx).WithFields(logrus.Fields{
		"task_id":      tw.taskCtx.TaskID,
		"budget":       "task_run",
		"budget_limit": maxTaskRunDuration.String(),
		"elapsed":      tw.elapsed(),
		"stop_reason":  stopReason,
		"final_status": taskStatus,
		"empty_result": emptyResult,
		"output_path":  outputPath,
		"host_path":    hostPath,
		"failure":      writeFailure,
	}).Info("task run finalized")

	return nil
}

// parkForInput leaves the task in Waiting so a user can steer it back. Every
// error exit from Run goes through here: leaving the row in Running would strand
// it, because nothing else writes a status for a task whose run stopped early.
func (tw *taskWorker) parkForInput(err error) error {
	if err == nil {
		return nil
	}

	resetCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if errSt := tw.SetStatus(resetCtx, database.TaskStatusWaiting); errSt != nil {
		logrus.WithError(errSt).WithField("task_id", tw.taskCtx.TaskID).
			Warn("failed to set task waiting after a run error")
	}

	return err
}

// handleInterrupting parks the task when err is a user cancel or a deadline, and
// skips it when the task is already marked completed (Finished/Failed) so we do
// not revive a finished task.
func (tw *taskWorker) handleInterrupting(err error) {
	if err == nil {
		return
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return
	}
	if tw.IsCompleted() {
		return
	}

	_ = tw.parkForInput(err)
}

func (tw *taskWorker) Finish(ctx context.Context) error {
	if tw.IsCompleted() {
		return fmt.Errorf("task has already completed")
	}

	for _, st := range tw.stc.ListSubtasks(ctx) {
		if !st.IsCompleted() {
			if err := st.Finish(ctx); err != nil {
				return err
			}
		}
	}

	if err := tw.SetStatus(ctx, database.TaskStatusFinished); err != nil {
		return err
	}

	return nil
}

// InvalidateSubtasks evicts subtaskIDs from this task's in-memory subtask
// controller. See flowWorker.InvalidateTaskSubtasks for why this is needed.
func (tw *taskWorker) InvalidateSubtasks(subtaskIDs []int64) {
	tw.stc.InvalidateSubtasks(subtaskIDs)
}

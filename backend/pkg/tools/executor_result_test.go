package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"pentagi/pkg/database"
)

// fakeToolCallLog records what the executor persists and behaves like a real
// database driver: a write against a cancelled or expired context fails.
type fakeToolCallLog struct {
	putArgs   json.RawMessage
	putResult string
	success   bool
	failed    bool
}

func (f *fakeToolCallLog) PutLog(
	ctx context.Context,
	callID string,
	name string,
	args json.RawMessage,
	taskID, subtaskID *int64,
) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f.putArgs = args
	return 1, nil
}

func (f *fakeToolCallLog) UpdateLogSuccess(ctx context.Context, id int64, result string, durationSeconds float64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.success = true
	f.putResult = result
	return nil
}

func (f *fakeToolCallLog) UpdateLogFailed(ctx context.Context, id int64, result string, durationSeconds float64) error {
	f.failed = true
	return nil
}

// fakeMsgLog rejects writes made against a cancelled context, the way a real
// database driver does, and records the result it managed to store.
type fakeMsgLog struct {
	updatedResult string
	updateErr     error
}

func (f *fakeMsgLog) PutMsg(
	ctx context.Context,
	msgType database.MsglogType,
	taskID, subtaskID *int64,
	streamID int64,
	thinking, msg string,
) (int64, error) {
	return 7, nil
}

func (f *fakeMsgLog) UpdateMsgResult(
	ctx context.Context,
	msgID, streamID int64,
	result string,
	resultFormat database.MsglogResultFormat,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.updatedResult = result
	return f.updateErr
}

// A handler that recovers a large partial result must still see that result
// reach the agent, even when the caller's context is cancelled while the
// handler is running (the flow-23 incident: the budget killed the subtask
// after the searcher had already earned its result). Losing it is what made
// the agent report a failed search instead of a partial one.
func TestExecuteKeepsResultWhenContextCancelsDuringHandler(t *testing.T) {
	t.Parallel()

	tclp := &fakeToolCallLog{}
	mlp := &fakeMsgLog{}

	ctx, cancel := context.WithCancel(t.Context())
	ce := &customExecutor{
		tclp: tclp,
		mlp:  mlp,
		handlers: map[string]ExecutorHandler{
			TerminalToolName: func(ctx context.Context, name string, args json.RawMessage) (string, error) {
				cancel()
				return "[INCOMPLETE SEARCH]\nrecovered payload", nil
			},
		},
	}

	result, err := ce.Execute(ctx, 1, "call-1", TerminalToolName, "", "", json.RawMessage(`{"query":"pu-er","message":"hello"}`))
	if err != nil {
		t.Fatalf("Execute() unexpected error: %v", err)
	}
	if !strings.Contains(result, "[INCOMPLETE SEARCH]") {
		t.Fatalf("Execute() result = %q, expected the recovered payload", result)
	}
	if mlp.updatedResult != result {
		t.Fatalf("stored message result = %q, want %q", mlp.updatedResult, result)
	}
	if tclp.putResult != result {
		t.Fatalf("stored toolcall result = %q, want %q", tclp.putResult, result)
	}
}

// The persistence deadline starts when the write begins, not at Execute
// entry, so a tool that takes longer than the deadline must not find its
// post-handler persistence context already expired.
func TestExecuteDoesNotExpirePersistContextDuringHandler(t *testing.T) {
	t.Parallel()

	tclp := &fakeToolCallLog{}
	mlp := &fakeMsgLog{}

	ce := &customExecutor{
		tclp: tclp,
		mlp:  mlp,
		handlers: map[string]ExecutorHandler{
			TerminalToolName: func(ctx context.Context, name string, args json.RawMessage) (string, error) {
				time.Sleep(toolResultPersistTimeout + 50*time.Millisecond)
				return "slow tool payload", nil
			},
		},
	}

	result, err := ce.Execute(t.Context(), 1, "call-1", TerminalToolName, "", "", json.RawMessage(`{"query":"pu-er","message":"hello"}`))
	if err != nil {
		t.Fatalf("Execute() unexpected error: %v", err)
	}
	if result != "slow tool payload" {
		t.Fatalf("Execute() result = %q, want %q", result, "slow tool payload")
	}
	if !tclp.success {
		t.Fatal("toolcall result was not marked successful; the persistence deadline must not have started during the handler")
	}
	if mlp.updatedResult != result {
		t.Fatalf("stored message result = %q, want %q", mlp.updatedResult, result)
	}
}

// A failure to persist must not abort the agent chain: the caller turns a
// non-nil error into a discarded tool response.
func TestExecuteKeepsResultWhenPersistenceFails(t *testing.T) {
	t.Parallel()

	mlp := &fakeMsgLog{updateErr: errors.New("database is down")}

	ce := &customExecutor{
		tclp: &fakeToolCallLog{},
		mlp:  mlp,
		handlers: map[string]ExecutorHandler{
			TerminalToolName: func(ctx context.Context, name string, args json.RawMessage) (string, error) {
				return "handler payload", nil
			},
		},
	}

	result, err := ce.Execute(t.Context(), 1, "call-1", TerminalToolName, "", "", json.RawMessage(`{"query":"pu-er","message":"hello"}`))
	if err != nil {
		t.Fatalf("Execute() unexpected error: %v", err)
	}
	if result != "handler payload" {
		t.Fatalf("Execute() result = %q, want %q", result, "handler payload")
	}
}

// The handler keeps the real arguments while the persisted copy is masked.
func TestExecutePersistsRedactedArguments(t *testing.T) {
	t.Parallel()

	tclp := &fakeToolCallLog{}
	ce := &customExecutor{
		tclp: tclp,
		mlp:  &fakeMsgLog{},
		handlers: map[string]ExecutorHandler{
			TerminalToolName: func(ctx context.Context, name string, args json.RawMessage) (string, error) {
				if !strings.Contains(string(args), "synthetic-secret") {
					t.Errorf("handler received masked arguments: %s", args)
				}
				return "ok", nil
			},
		},
	}

	_, err := ce.Execute(t.Context(), 1, "call-1", TerminalToolName, "", "", json.RawMessage(`{"api_key":"synthetic-secret","query":"pu-er","message":"hello"}`))
	if err != nil {
		t.Fatalf("Execute() unexpected error: %v", err)
	}
	if strings.Contains(string(tclp.putArgs), "synthetic-secret") {
		t.Fatalf("persisted arguments still contain the secret: %s", tclp.putArgs)
	}
	if !strings.Contains(string(tclp.putArgs), "query") {
		t.Fatalf("persisted arguments lost the harmless fields: %s", tclp.putArgs)
	}
}

// A secret in the agent message must not be stored in the message log. The
// message is free text, so this is the value-shape layer's responsibility.
func TestExecuteRedactsSecretInMessage(t *testing.T) {
	t.Parallel()

	mlp := &fakeMsgLog{}
	ce := &customExecutor{
		tclp: &fakeToolCallLog{},
		mlp:  mlp,
		handlers: map[string]ExecutorHandler{
			TerminalToolName: func(ctx context.Context, name string, args json.RawMessage) (string, error) {
				return "ok", nil
			},
		},
	}

	msg := `curl -H "Authorization: ghp_syntheticsecretvalue1234" https://example.test`
	args, err := json.Marshal(map[string]string{"message": msg})
	if err != nil {
		t.Fatalf("failed to marshal args: %v", err)
	}

	if _, err := ce.Execute(t.Context(), 1, "call-1", TerminalToolName, "", "", args); err != nil {
		t.Fatalf("Execute() unexpected error: %v", err)
	}
	if strings.Contains(mlp.updatedResult, "syntheticsecretvalue1234") {
		t.Fatalf("stored message still contains the secret: %s", mlp.updatedResult)
	}
}

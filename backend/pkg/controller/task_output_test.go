package controller

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"pentagi/pkg/config"
	"pentagi/pkg/database"
	"pentagi/pkg/docker"
	"pentagi/pkg/tools"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- result sink fakes ------------------------------------------------------

// fakeResultSink records what the runtime asked to persist. A fake rather than the
// real sink keeps these tests about finalizeRun's contract: which path is chosen,
// what text is handed over, and what a write failure does to the terminal state.
type fakeResultSink struct {
	paths    []string
	results  []string
	hostPath string
	err      error
}

func (f *fakeResultSink) WriteResult(_ context.Context, outputPath string, content string) (string, error) {
	f.paths = append(f.paths, outputPath)
	f.results = append(f.results, content)
	if f.err != nil {
		return f.hostPath, f.err
	}
	if f.hostPath != "" {
		return f.hostPath, nil
	}
	return "/mirror" + outputPath, nil
}

// ---- finalizeRun contract ---------------------------------------------------

// A declared output path is the path the framework must write to — not a suggestion,
// and not a path the model gets to choose afterwards.
func TestFinalizeRunWritesResultToDeclaredOutputPath(t *testing.T) {
	t.Parallel()

	q := &runFakeQuerier{}
	p := &runFakeProvider{result: &tools.TaskResult{Success: tools.Bool(true), Result: "findings"}}
	sink := &fakeResultSink{}
	tw, _ := newRunWorker(q, p, &runFakeSTC{})
	tw.taskCtx.ResultSink = sink
	tw.taskCtx.OutputPath = "/root/lotus_report.md"

	require.NoError(t, tw.finalizeRun(""))
	require.Len(t, sink.paths, 1)
	assert.Equal(t, "/root/lotus_report.md", sink.paths[0])
	require.Len(t, sink.results, 1)
	require.NotEmpty(t, q.results, "the result has to reach the row as well as the file")
	assert.Equal(t, q.results[0], sink.results[0], "the file must hold exactly the stored result")
	assert.Equal(t, database.TaskStatusFinished, lastStatus(q))
}

// With no declared path the runtime still has to leave a deliverable somewhere. The
// silent alternative is a run that reports findings and leaves no file at all.
func TestFinalizeRunFallsBackToCanonicalOutputPath(t *testing.T) {
	t.Parallel()

	q := &runFakeQuerier{}
	p := &runFakeProvider{result: &tools.TaskResult{Success: tools.Bool(true), Result: "findings"}}
	sink := &fakeResultSink{}
	tw, _ := newRunWorker(q, p, &runFakeSTC{})
	tw.taskCtx.ResultSink = sink

	require.NoError(t, tw.finalizeRun(""))
	require.Len(t, sink.paths, 1)
	assert.Equal(t, "/root/task-42_report.md", sink.paths[0])
}

// A result that never reached disk is not evidence of success. The run must say so in
// the result text itself, where it cannot be missed, and must not report Finished.
func TestFinalizeRunWriteFailureIsReportedAndForcesFailure(t *testing.T) {
	t.Parallel()

	q := &runFakeQuerier{}
	p := &runFakeProvider{result: &tools.TaskResult{Success: tools.Bool(true), Result: "findings"}}
	sink := &fakeResultSink{err: errors.New("container is not running")}
	tw, _ := newRunWorker(q, p, &runFakeSTC{})
	tw.taskCtx.ResultSink = sink

	require.NoError(t, tw.finalizeRun(""))
	require.NotEmpty(t, q.results)
	assert.Contains(t, q.results[0], "failure: file_write")
	assert.Contains(t, q.results[0], "container is not running", "the result has to say why the write failed")
	assert.Equal(t, database.TaskStatusFailed, lastStatus(q),
		"a missing deliverable overrides a provider that claimed success")
}

// The write is the least reliable step in finalizeRun. If it could withhold the
// terminal state the task would strand in Running — the exact failure the convergence
// guard exists to prevent.
func TestFinalizeRunWriteFailureDoesNotBlockTerminalState(t *testing.T) {
	t.Parallel()

	q := &runFakeQuerier{}
	p := &runFakeProvider{result: &tools.TaskResult{Success: tools.Bool(true), Result: "findings"}}
	msgLog := &runFakeMsgLog{}
	tw, _ := newRunWorker(q, p, &runFakeSTC{})
	tw.taskCtx.ResultSink = &fakeResultSink{err: errors.New("disk full")}
	tw.taskCtx.MsgLog = msgLog

	require.NoError(t, tw.finalizeRun(""))
	assert.NotEmpty(t, q.results, "the result must still be stored")
	assert.NotEmpty(t, q.statuses, "the task must still reach a terminal status")
	assert.NotEmpty(t, msgLog.reports, "the report message must still go out")
}

// The result row is the record of truth for the deliverable. A write that fails because
// the finalize budget ran out is retried once on a fresh context: without that the row is
// terminal and empty, which is the exact failure the whole output contract exists to
// remove. The retry is bounded so a dead database cannot block the terminal status write.
func TestFinalizeRunRetriesAResultWriteThatFailedOnTheBudget(t *testing.T) {
	t.Parallel()

	q := &runFakeQuerier{resultErrOnce: errors.New("context deadline exceeded")}
	p := &runFakeProvider{result: &tools.TaskResult{Success: tools.Bool(true), Result: "findings"}}
	tw, _ := newRunWorker(q, p, &runFakeSTC{})

	require.NoError(t, tw.finalizeRun(""))
	require.Equal(t, []string{"findings"}, q.results,
		"the retried write must land the result instead of leaving the row empty")
	assert.Equal(t, database.TaskStatusFinished, lastStatus(q))
}

// A nil sink means "this runtime persists no file" and must stay harmless: every test
// that only exercises terminal-state handling builds a worker without one.
func TestFinalizeRunWithoutASinkStillFinalizes(t *testing.T) {
	t.Parallel()

	q := &runFakeQuerier{}
	p := &runFakeProvider{result: &tools.TaskResult{Success: tools.Bool(true), Result: "findings"}}
	tw, _ := newRunWorker(q, p, &runFakeSTC{})

	require.NoError(t, tw.finalizeRun(""))
	assert.Equal(t, database.TaskStatusFinished, lastStatus(q))
	require.NotEmpty(t, q.results)
	assert.NotContains(t, q.results[0], "failure: file_write")
}

// The canonical path is derived from the task, so two tasks in one flow cannot collide
// on the same report file.
func TestTaskOutputPath(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "/root/report.md", taskOutputPath(7, "/root/report.md"))
	assert.Equal(t, "/root/task-7_report.md", taskOutputPath(7, ""))
}

// ---- flowResultSink ---------------------------------------------------------

// fakeDocker implements docker.DockerClient for the four calls the result sink makes.
// The rest stays a nil embedded interface so an unexpected call panics instead of
// quietly succeeding.
type fakeDocker struct {
	docker.DockerClient

	running    bool
	runningErr error
	copyDst    string
	copyBody   []byte
	copyErr    error
	// execFile is what `cat` reports inside the container. Empty means the file is
	// missing there, which is the failure the read-back exists to catch.
	execFile string
	execErr  error
}

func (d *fakeDocker) IsContainerRunning(context.Context, string) (bool, error) {
	return d.running, d.runningErr
}

func (d *fakeDocker) CopyToContainer(
	_ context.Context, _ string, dstPath string, content io.Reader, _ client.CopyToContainerOptions,
) error {
	d.copyDst = dstPath
	if d.copyErr != nil {
		return d.copyErr
	}
	body, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	d.copyBody = body
	return nil
}

func (d *fakeDocker) ContainerExecCreate(context.Context, string, client.ExecCreateOptions) (client.ExecCreateResult, error) {
	if d.execErr != nil {
		return client.ExecCreateResult{}, d.execErr
	}
	return client.ExecCreateResult{ID: "exec-1"}, nil
}

func (d *fakeDocker) ContainerExecAttach(context.Context, string, client.ExecAttachOptions) (client.HijackedResponse, error) {
	return client.NewHijackedResponse(dockerExecStream(d.execFile), "application/vnd.docker.raw-stream"), nil
}

// dockerExecStream returns a connection carrying payload framed the way Docker
// multiplexes exec output: [1-byte stream type][3 pad][4-byte big-endian size][payload].
// Without real framing StdCopy would reject the stream and every verify would look failed.
func dockerExecStream(payload string) net.Conn {
	local, remote := net.Pipe()
	go func() {
		defer remote.Close()

		var header [8]byte
		header[0] = byte(stdcopy.Stdout)
		binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
		if _, err := remote.Write(header[:]); err != nil {
			return
		}
		_, _ = io.WriteString(remote, payload)
	}()
	return local
}

// tarEntries returns the entries of a tar stream as name -> contents.
func tarEntries(t *testing.T, body []byte) map[string]string {
	t.Helper()

	entries := make(map[string]string)
	tr := tar.NewReader(bytes.NewReader(body))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		content, err := io.ReadAll(tr)
		require.NoError(t, err)
		entries[hdr.Name] = string(content)
	}
	return entries
}

func newSinkFixture(t *testing.T) (*config.Config, *fakeDocker, string) {
	t.Helper()

	dataDir := t.TempDir()
	return &config.Config{DataDir: dataDir}, &fakeDocker{running: true, execFile: "report body"}, dataDir
}

// The deliverable has to land on the host as well as in the sandbox: a stopped container
// cannot be read back, and a report that dies with the flow is no report at all.
func TestFlowResultSinkWritesHostMirrorAndContainerCopy(t *testing.T) {
	t.Parallel()

	cfg, dc, dataDir := newSinkFixture(t)
	sink := NewFlowResultSink(cfg, dc, 17)

	hostPath, err := sink.WriteResult(context.Background(), "/root/report.md", "report body")
	require.NoError(t, err)

	want := filepath.Join(dataDir, "flow-17-data", "container", "root", "report.md")
	assert.Equal(t, want, hostPath)
	got, err := os.ReadFile(hostPath)
	require.NoError(t, err)
	assert.Equal(t, "report body", string(got))

	assert.Equal(t, "/", dc.copyDst, "the archive carries the path, so the copy is rooted at /")
	entries := tarEntries(t, dc.copyBody)
	assert.Equal(t, map[string]string{"root/report.md": "report body"}, entries)
}

// A result that is not in the container cannot be reported as written, even when the
// host mirror is fine — that is the claim this sink exists to test.
func TestFlowResultSinkRejectsAMissingContainerFile(t *testing.T) {
	t.Parallel()

	cfg, dc, _ := newSinkFixture(t)
	dc.execFile = "" // nothing at the path
	sink := NewFlowResultSink(cfg, dc, 17)

	hostPath, err := sink.WriteResult(context.Background(), "/root/report.md", "report body")
	assert.Error(t, err)
	assert.NotEmpty(t, hostPath, "the host mirror is still worth reporting when the copy failed")
}

func TestFlowResultSinkRejectsATruncatedContainerFile(t *testing.T) {
	t.Parallel()

	cfg, dc, _ := newSinkFixture(t)
	dc.execFile = "report"
	sink := NewFlowResultSink(cfg, dc, 17)

	_, err := sink.WriteResult(context.Background(), "/root/report.md", "report body")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "verification failed")
}

// A container that is gone is a failed write, not a reason to pretend the deliverable
// exists only on the host.
func TestFlowResultSinkRequiresARunningContainer(t *testing.T) {
	t.Parallel()

	cfg, dc, _ := newSinkFixture(t)
	dc.running = false
	sink := NewFlowResultSink(cfg, dc, 17)

	hostPath, err := sink.WriteResult(context.Background(), "/root/report.md", "report body")
	require.Error(t, err)
	assert.NotEmpty(t, hostPath, "the mirror is written before the container copy on purpose")
	_, statErr := os.Stat(hostPath)
	assert.NoError(t, statErr, "a failed copy must still leave a readable artifact")
}

// Past the mirror write every failure has to say that the mirror succeeded: a task marked
// Failed while a clean report sits on the host is two truths about one deliverable, and the
// failure text is the only place that reconciles them. The container-side cause is named
// over the archive error because a failed copy tears the pipe down and would otherwise
// report a closed pipe while hiding why delivery actually failed.
func TestFlowResultSinkNamesTheHostMirrorWhenDeliveryFails(t *testing.T) {
	t.Parallel()

	cfg, dc, _ := newSinkFixture(t)
	dc.running = true
	dc.copyErr = errors.New("no such container")
	sink := NewFlowResultSink(cfg, dc, 17)

	hostPath, err := sink.WriteResult(context.Background(), "/root/report.md", "report body")
	require.Error(t, err)
	assert.NotEmpty(t, hostPath)
	assert.Contains(t, err.Error(), "host mirror written to")
	assert.Contains(t, err.Error(), hostPath, "the reader needs the path of the artifact that did land")
	assert.Contains(t, err.Error(), "no such container", "the container-side cause must not be masked")
	assert.NotContains(t, err.Error(), "failed to build the output archive",
		"the archive symptom must not take the place of the copy error")
}

// The output path names a file the framework writes, so anything that is not a clean
// absolute path is refused rather than normalized into some other location.
func TestFlowResultSinkRejectsUnsafePaths(t *testing.T) {
	t.Parallel()

	cfg, dc, dataDir := newSinkFixture(t)
	sink := NewFlowResultSink(cfg, dc, 17)

	for _, p := range []string{
		"",
		"relative/report.md",
		"/root/../etc/cron.d/evil",
		"/root/../../outside.md",
		"/root//report.md",
		"/root/report.md/",
	} {
		hostPath, err := sink.WriteResult(context.Background(), p, "body")
		assert.Error(t, err, "path %q must be refused", p)
		assert.Empty(t, hostPath, "refused paths must not create anything")
	}

	_, err := os.ReadDir(filepath.Join(dataDir, "flow-17-data"))
	assert.True(t, os.IsNotExist(err), "a refused path must leave the flow data dir untouched")
}

// Nothing in this path is ever handed to a shell: the verify runs `cat <path>` as an
// argv array and the archive entry is a plain filename. A metacharacter is therefore a
// legal (if odd) file name, and it must survive to disk unchanged instead of being
// rewritten into a different file than the one the user declared.
func TestFlowResultSinkTreatsThePathAsAFileNameNotACommand(t *testing.T) {
	t.Parallel()

	cfg, dc, _ := newSinkFixture(t)
	dc.execFile = "body"
	sink := NewFlowResultSink(cfg, dc, 17)

	hostPath, err := sink.WriteResult(context.Background(), "/root/report.md;rm", "body")
	require.NoError(t, err)
	assert.FileExists(t, hostPath)

	entries := tarEntries(t, dc.copyBody)
	assert.Equal(t, map[string]string{"root/report.md;rm": "body"}, entries)
}

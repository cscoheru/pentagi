package controller

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"

	"pentagi/pkg/config"
	"pentagi/pkg/docker"
	"pentagi/pkg/flowfiles"
	"pentagi/pkg/tools"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// TaskResultSink persists a task result to the task's output path.
//
// The runtime owns the deliverable: the model is never asked where it saved anything, and a
// result that did not reach disk cannot be reported as a success. A sink may be nil, which
// means "no persistence" — production paths always build one, and tests that only care about
// the terminal state can leave it unset.
type TaskResultSink interface {
	// WriteResult writes content to outputPath inside the flow sandbox and mirrors it on
	// the host, returning the host path of the mirror. outputPath is an absolute container
	// path; it is validated again here because it names a file that is about to be written.
	WriteResult(ctx context.Context, outputPath string, content string) (hostPath string, err error)
}

// flowResultSink is the production TaskResultSink.
//
// Every result is written twice on purpose. The container copy is what the sandbox and the
// agent see at the path the task declared. The host mirror is what survives the flow ending:
// a stopped container cannot be read back, and a report that disappears with the flow is the
// same deliverable as no report at all (see the host fallback in flowReport).
type flowResultSink struct {
	cfg    *config.Config
	docker docker.DockerClient
	flowID int64
}

// NewFlowResultSink returns a sink that persists results for flowID.
func NewFlowResultSink(cfg *config.Config, dc docker.DockerClient, flowID int64) TaskResultSink {
	return &flowResultSink{cfg: cfg, docker: dc, flowID: flowID}
}

// taskOutputPath returns where the framework must write the result for taskID: the contract
// path the user declared, or the canonical per-task path when none was declared.
//
// Falling back instead of skipping is deliberate — the failure this replaces was a run that
// claimed to have written a report and left no file anywhere.
func taskOutputPath(taskID int64, declared string) string {
	if declared != "" {
		return declared
	}
	return fmt.Sprintf("/root/task-%d_report.md", taskID)
}

func (s *flowResultSink) WriteResult(ctx context.Context, outputPath string, content string) (string, error) {
	if s.cfg == nil || s.docker == nil {
		return "", fmt.Errorf("result sink is not configured")
	}

	// The path names a file we are about to write, so refuse anything that is not a clean
	// absolute path rather than normalizing it into a different path than the one declared.
	contractPath, err := flowfiles.ValidateTaskOutputPath(outputPath)
	if err != nil {
		return "", err
	}
	if contractPath == "" {
		return "", fmt.Errorf("output path is required")
	}

	// Host mirror: <dataDir>/flow-<id>-data/container/<path without its leading slash>.
	sanitized, err := flowfiles.SanitizeContainerCachePath(contractPath)
	if err != nil {
		return "", fmt.Errorf("invalid output path %q: %w", contractPath, err)
	}
	hostDir := flowfiles.FlowContainerDir(s.cfg.DataDir, uint64(s.flowID))
	hostPath := path.Join(hostDir, sanitized)
	// Defense in depth. Every component of `sanitized` was validated before the join, so
	// this cannot fire today; it is what keeps that true if a future writer widens what
	// "sanitized" can mean or a symlink appears in the cache. Writing outside the mirror
	// directory would turn a user-declared output path into an arbitrary host write.
	if !flowfiles.IsWithinDir(hostPath, hostDir) {
		return "", fmt.Errorf("output path %q escapes the flow mirror directory", contractPath)
	}
	if err := os.MkdirAll(path.Dir(hostPath), 0o755); err != nil {
		return "", fmt.Errorf("failed to create output directory: %w", err)
	}
	if err := os.WriteFile(hostPath, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("failed to write host mirror: %w", err)
	}

	// The mirror is written first so a later failure still leaves a readable artifact.
	// Size is checked because flowfiles.WriteFilesTar silently skips a missing file and
	// would turn an absent mirror into an empty archive and a copy that "succeeds".
	info, err := os.Stat(hostPath)
	if err != nil {
		return hostPath, fmt.Errorf("failed to verify host mirror: %w", err)
	}
	if info.Size() != int64(len(content)) {
		return hostPath, fmt.Errorf("host mirror size mismatch: got %d bytes, want %d", info.Size(), len(content))
	}

	// Container copy, so the sandbox sees the deliverable at the declared path. Past this
	// point the mirror already holds the report, so every error below says so: a task marked
	// Failed while a clean report is on disk is two truths about one deliverable, and the
	// failure text is the only place that reconciles them.
	containerName := tools.PrimaryTerminalName(s.cfg.TenantPrefix(), s.flowID)
	running, err := s.docker.IsContainerRunning(ctx, containerName)
	if err != nil {
		return hostPath, fmt.Errorf("host mirror written to %s, sandbox delivery not attempted: failed to check container %q: %w", hostPath, containerName, err)
	}
	if !running {
		return hostPath, fmt.Errorf("host mirror written to %s, sandbox delivery failed: container %q is not running", hostPath, containerName)
	}

	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		errCh <- flowfiles.WriteSingleFileTar(pw, hostPath, sanitized)
	}()
	// The archive entry carries the path without its leading slash, so rooting the copy at
	// "/" yields exactly contractPath.
	copyErr := s.docker.CopyToContainer(ctx, containerName, "/", pr, client.CopyToContainerOptions{AllowOverwriteDirWithFile: true})
	pr.Close()
	writeErr := <-errCh
	// The copy error is checked first on purpose. A failed copy tears down the pipe and
	// leaves the archive writer with ErrClosedPipe, so reporting that symptom instead
	// would name a closed pipe and hide the container-side cause.
	if copyErr != nil {
		if writeErr != nil {
			return hostPath, fmt.Errorf("host mirror written to %s, sandbox delivery failed: %w (archive writer also failed: %v)", hostPath, copyErr, writeErr)
		}
		return hostPath, fmt.Errorf("host mirror written to %s, sandbox delivery failed: %w", hostPath, copyErr)
	}
	if writeErr != nil {
		return hostPath, fmt.Errorf("host mirror written to %s, failed to build the output archive: %w", hostPath, writeErr)
	}

	if err := s.verifyInContainer(ctx, containerName, contractPath, content); err != nil {
		return hostPath, fmt.Errorf("host mirror written to %s, %w", hostPath, err)
	}

	return hostPath, nil
}

// verifyInContainer reads the file back and compares it with what was written.
//
// `cat` is passed as an argv array, not a shell string, so even a path that reached here
// cannot turn into a command. A mismatch is the exact failure this sink exists to surface:
// a task that claims a deliverable it does not actually have.
func (s *flowResultSink) verifyInContainer(ctx context.Context, containerName, containerPath, content string) error {
	execRes, err := s.docker.ContainerExecCreate(ctx, containerName, client.ExecCreateOptions{
		Cmd:          []string{"cat", containerPath},
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return fmt.Errorf("failed to verify output in container: %w", err)
	}

	attach, err := s.docker.ContainerExecAttach(ctx, execRes.ID, client.ExecAttachOptions{})
	if err != nil {
		return fmt.Errorf("failed to verify output in container: %w", err)
	}
	defer attach.Close()

	// Docker multiplexes exec output; stdcopy.StdCopy demuxes it into plain streams.
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, attach.Reader); err != nil {
		return fmt.Errorf("failed to verify output in container: %w", err)
	}
	if !bytes.Equal(stdout.Bytes(), []byte(content)) {
		return fmt.Errorf("output verification failed: container has %d bytes at %s, want %d", stdout.Len(), containerPath, len(content))
	}

	return nil
}

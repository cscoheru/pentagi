package graph

import (
	"context"
	"database/sql"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pentagi/pkg/config"
	"pentagi/pkg/database"
	"pentagi/pkg/docker"
	"pentagi/pkg/flowfiles"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reportFakeQuerier answers only the three calls FlowReport makes. Anything else panics
// through the nil embedded interface, flagging a gap instead of returning zero values.
type reportFakeQuerier struct {
	database.Querier

	containers []database.Container
	tasks      []database.Task
}

func (q *reportFakeQuerier) GetFlow(context.Context, int64) (database.Flow, error) {
	// Owned by the user authedContext() authenticates as.
	return database.Flow{ID: 17, UserID: 1}, nil
}

func (q *reportFakeQuerier) GetFlowContainers(context.Context, int64) ([]database.Container, error) {
	return q.containers, nil
}

func (q *reportFakeQuerier) GetFlowTasks(context.Context, int64) ([]database.Task, error) {
	return q.tasks, nil
}

// reportFakeDocker serves the find/cat pair the container scan runs. Both execs carry the
// same path back, so the fixture only has to say where the report is and what is in it.
type reportFakeDocker struct {
	docker.DockerClient

	path    string
	content string
}

func (d *reportFakeDocker) ContainerExecCreate(_ context.Context, _ string, opts client.ExecCreateOptions) (client.ExecCreateResult, error) {
	if len(opts.Cmd) > 0 && opts.Cmd[0] == "find" {
		return client.ExecCreateResult{ID: "find"}, nil
	}
	return client.ExecCreateResult{ID: "cat"}, nil
}

func (d *reportFakeDocker) ContainerExecAttach(_ context.Context, execID string, _ client.ExecAttachOptions) (client.HijackedResponse, error) {
	payload := d.content
	if execID == "find" {
		payload = d.path
	}
	return client.NewHijackedResponse(execStream(payload), "application/vnd.docker.raw-stream"), nil
}

// execStream frames payload the way Docker multiplexes exec output:
// [1-byte stream type][3 pad][4-byte big-endian size][payload]. Without real framing
// stdcopy.StdCopy rejects the stream and every scan would look failed.
func execStream(payload string) net.Conn {
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

func writeReportMirror(t *testing.T, dataDir string, containerPath, content string) {
	t.Helper()

	sanitized, err := flowfiles.SanitizeContainerCachePath(containerPath)
	require.NoError(t, err)

	p := filepath.Join(flowfiles.FlowContainerDir(dataDir, 17), sanitized)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
}

// authedContext returns a context authorized for flows.view as the owner of the flow.
func authedContext() context.Context {
	ctx := SetUserID(context.Background(), 1)
	ctx = SetUserPermissions(ctx, []string{"flows.view"})
	return ctx
}

func newReportResolver(q database.Querier, dc docker.DockerClient, dataDir string) *queryResolver {
	return &queryResolver{&Resolver{
		DB:           q,
		Config:       &config.Config{DataDir: dataDir},
		Logger:       logrus.NewEntry(logrus.StandardLogger()),
		DockerClient: dc,
	}}
}

// A flow that has ended has no running container to scan. The report still exists as the
// host mirror written at finalize time, and returning nothing there is what made
// "Download Report" empty for every finished flow.
func TestFlowReportFallsBackToHostMirrorWhenNoContainerIsRunning(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	writeReportMirror(t, dataDir, "/root/task-1_report.md", "the final report body, long enough to be a real report")

	q := &reportFakeQuerier{containers: []database.Container{{Name: "terminal", Status: database.ContainerStatusStopped}}}
	r := newReportResolver(q, nil, dataDir) // docker is never reached

	got, err := r.FlowReport(authedContext(), 17)
	require.NoError(t, err)
	assert.Contains(t, got, "the final report body")
}

// Both copies of the same report can exist; the bigger one is the consolidated final
// version rather than an intermediate draft, matching the in-container rule. Both copies
// have to clear the container scan's 200-byte stub filter, or the comparison would never
// be exercised at all.
func TestFlowReportPrefersTheLargerCopy(t *testing.T) {
	t.Parallel()

	// Well past the stub filter, and clearly ordered by length.
	medium := "container report " + strings.Repeat("x", 260)
	large := "host mirror " + strings.Repeat("y", 420)

	t.Run("container wins when it is longer", func(t *testing.T) {
		t.Parallel()

		dataDir := t.TempDir()
		writeReportMirror(t, dataDir, "/root/task-1_report.md", medium)

		q := &reportFakeQuerier{containers: []database.Container{{Name: "terminal", Status: database.ContainerStatusRunning}}}
		dc := &reportFakeDocker{path: "/root/task-1_report.md\n", content: large}
		r := newReportResolver(q, dc, dataDir)

		got, err := r.FlowReport(authedContext(), 17)
		require.NoError(t, err)
		assert.Equal(t, large, got)
	})

	t.Run("host mirror wins when it is longer", func(t *testing.T) {
		t.Parallel()

		dataDir := t.TempDir()
		writeReportMirror(t, dataDir, "/root/task-1_report.md", large)

		q := &reportFakeQuerier{containers: []database.Container{{Name: "terminal", Status: database.ContainerStatusRunning}}}
		dc := &reportFakeDocker{path: "/root/task-1_report.md\n", content: medium}
		r := newReportResolver(q, dc, dataDir)

		got, err := r.FlowReport(authedContext(), 17)
		require.NoError(t, err)
		assert.Equal(t, large, got)
	})
}

// A task may declare an output path whose name says nothing about reports. The fallback
// only finds it because the task recorded the contract path.
func TestFlowReportFindsADeclaredOutputPath(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	writeReportMirror(t, dataDir, "/root/findings.md", "deliverable written to a declared path")

	q := &reportFakeQuerier{
		tasks: []database.Task{{OutputPath: sql.NullString{String: "/root/findings.md", Valid: true}}},
	}
	r := newReportResolver(q, nil, dataDir)

	got, err := r.FlowReport(authedContext(), 17)
	require.NoError(t, err)
	assert.Contains(t, got, "deliverable written to a declared path")
}

func TestFlowReportEmptyWhenNothingExists(t *testing.T) {
	t.Parallel()

	r := newReportResolver(&reportFakeQuerier{}, nil, t.TempDir())

	got, err := r.FlowReport(authedContext(), 17)
	require.NoError(t, err)
	assert.Empty(t, got)
}

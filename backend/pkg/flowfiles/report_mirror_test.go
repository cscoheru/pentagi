package flowfiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeMirror drops a file into the flow's container mirror directory the way the result
// sink does, and returns its path.
func writeMirror(t *testing.T, dataDir string, flowID uint64, containerPath, content string) string {
	t.Helper()

	sanitized, err := SanitizeContainerCachePath(containerPath)
	require.NoError(t, err)

	p := filepath.Join(FlowContainerDir(dataDir, flowID), sanitized)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	return p
}

// The biggest report is the consolidated one; intermediate drafts are smaller. This is the
// same rule the in-container scan uses, applied to the mirror that outlives the flow.
func TestLargestReportMirrorReturnsTheBiggest(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	writeMirror(t, dataDir, 17, "/root/task-1_report.md", "draft")
	writeMirror(t, dataDir, 17, "/root/task-2_report.md", "the consolidated final report")
	writeMirror(t, dataDir, 17, "/root/task-2_assessment.md", "notes")

	got, err := LargestReportMirror(dataDir, 17, nil)
	require.NoError(t, err)
	assert.Equal(t, "the consolidated final report", got)
}

// A contract path is an arbitrary name. Nothing about /root/findings.md says "report", so
// it is only reachable because the task recorded it — without that the report of a flow
// that declared its own path would be lost the moment the container stopped.
func TestLargestReportMirrorFindsADeclaredContractPath(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	writeMirror(t, dataDir, 17, "/root/findings.md", "declared deliverable")

	got, err := LargestReportMirror(dataDir, 17, []string{"/root/findings.md"})
	require.NoError(t, err)
	assert.Equal(t, "declared deliverable", got)

	without, err := LargestReportMirror(dataDir, 17, nil)
	require.NoError(t, err)
	assert.Empty(t, without, "no name pattern can discover a contract path that is not declared")
}

// The pattern scan still has to work when the contract path is unknown, and both sources
// compete on size rather than one shadowing the other.
func TestLargestReportMirrorComparesBothSources(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	writeMirror(t, dataDir, 17, "/root/findings.md", "small")
	writeMirror(t, dataDir, 17, "/root/task-1_report.md", "a much longer consolidated report")

	got, err := LargestReportMirror(dataDir, 17, []string{"/root/findings.md"})
	require.NoError(t, err)
	assert.Equal(t, "a much longer consolidated report", got)
}

// Files that are not reports must not win the comparison just for being large.
func TestLargestReportMirrorIgnoresOtherFiles(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	writeMirror(t, dataDir, 17, "/root/notes.txt", strings.Repeat("x", 4096))
	writeMirror(t, dataDir, 17, "/root/task-1_report.md", "report")

	got, err := LargestReportMirror(dataDir, 17, nil)
	require.NoError(t, err)
	assert.Equal(t, "report", got)
}

// A short report is still a report. The in-container scan drops stubs under 200 bytes;
// doing that here too would leave a genuinely short deliverable with no way to read it
// back after the flow ends.
func TestLargestReportMirrorKeepsShortReports(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	writeMirror(t, dataDir, 17, "/root/task-1_report.md", "tiny")

	got, err := LargestReportMirror(dataDir, 17, nil)
	require.NoError(t, err)
	assert.Equal(t, "tiny", got)
}

func TestLargestReportMirrorEmptyWhenNothingIsThere(t *testing.T) {
	t.Parallel()

	got, err := LargestReportMirror(t.TempDir(), 17, nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// Contract paths are user input. One that tries to leave the mirror directory is refused
// rather than normalized: the writer rejects it, so the reader must too, or the fallback
// could be aimed at some other file. A decoy at the normalized name proves the refusal.
func TestLargestReportMirrorSkipsEscapingContractPaths(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "outside.md"), []byte("not a report"), 0o644))
	// Where a lenient clean would have landed.
	writeMirror(t, dataDir, 17, "/outside.md", "decoy")

	got, err := LargestReportMirror(dataDir, 17, []string{"../../outside.md", "/root/../../outside.md", "relative.md"})
	require.NoError(t, err)
	assert.Empty(t, got, "an escaping or relative path must not resolve to any mirror file")

	// A valid declared path in the same call is still honoured.
	writeMirror(t, dataDir, 17, "/root/findings.md", "declared")
	got, err = LargestReportMirror(dataDir, 17, []string{"../../outside.md", "/root/findings.md"})
	require.NoError(t, err)
	assert.Equal(t, "declared", got)
}

// The mirror cache also holds whatever the agent pulled into the sandbox, so a file named
// like a report can be far larger than any real one, and flowReport reads it once per
// request. The read is bounded on the stream — capping after a full os.ReadFile would
// allocate the whole entry and cap nothing — and the content is truncated with a marker
// rather than skipped, because a silently empty report is the failure this mirror exists
// to prevent.
func TestLargestReportMirrorCapsAnOversizedReport(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	writeMirror(t, dataDir, 17, "/root/huge_report.md", strings.Repeat("a", reportMirrorMaxBytes+1024))

	got, err := LargestReportMirror(dataDir, 17, nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(got, "aaaa"), "the report content must still be readable")
	assert.True(t, strings.HasSuffix(got, truncationNotice), "truncation must be visible, not silent")
	assert.Equal(t, reportMirrorMaxBytes+len(truncationNotice), len(got),
		"the read must stop at the cap instead of buffering the whole entry")
}

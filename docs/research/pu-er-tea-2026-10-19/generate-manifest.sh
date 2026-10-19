#!/bin/sh
# Regenerates source-manifest.json for this research directory.
#
#   cd docs/research/pu-er-tea-2026-10-19 && ./generate-manifest.sh
#
# Uses only shasum(1) and wc(1) so it runs on stock macOS and Linux.
# Re-running is idempotent: the file is rewritten from scratch each time.
#
# The static half of the manifest (bucket / role / provenance) lives in the
# here-doc below. The dynamic half (sha256, bytes) is computed from the files
# on disk, so the manifest always describes the bytes that actually exist.

set -eu

cd "$(dirname "$0")"

FILES="README.md
report.md
hot/hottest-topics.md
trends/trends.md
analysis/blocked-causes.md
raw/01-msglog-index.md
raw/02-searxng-results.md
raw/03-browser-failures.md
raw/04-memory-store-hit.md
raw/05-task-prompt.md"

json_escape() {
    printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'
}

{
    printf '{\n'
    printf '  "schema": "pentagi.research-manifest/v1",\n'
    printf '  "generated_at": "%s",\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf '  "generator": "./generate-manifest.sh",\n'

    printf '  "source": {\n'
    printf '    "system": "PentAGI",\n'
    printf '    "flow_id": 23,\n'
    printf '    "task_id": 20,\n'
    printf '    "subtask_id": 289,\n'
    printf '    "subtask_status": "failed",\n'
    printf '    "trace_id": "a803fa7652b0607ea7e8b3b876b8b366",\n'
    printf '    "model": "MiniMax-M3",\n'
    printf '    "provider": "minimax",\n'
    printf '    "window_start": "2026-10-19T13:19:25Z",\n'
    printf '    "window_end": "2026-10-19T13:22:48Z",\n'
    printf '    "msglog_ids": [3726, 3742],\n'
    printf '    "msglog_id_range": "3726-3742",\n'
    printf '    "msglog_count": 17,\n'
    printf '    "raw_dump": "/Users/kjonekong/.claude/projects/-Users-kjonekong-projects-pentAGI/b79ad017-ad62-40c6-bd3f-21f211828fc8/tool-results/call_u1ym2qxeie00r7qkrjh09dr9.json"\n'
    printf '  },\n'

    printf '  "counts": {\n'
    printf '    "searxng_results": 29,\n'
    printf '    "browser_attempts": 12,\n'
    printf '    "browser_failures": 12,\n'
    printf '    "valid_report_items": 22,\n'
    printf '    "report_items_industry_media": 7,\n'
    printf '    "report_items_brand_activity": 15,\n'
    printf '    "report_items_tieba": 0,\n'
    printf '    "report_items_social": 0,\n'
    printf '    "results_with_published_date": 0,\n'
    printf '    "hottest_topics": 5,\n'
    printf '    "trends": 10\n'
    printf '  },\n'

    printf '  "files": [\n'
    first=1
    for f in $FILES; do
        [ -f "$f" ] || { echo "missing file: $f" >&2; exit 1; }
        digest=$(shasum -a 256 "$f" | cut -d' ' -f1)
        bytes=$(wc -c < "$f" | tr -d ' ')
        case "$f" in
            README.md)              bucket="index";      role="navigation index and quick facts" ;;
            report.md)              bucket="report";     role="full four-section Chinese report with limitations" ;;
            hot/*)                  bucket="hot";        role="hottest topics with ranking method and evidence" ;;
            trends/*)               bucket="trends";     role="structural trends with evidence-strength grading" ;;
            analysis/*)             bucket="analysis";   role="code-level root-cause analysis of retrieval blocking" ;;
            raw/02-searxng-results.md) bucket="raw";     role="verbatim 29 Searxng results (primary evidence)" ;;
            raw/03-browser-failures.md) bucket="raw";    role="verbatim 12 browser failures with root-cause table" ;;
            raw/01-msglog-index.md)  bucket="raw";        role="17-row msglog call index" ;;
            raw/04-memory-store-hit.md) bucket="raw";     role="PentAGI vector-memory hit record (URLs redacted)" ;;
            raw/05-task-prompt.md)   bucket="raw";        role="original task prompt text" ;;
            *)                      bucket="raw";        role="raw evidence" ;;
        esac
        [ $first -eq 1 ] || printf ',\n'
        first=0
        printf '    { "path": "%s", "bucket": "%s", "role": "%s", "bytes": %s, "sha256": "%s" }' \
            "$(json_escape "$f")" "$bucket" "$role" "$bytes" "$digest"
    done
    printf '\n  ],\n'

    printf '  "buckets": {\n'
    printf '    "hot":    { "dir": "hot/",    "files": ["hot/hottest-topics.md"], "meaning": "current hottest 5 topics" },\n'
    printf '    "trends": { "dir": "trends/", "files": ["trends/trends.md"], "meaning": "structural directions with evidence strength" },\n'
    printf '    "raw":    { "dir": "raw/",    "files": ["raw/01-msglog-index.md", "raw/02-searxng-results.md", "raw/03-browser-failures.md", "raw/04-memory-store-hit.md", "raw/05-task-prompt.md"], "meaning": "unprocessed retrieval evidence and call trace" },\n'
    printf '    "report": { "dir": ".",       "files": ["report.md"], "meaning": "assembled report" },\n'
    printf '    "analysis": { "dir": "analysis/", "files": ["analysis/blocked-causes.md"], "meaning": "why retrieval was blocked and how to get past it" }\n'
    printf '  },\n'

    printf '  "constraints": {\n'
    printf '    "web_search_and_fetch_only": true,\n'
    printf '    "no_port_scan_no_vuln_probe": true,\n'
    printf '    "no_captcha_bypass_no_proxy_rotation": true,\n'
    printf '    "all_items_from_retrieved_pages": true,\n'
    printf '    "unconfirmable_dates_marked": "日期未确认",\n'
    printf '    "fabricated_dates": false,\n'
    printf '    "memory_store_used_as_source": false\n'
    printf '  }\n'
    printf '}\n'
} > source-manifest.json

echo "wrote source-manifest.json"

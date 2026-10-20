#!/bin/sh
# Regenerates source-manifest.json for this research directory.
# Uses only sh and wc so it runs on stock macOS and Linux.
#
#   cd docs/research/pu-er-tea-2026-10-19 && ./generate-manifest.sh
#
# The static half of the manifest (source block, counts, constraints) lives in
# the here-doc below. The dynamic half (sha256, bytes) is computed from the
# files on disk. Re-running after changing any tracked file is the way to keep
# the manifest honest.

cd "$(dirname "$0")" || exit 1

FILES="README.md
report.md
hot/hottest-topics.md
trends/trends.md
analysis/blocked-causes.md
raw/01-msglog-index.md
raw/02-search-results.md
raw/03-tavily-answers.md
raw/04-browser-attempts.md
raw/05-search-partial.md
raw/06-toolcalls-full.md"

json_escape() {
  # minimal JSON string escape for the path names in this manifest
  printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'
}

{
  printf '{\n'
  printf '  "schema": "pentagi.research-manifest/v2",\n'
  printf '  "generated_at": "%s",\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf '  "generator": "./generate-manifest.sh",\n'
  printf '\n'
  printf '  "source": {\n'
  printf '    "system": "PentAGI",\n'
  printf '    "flow_id": 23,\n'
  printf '    "task_id": 20,\n'
  printf '    "trace_id": "a803fa7652b0607ea7e8b3b876b8b366",\n'
  printf '    "model": "MiniMax-M3",\n'
  printf '    "provider": "minimax",\n'
  printf '    "window_start": "2026-10-19T13:19:25Z",\n'
  printf '    "window_end": "2026-10-19T13:37:10Z",\n'
  printf '    "msglog_id_range": "3726-3751",\n'
  printf '    "msglog_count": 26,\n'
  printf '    "toolcall_id_range": "4077-4102",\n'
  printf '    "toolcall_count": 26,\n'
  printf '    "searchlog_id_range": "513-521",\n'
  printf '    "searchlog_count": 9,\n'
  printf '    "search_engine_counts": {"tavily": 7, "searxng": 2}\n'
  printf '  },\n'
  printf '\n'
  printf '  "counts": {\n'
  printf '    "searxng_results": 29,\n'
  printf '    "browser_attempts": 12,\n'
  printf '    "browser_failures": 12,\n'
  printf '    "valid_report_items_industry_media": 7,\n'
  printf '    "report_items_tieba": 0,\n'
  printf '    "report_items_social": 0,\n'
  printf '    "results_with_published_date": 0,\n'
  printf '    "hottest_topics": 5,\n'
  printf '    "trends": 10\n'
  printf '  },\n'
  printf '\n'
  printf '  "buckets": {\n'
  printf '    "hot":    { "dir": "hot/", "role": "current hottest topics" },\n'
  printf '    "trends": { "dir": "trends/", "role": "structural directions" },\n'
  printf '    "raw":    { "dir": "raw/", "role": "unprocessed retrieval evidence and call trace" },\n'
  printf '    "analysis": { "dir": "analysis/", "role": "why retrieval was blocked and how to get past it" }\n'
  printf '  },\n'
  printf '\n'
  printf '  "constraints": {\n'
  printf '    "web_search_and_fetch_only": true,\n'
  printf '    "no_port_scan_no_vuln_probe": true,\n'
  printf '    "no_captcha_bypass_no_proxy_rotation": true,\n'
  printf '    "all_items_from_retrieved_pages": true,\n'
  printf '    "unconfirmable_dates_marked": "日期未确认",\n'
  printf '    "memory_store_used_as_source": false\n'
  printf '  },\n'
  printf '\n'
  printf '  "files": ['

  first=1
  echo "$FILES" | while IFS= read -r f; do
    [ -f "$f" ] || { echo "missing file: $f" >&2; exit 1; }
    if [ "$first" -eq 1 ]; then first=0; else printf ','; fi
    printf '\n    { "path": "%s", "bytes": %s, "sha256": "%s" }' \
      "$(json_escape "$f")" "$(wc -c < "$f" | tr -d ' ')" "$(shasum -a 256 "$f" | cut -d' ' -f1)"
  done

  printf '\n  ]\n'
  printf '}\n'
} > source-manifest.json

echo "wrote source-manifest.json"

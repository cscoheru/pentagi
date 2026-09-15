# PentAGI Server Hardening Report

**Date**: 2026-09-15
**Operator**: Claude (assisted)
**Scope**: `pentagi-vps` (`207.57.125.162:22`), `intel-platform` fork branch
**Host specs**: 4C / 8G RAM / 88G disk + 4G swap, dedicated machine
**Pre-state**: 18 containers running, sum ~3.4 GiB used of 11.8 GiB total (host + swap)

## TL;DR

Resource hardening pass to prevent OOM-killer surprises under flow load. Four
operational risks addressed, all live on VPS, all verified after container
restart. One hidden bug (MinIO image) caught and fixed before it broke the
cluster.

| # | Change | Effect |
|---|---|---|
| 1 | `mem_limit` on 13 managed containers | Hard cap per container; OOM killer targets the runaway, not `pentagi` |
| 2 | `MAX_GENERAL_AGENT_TOOL_CALLS=30` | Bound agent loops; was relying on default 100 |
| 3 | ClickHouse TTL: 30d analytical / 14d event log | Bounded growth of observability stack |
| 4 | `OLLAMA_KEEP_ALIVE=-1` (was `5m`) | bge-m3 stays resident → embedding 0 reload latency |
| 5 | MinIO image pin `quay.io/minio/minio:latest` | Repo-default `minio/minio:RELEASE.2025-07-23T15-54-02Z` no longer pulls |

## Motivation

Pre-change steady-state measurements (idle, no active flow):

```
pentagi                96 MiB
ollama                 17 MiB   ← empty (KEEP_ALIVE=5m unloaded bge-m3)
langfuse-web         1023 MiB
langfuse-clickhouse   758 MiB
langfuse-worker       513 MiB
...
host available        4.3 GiB
```

**Worst-case projection** under active flow (3 terminals + Ollama embed
load + ClickHouse trace burst + scraper page render) summed to ~7.5 GiB
active, with realistic peak ~9.7 GiB — exceeding host RAM and forcing
swap in.

**Critical risk identified**: no container had `mem_limit` set. Under
memory pressure, kernel OOM-killer picks victims by `oom_score` (loosely
proportional to usage) — could pick `pentagi` itself, killing the API
and any in-flight flow with no graceful shutdown.

## Changes

### 1. Container `mem_limit` (commit `fbbc095`)

Added `mem_limit` to 13 of 15 managed containers. The two unmanaged
(`uptime-kuma`, `portainer_agent`) are started via direct `docker run`
and not in any compose file; intentionally skipped per operator decision
(Kuma serves as central monitoring, low risk).

| Container | Active peak | mem_limit | Headroom |
|---|---:|---:|---:|
| pentagi | ~700 MiB | **1.5 GB** | 2.1× |
| ollama | ~1.3 GiB (bge-m3) | **2.5 GB** | 1.9× |
| pgvector | ~200 MiB | **800 MB** | 4× |
| scraper | ~800 MiB | **1.2 GB** | 1.5× |
| flaresolverr | ~400 MiB | **800 MB** | 2× |
| searxng | ~200 MiB | **400 MB** | 2× |
| langfuse-web | ~1.1 GiB | **1.5 GB** | 1.4× |
| langfuse-worker | ~900 MiB | **1.5 GB** | 1.7× |
| langfuse-clickhouse | ~1.2 GiB | **2.0 GB** | 1.7× |
| langfuse-postgres | ~200 MiB | **400 MB** | 2× |
| langfuse-redis | ~50 MiB | **200 MB** | 4× |
| langfuse-minio | ~200 MiB | **500 MB** | 2.5× |
| pgexporter | ~10 MiB | **128 MB** | 12× |

Sum of limits (~13 GB) exceeds host RAM (8 GB). This is intentional and
safe: `mem_limit` is a hard cap, not a reservation. Containers running
below their limit consume only what they need; only the runaway gets
OOM-killed. The alternative (sum under host) requires reserving capacity
that isn't shared, which is wasteful.

**Files**: `docker-compose.yml`, `docker-compose-langfuse.yml`,
`docker-compose.override.yml` (VPS-side).

### 2. `MAX_GENERAL_AGENT_TOOL_CALLS=30`

Default in Go config: 100 (`backend/pkg/config/config.go: envDefault:"100"`).
VPS `.env` was empty → effectively 100.

Lowered to 30 to bound agent runaway loops. A normal pentest flow with
15 agents × 30 calls = 450 total tool calls max — more than enough for
legitimate work, but prevents infinite loops from consuming unbounded
LLM tokens and runtime.

Pair with `MAX_LIMITED_AGENT_TOOL_CALLS` (still default 25) for
limited-role agents.

### 3. ClickHouse TTL — 30d analytical / 14d event log

Langfuse v3 ClickHouse schema (5 trace/event tables, others are aggregates
or system):

```
observations           start_time   DateTime64(3)   PARTITION BY toYYYYMM(start_time)
traces                 timestamp    DateTime64(3)   PARTITION BY toYYYYMM(timestamp)
scores                 timestamp    DateTime64(3)   PARTITION BY toYYYYMM(timestamp)
event_log              created_at   DateTime64(3)
blob_storage_file_log  event_ts     DateTime64(3)
```

Applied DDL:

```sql
ALTER TABLE observations           MODIFY TTL toDateTime(start_time) + INTERVAL 30 DAY;
ALTER TABLE traces                MODIFY TTL toDateTime(timestamp)  + INTERVAL 30 DAY;
ALTER TABLE scores                MODIFY TTL toDateTime(timestamp)  + INTERVAL 30 DAY;
ALTER TABLE event_log             MODIFY TTL toDateTime(created_at) + INTERVAL 14 DAY;
ALTER TABLE blob_storage_file_log MODIFY TTL toDateTime(event_ts)   + INTERVAL 14 DAY;
```

**Retention rationale**:
- 30 days for trace/observation/score covers 2–3 nightly scan cycles
  plus a debug window for production issues
- 14 days for raw event log is sufficient because full payloads are
  also in MinIO (`langfuse-minio`); analytics tables are summaries
  derived from main tables and self-trim

**ClickHouse 24 type-strict gotcha**: TTL expression requires `DateTime`
or `Date`, NOT `DateTime64(3)`. Must wrap each time column with
`toDateTime()`. ClickHouse stores the rewritten form as
`toDateTime(col) + toIntervalDay(N)`.

**Idempotency**: the SQL is now mounted at
`./langfuse-clickhouse-init:/docker-entrypoint-initdb.d:ro` in
`docker-compose-langfuse.yml` so fresh deploys apply automatically.
Existing data required manual `docker exec clickhouse-client --multiquery`
once after deploy (data already populated, init scripts don't re-run).

### 4. Ollama warm resident

PentAGI uses Ollama exclusively for **embedding** (`EMBEDDING_PROVIDER=ollama`,
`EMBEDDING_MODEL=bge-m3`), not for LLM inference (LLM is remote MiniMax-M3).
Every agent observation triggers an embedding call → a 30-min flow can
hit embedding dozens to hundreds of times.

With `KEEP_ALIVE=5m`: each call after 5 min idle pays 3–8s model reload.
With `KEEP_ALIVE=-1`: bge-m3 stays in memory permanently (≈ 1.3 GB
permanent cost), all calls instant.

**Trade-off**: -1.3 GB permanent → available RAM drops from 4.3 GB to
~3.0 GB. Still healthy on 8 GB host, and acceptable because we're
heading toward nightly scans where this matters continuously.

### 5. Hidden bug: MinIO image

During deploy, `scp` of modified `docker-compose-langfuse.yml` would
have overwritten the VPS-side fix that swapped to `quay.io/minio/minio`.
The repo-default `minio/minio:RELEASE.2025-07-23T15-54-02Z` is no
longer pullable from Docker Hub (anonymous denied).

Caught before restart would have failed. Pinned repo-default to match
production: `quay.io/minio/minio:latest`. Future deploys and
`docker compose pull` from the repo are now safe.

## Verification

Post-restart baseline (`docker stats --no-stream` + `docker inspect
.HostConfig.Memory`):

```
CONTAINER                  USAGE           LIMIT
pentagi            92.23MiB / 1.5GiB           1.5GB
ollama              92.3MiB / 2.5GiB           2.5GB
pgvector            38.84MiB / 800MiB           800MB
scraper             302.5MiB / 1.2GiB           1.2GB
flaresolverr        80.53MiB / 800MiB           800MB
searxng             103.4MiB / 400MiB           400MB
langfuse-web        895.6MiB / 1.5GiB           1.5GB
langfuse-worker     354.2MiB / 1.5GiB           1.5GB
langfuse-clickhouse 391.4MiB / 2GiB           2.0GB
langfuse-postgres   35.57MiB / 400MiB           400MB
langfuse-redis      9.863MiB / 200MiB           200MB
langfuse-minio      126.9MiB / 500MiB           500MB
pgexporter          2.734MiB / 128MiB           128MB
```

- Host: 7.8 GiB used → **4.6 GiB available**, 68 MiB swap used
- All 10 main services `running`; `pgvector`/`langfuse-clickhouse`/`langfuse-minio` `healthy`
- ClickHouse TTL survived container restart (DDL in `system.tables`)
- Ollama env shows `OLLAMA_KEEP_ALIVE=-1`; bge-m3 will load on first embedding request

## Known Gaps / Out of Scope

| Item | Status | Reason |
|---|---|---|
| `uptime-kuma` / `portainer_agent` mem_limit | ⏸ skipped | Started via `docker run`, not in compose; operator decision |
| Terminal sandboxes (`pentagi-terminal-*`) mem_limit | ⏸ deferred | Dynamic spawn via PentAGI Docker SDK, not compose; would require Go code change for `DOCKER_MEMORY_LIMIT` env |
| Swap increase to 8 GB | ❌ rejected | Operator decision; current 4 GB swap is sufficient |
| Nightly scan sandbox concurrency cap | ❌ rejected | Operator decision; Kuma provides total monitoring |

## Pending User Actions (blocking fork #2 work)

1. **Cloudflare SSL fix**: `pentagi.3strategy.cc` returns HTTP 526.
   Fix: change CF SSL mode to **Full** (non-strict) for 10-second fix,
   or paste Origin Certificate into `pentagi-ssl` volume for proper
   end-to-end encryption.
2. **Admin password change**: `admin@pentagi.com / admin` is the
   default — must be changed now that the instance is public.
3. **PentAGI API Token**: generate via UI → Settings → API Tokens;
   needed to script nightly scan flow.

Without (1) and (2), the public endpoint is not safe for external
testing. Without (3), can't start nightly scan automation.

## Push Status

- Commit `fbbc095` made on local `intel-platform` branch
- `git push origin intel-platform` **rejected** (`403 Permission denied
  to cscoheru` on `vxcontrol/pentagi.git`)
- `origin` still points to upstream OSS, no personal fork remote added

To enable auto-push per the 2026-09-14 rule:

```bash
# 1. Fork on GitHub: github.com/vxcontrol/pentagi → click Fork
# 2. Add fork as remote
git -c http.proxy=127.0.0.1:7890 -c https.proxy=127.0.0.1:7890 \
    remote add fork https://github.com/<USER>/pentagi.git
# 3. Push
git -c http.proxy=127.0.0.1:7890 -c https.proxy=127.0.0.1:7890 \
    push -u fork intel-platform
```

## References

- Project: `/Users/kjonekong/projects/pentAGI`
- VPS deploy: `pentagi-vps` (207.57.125.162:22), deploy dir `/opt/pentagi`
- Obsidian notes: `~/Documents/Obsidian Vault/pendAPI/`
  - `pentAGI 部署完成.md` — initial deploy summary
  - `pentAGI 公网化.md` — public deployment + source host lockdown
  - `pentaig的能力范围.md` — PentAGI capability inventory
  - `fork化.md` — first fork (crt.sh) closure notes
- PentAGI docs: `backend/docs/` (17 architecture documents), `backend/pkg/templates/prompts/` (40 agent prompts)
- CLAUDE.md: contains 6-step "Adding a New Search Engine" template for fork #2

## Next Suggested Work

With hardening complete, the natural next steps are:

1. **Configure personal fork remote + push** (5 min, requires user GitHub action)
2. **CF SSL fix + admin password change + API token** (user actions, total ~5 min)
3. **Fork #2: FOFA searcher** (Chinese asset mapping, biggest kill-radius
   for domestic targets) — apply same 6-step CLAUDE.md template as crt.sh
4. **MCP client integration** (`mcp.nuclei.run_template` etc. design
   proposals exist in `examples/proposals/`) — make any MCP tool server
   plug-and-play (Shodan, nuclei, Burp)
5. **Nightly scan flow** (requires API token from #2): scheduler config
   that runs target recon daily against `puer.im` and `207.57.134.99`,
   writes reports, alerts via Langfuse cost threshold

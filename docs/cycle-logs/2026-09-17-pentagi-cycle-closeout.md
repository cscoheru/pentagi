# PentAGI Fork Cycle — Handoff Log (2026-09-17 → paused for macOS update)

> **Read this file first when you (Claude Code) start a new session in this repo.**
> It captures the full state of the cycle that was closed on 2026-09-17. The user paused the work to wait for a macOS system update; when the user resumes, they will tell you to continue from here.

## TL;DR

| | |
|---|---|
| Branch | `intel-platform` (local + pushed to `cscoheru/pentagi`) |
| Last commit | `44e7382 fix(flowReport): strip Docker multiplexed-stream header with stdcopy` |
| Deploy state | pentagi container on `pentagi-vps` (207.57.125.162) is live with `44e7382`; Langfuse public HTTPS at `https://langfuse.3strategy.cc`; CF AO Pulls enforced on `*.3strategy.cc`; fish-harness edge1 wrapper bound to `127.0.0.1:4001` only; Puer.im SPF+DMARC live |
| **Open follow-ups** | wrapper `/metrics` 500 (better-sqlite3 native binding bug, Tailscale-only); flowReport frontend button (15-min React add); `fbbc095` hardening selective push (predates the new commits); Langfuse change-password cosmetic bug |
| Resume command | `cd /Users/kjonekong/projects/pentAGI && git pull --rebase fork intel-platform && ./docs/cycle-logs/2026-09-17-pentagi-cycle-closeout.md#resume` |

## 1. Scope of this cycle

User asked for 4 sequential steps, each with go/no-go approval:

1. **Shodan InternetDB 验证** — verify the engine (just-deployed in this cycle) returns useful data for the Puer.im target.
2. **CF Authenticated Origin Pulls** — close Flow 7 finding F-02 (Origin IP bypass) by enforcing CF-origin mutual TLS.
3. **flow_report 视图** — fix the chronic "flow finished but no report shown" problem by adding a GraphQL query + (eventually) UI button to surface the markdown report that pentAGI already writes to its terminal-sandbox container at `/root/<title>_report.md`.
4. **修复 Puer.im Flow 7 findings** — F-01 (email auth), F-02 (covered by step 2), F-13 (port 4001 public exposure).

All four steps delivered. Cycle is **closed**.

## 2. Status matrix

### 2.1 Step 1 — Shodan InternetDB (✅)

- Engine `ShodanInternetDB` wired into `buildSearchEngines` and appended to `ModeAnswer` fallback chain.
- Engine returns `{ports, cpes, hostnames, tags, vulns}` for any IPv4/IPv6 query.
- Verified against `207.57.134.99` (Puer.im origin): returned nginx 1.18.0 CPE + `eol-product` tag + CVE-2021-23017 + CVE-2023-44487 + CVE-2025-23419. This data **directly validates** Flow 7 F-02/F-04 findings without any manual nmap.
- Commits:
  - `d5b0999 feat(searchers): add Shodan InternetDB engine (zero key, zero quota)`
  - `20260917_120000_add_shodan_internetdb_search_type.sql` goose migration adds `'shodan_internetdb'` to `SEARCHENGINE_TYPE` enum (Up/Down symmetric with the existing FOFA migration).

### 2.2 Step 2 — CF Authenticated Origin Pulls (✅)

- User enabled **Global toggle** in CF panel for `3strategy.cc` zone (applies to all `*.3strategy.cc` hosts).
- VPS-side:
  - Downloaded `https://developers.cloudflare.com/ssl/static/authenticated_origin_pull_ca.pem` to `/etc/nginx/ssl/cloudflare-authenticated-origin-pull-ca.pem` (2154 bytes, self-signed by `CloudFlare, Inc., OU = Origin Pull`).
  - Added `ssl_verify_client on; ssl_client_certificate /etc/nginx/ssl/cloudflare-authenticated-origin-pull-ca.pem;` to **both** server blocks: `/etc/nginx/conf.d/pentagi.conf` and `/etc/nginx/conf.d/langfuse.conf`.
  - `systemctl reload nginx` executed within 30 s of the CF toggle (no 526 outage window).
- Verified:
  - `curl https://pentagi.3strategy.cc/api/v1/info` → HTTP 200 in 152 ms (CF→origin works).
  - `curl https://langfuse.3strategy.cc/api/public/health` → HTTP 200 in 160 ms.
  - Direct VPS→443 with no client cert → HTTP 400 (rejected by nginx at TLS handshake).

### 2.3 Step 3 — flowReport GraphQL query (✅)

- New query in `pkg/graph/schema.graphqls`: `flowReport(flowId: ID!): String!`
- Resolver in `pkg/graph/schema.resolvers.go`:
  - Validates `flows.view` permission via existing helper.
  - `r.DB.GetFlowContainers(ctx, flowID)` → list of `[]Container`.
  - For each `ContainerStatusRunning`:
    - `r.DockerClient.ContainerExecCreate` + `ContainerExecAttach` with `find /root /tmp -maxdepth 4 -type f \( -name '*report*.md' -o -name '*assessment*.md' \) -size +100c ... | xargs -r ls -la | sort -k5 -n -r | head -1 | awk '{print $NF}'`
    - `stdcopy.stdCopy` (from `github.com/moby/moby/api/pkg/stdcopy`) to demux Docker's multiplexed stream — **critical**, see "gotchas" below.
    - Pick largest file path from stdout.
    - `cat` the file via Docker SDK.
    - Skip if < 200 bytes (orchestrator writes stubs before real report).
  - Return content (or `""` if no report found).
- Plumbing: `DockerClient docker.DockerClient` field on `Resolver`, wired through `NewGraphqlService(... dockerClient)` from `router.go`, using the existing client created in `cmd/pentagi/main.go:172`.
- Verified on flow 7: returns 60009-byte `puer_assessment_report.md` (the FULL Flow 7 report).
- Commits:
  - `faa1209 feat(graphql): add flowReport(flowId) query for terminal-sandbox reports`
  - `2a7f325 fix(flowReport): use Docker SDK instead of missing docker CLI`
  - `44e7382 fix(flowReport): strip Docker multiplexed-stream header with stdcopy`

### 2.4 Step 4 — Puer.im finding remediation (✅ partially)

| Finding | Fix | Commit |
|---|---|---|
| **F-02** Origin IP bypass | CF AO Pulls (step 2 above) | (config only) |
| **F-13** Public port 4001 with module-path disclosure | fish-harness commit `1658c57 ops(edge1): bind wrapper port 4001 to 127.0.0.1 (Tailscale-only)` — changed `6host-compose.edge1.yml` from `"4001:4001"` to `"127.0.0.1:4001:4001"`. Tailscale Funnel still routes via the container hostname (`harness-edge1-wrapper`) on the bridge network, so internal access is preserved. | (fish-harness repo, detached HEAD on `v1.2.0l.0`) |
| **F-01** No email auth (SPF/DKIM/DMARC/MX) | User added to `puer.im` zone (NOT `3strategy.cc`): `TXT @ = "v=spf1 -all"` + `TXT _dmarc = "v=DMARC1; p=reject;rua=mailto:fisher@3strategy.cc; aspf=s; adkim=s"`. Verified live via `dig @teagan.ns.cloudflare.com +short puer.im TXT` and `_dmarc.puer.im TXT`. | (DNS only) |
| F-13 better-sqlite3 500 bug | **NOT FIXED** — separate code-level bug. Fix is `docker exec harness-edge1-wrapper sh -c "cd /app/wrapper && npm rebuild better-sqlite3 && docker compose -f deploy/6host-compose.edge1.yml restart edge-wrapper"`. Only Tailscale users see the 500; public users no longer reach port 4001. | deferred |

## 3. Deploy state (current, 2026-09-17)

### VPS `pentagi-vps` (207.57.125.162)

```
pentagi:intel image   : 25 containers (17 main + 8 observability)
disk                   : 88 GB total, 62 GB used (72%)
RAM total              : 8 GB
notable usage:
  ollama               : 1.61 GiB / 2.5 GiB limit
  langfuse-web         : 871 MiB / 3 GiB limit
  langfuse-clickhouse  : 607 MiB / 2 GiB limit
  scraper              : 235 MiB / 1.2 GiB limit
observability (8 new)  : grafana + node-exporter + cadvisor + otel + victoriametrics + loki + jaeger + clickstore
  no mem_limit set on observability containers (planned next hardening cycle)
```

Public endpoints (all CF-proxied, AO Pulls enforced):
- `https://pentagi.3strategy.cc` — PentAGI Next.js frontend
- `https://langfuse.3strategy.cc` — Langfuse (password `PuerLangfuse2026!` for `admin@pentagi.com`, change-password flow has cosmetic bug — see gotchas)

VPS-side internal services:
- Langfuse web at `127.0.0.1:4000` (Next.js bound to 0.0.0.0:3000 inside container, HOSTNAME=0.0.0.0 in compose; see gotchas)
- SearXNG at `127.0.0.1:8888` (no API key, fully internal)
- Ollama serving `bge-m3` (1.2 GB) with `KEEP_ALIVE=-1` (kept loaded forever)
- ClickHouse TTL applied (`traces/observations/scores = 30 d`, `blob_storage_file_log/event_log = 14 d`)

### Puer.im (`puer-hk` 207.57.134.99)

- DNS: TXT `@` = `google-site-verification...` + `v=spf1 -all`; TXT `_dmarc` = `v=DMARC1; p=reject;...`; MX none.
- `harness-edge1-wrapper` port 4001 bound to **127.0.0.1 only** (was 0.0.0.0; committed as fish-harness `1658c57`).
- Port 8443 (`pentagi.3strategy.cc` nginx → pentagi container) unchanged.

## 4. Open follow-ups (priority-ordered)

| # | Item | Effort | Why not done |
|---|---|---|---|
| 1 | **wrapper `/metrics` 500 fix** (better-sqlite3 rebuild + restart) | 5 min | Cosmetic — Tailscale-only path; public exposure already mitigated. User deferred. |
| 2 | **flowReport frontend button** (React + marked.js in flow detail page) | 30 min | Backend works; α option (simple download) was approved. User can use the GraphQL endpoint directly. |
| 3 | **observability mem_limit hardening** (8 services have no mem_limit, show as host total 7.751 GiB) | 15 min | Deferred to next hardening cycle. |
| 4 | **`fbbc095` hardening selective push** (predates the 2 new commits; needs rebase or `--force-with-lease`) | 10 min | Not critical — `fbbc095` is on detached branch state; current fork HEAD is `44e7382`. |
| 5 | **Langfuse change-password bug** (asks for current password but rejects the right one) | 30 min | Cosmetic — workaround is direct DB UPDATE: `UPDATE users SET password = '<bcrypt hash>', updated_at = NOW() WHERE email = 'admin@pentagi.com'`. |
| 6 | **FOFA searcher** (currently in repo but no API key wired) | decision | User said FOFA free-tier is annoying. Code stays in `60de349` for when paid key is acquired. |
| 7 | **Fork #3 candidate** | research | crtsh (done) + FOFA (done) shipped. Next candidates: Hunter? Quake? ICP备案? WhoisXMLAPI? |

## 5. Resume commands (next session)

After macOS update, when user says "继续", do this sequence:

```bash
cd /Users/kjonekong/projects/pentAGI

# 1. Sync with fork
git fetch fork && git status
# If "Your branch is behind", rebase:
# git pull --rebase fork intel-platform

# 2. Verify VPS state
ssh pentagi-vps 'docker ps --format "table {{.Names}}\t{{.Status}}" | head -30'
ssh pentagi-vps 'curl -sk https://localhost:8443/healthz -w "\nhealthz HTTP %{http_code}\n"'

# 3. Verify flowReport still works (no regression)
python3 ~/.claude/projects/-Users-kjonekong/pentagi-mint-jwt.py 2>/dev/null > /tmp/pentagi_jwt.txt
ssh pentagi-vps "JWT=\$(cat /tmp/pentagi_jwt.txt); curl -sk -X POST 'https://localhost:8443/api/v1/graphql' -H 'Content-Type: application/json' -H \"Authorization: Bearer \$JWT\" -d '{\"query\":\"{ flowReport(flowId: 7) }\"}' --max-time 30 | python3 -c 'import json,sys; d=json.load(sys.stdin); r=d.get(\"data\",{}).get(\"flowReport\",\"\"); print(\"len=\"+str(len(r)))'"
# Expect: len=60009

# 4. Pick up follow-up #1 (better-sqlite3) OR #2 (frontend button) based on user preference
```

## 6. Gotchas (things future-me should know)

### 6.1 Next.js 16 production binds to `$HOSTNAME`, not `HOST`

`f99d1c6` documented the bug but never fixed it. The fix is `HOSTNAME: 0.0.0.0` in the container env, not `HOST: 0.0.0.0`. Verified 2026-09-17 with `/proc/net/tcp` inside the container.

### 6.2 pentagi container has no `docker` CLI on PATH

`/var/run/docker.sock` IS mounted, but `docker` binary is not. Any new feature that needs to exec into terminal-sandbox containers **must** use the Docker SDK (`pkg/docker/DockerClient`), not `os/exec.Command("docker", ...)`. The first iteration of flowReport failed silently on this; subsequent fixes plumbed the SDK through Resolver.

### 6.3 Docker `ContainerExecAttach` returns multiplexed frames

Each frame is `[1-byte type][3 pad][4-byte BE size][payload]`. `io.ReadAll` on the attach reader returns raw bytes WITH the header prepended. Use `stdcopy.StdCopy(&stdout, &stderr, attachReader)` to demux. The flowReport resolver's second-attempt failure (path became `"\x01\x00\x00\x00\x00\x00\x00 /root/puer_assessment_report.md"`) was exactly this bug.

### 6.4 docker-proxy with `-use-listen-fd` bypasses iptables

docker-proxy processes on puer-hk (e.g. port 4001) use systemd socket activation and bypass INPUT/FORWARD chains. Firewall rules don't work on them. **The right fix is `ports: "127.0.0.1:4001:4001"` in docker-compose, not iptables.** See fish-harness commit `1658c57` for the verified pattern.

### 6.5 fish-harness port 4001 was a public leak

Before this cycle, `harness-edge1-wrapper` exposed `0.0.0.0:4001` publicly, allowing anonymous probes to `/metrics` which leaked `/app/wrapper/` and `better-sqlite3` module path on 500 errors (CWE-209). Flow 7 reported it as F-13 (CVSS 7.5). Now fixed to `127.0.0.1:4001:4001`; Tailscale path preserved. The underlying better-sqlite3 500 still exists (Tailscale users see it) — needs `npm rebuild` to fully resolve.

### 6.6 Langfuse password rotation requires DB UPDATE

The `LANGFUSE_INIT_USER_PASSWORD` env var is only honored on **first** container startup. Subsequent restarts ignore it. To rotate: bcrypt-hash new password locally, then `UPDATE users SET password = '<hash>', updated_at = NOW() WHERE email = 'admin@pentagi.com'`. The UI change-password flow has a bug where it rejects the correct current password; DB UPDATE is the working path until that's fixed.

### 6.7 Memory file naming convention

Memory files at `~/.claude/projects/-Users-kjonekong/memory/pentagi-*.md` follow the pattern `pentagi-<topic>-<date>.md`. Index is `MEMORY.md` at the same path. Read both at session start.

## 7. Memory pointers

Cross-references that extend the context of this handoff:

- `~/.claude/projects/-Users-kjonekong/memory/pentagi-deployment.md` — deployment conventions (canonical)
- `~/.claude/projects/-Users-kjonekong/memory/pentagi-verify-2026-09-17.md` — same-day verification (17 containers, TTL, etc.) — done earlier this cycle
- `~/.claude/projects/-Users-kjonekong/memory/pentagi-intelplatform-2026-09-17.md` — full Step 1–4 deploy log + 3-attempt story for flowReport
- `~/.claude/projects/-Users-kjonekong/memory/pentagi-hardening-2026-09-15.md` — predecessor hardening cycle (commit `fbbc095`)
- `~/.claude/projects/-Users-kjonekong/memory/pentagi-langfuse-activation.md` — Langfuse install gotchas
- `~/.claude/projects/-Users-kjonekong/memory/pentagi-langfuse-cloud-decision.md` — why we self-host Langfuse, not Cloud
- `~/.claude/projects/-Users-kjonekong/memory/pentagi-api-auth-and-flow-control.md` — JWT mint + flow operation patterns

## 8. Active branch state

```
local intel-platform : 44e7382 (HEAD)
remote fork/intel-platform : 44e7382 (synced, 5 commits ahead of upstream origin)

44e7382 fix(flowReport): strip Docker multiplexed-stream header with stdcopy
2a7f325 fix(flowReport): use Docker SDK instead of missing docker CLI
faa1209 feat(graphql): add flowReport(flowId) query for terminal-sandbox reports
d5b0999 feat(searchers): add Shodan InternetDB engine (zero key, zero quota)
d91574b fix(langfuse): bind Next.js 16 to 0.0.0.0 via HOSTNAME (HOST= had no effect)

# Pre-cycle baseline (still upstream):
0c1328d ops(.env.example): document TAVILY_API_KEY signup flow
9d18574 ops(searxng): drop limiter.toml — schema mismatch caused restart loop
f99d1c6 ops(langfuse): document Next.js 16 HOST env workaround (still broken)  ← the misdiagnosis
fbbc095 (detached in fish-harness; pre-intel-platform hardening commit)  ← NOT pushed to fork
```

**Push protocol** (CLAUDE.md iron rule):
```bash
git -c http.proxy=127.0.0.1:7890 -c https.proxy=127.0.0.1:7890 push fork intel-platform
```

---

**Cycle 状态**: ✅ CLOSED on 2026-09-17. Awaiting user signal after macOS update.

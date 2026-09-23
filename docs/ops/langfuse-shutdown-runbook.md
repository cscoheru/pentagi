# Langfuse Shutdown Runbook

> **When to use**: VPS is back online after a downtime incident, and we want to permanently remove the Langfuse observability stack to reclaim ~6 GB of RAM on the 8 GB VPS.

## Why

Langfuse (LLM trace analytics) is **opt-in, not required** for pentAGI. The pentagi backend talks to it via OTLP exporter as best-effort — if the Langfuse endpoints are unreachable, pentagi does not crash; it just stops emitting traces. So removing Langfuse is a pure resource win with no functional regression on pentest flows.

| Container | mem_limit | Role |
|---|---|---|
| `langfuse-clickhouse` | 2.0 GB | trace storage (columnar) |
| `langfuse-web` | 1.5 GB | Next.js dashboard |
| `langfuse-worker` | 1.5 GB | async ingestion worker |
| `langfuse-postgres` | 400 MB | org/project/users metadata |
| `langfuse-minio` | 500 MB | S3-compatible blob store (events/media) |
| `langfuse-redis` | 200 MB | queue + cache |
| **Total reclaimable** | **~6.1 GB** | |

The 8 GB VPS previously ran pentagi + observability + langfuse with **~0 GB headroom**, which caused repeated OOM-driven crashes (see cycle closeout `2026-09-17-pentagi-cycle-closeout.md`). Stopping Langfuse + observability leaves plenty of room for pentagi core.

## Stop sequence (run after VPS comes back online)

SSH in first to confirm reachability, then run as root:

```bash
ssh pentagi-vps
cd /opt/pentagi                              # or wherever the repo lives on VPS

# 1. Soft stop — keeps named volumes so we can resurrect later if we change our mind
docker compose -f docker-compose-langfuse.yml down

# 2. Verify the 6 containers are gone
docker ps -a --format '{{.Names}}\t{{.Status}}' | grep -E 'langfuse-(worker|web|clickhouse|minio|redis|postgres)' || echo "OK: no langfuse containers running"

# 3. Confirm pentagi still healthy (Langfuse is best-effort, pentagi must not depend on it)
curl -sk https://127.0.0.1:8443/api/public/health | head -c 200
# expect: 200 OK with JSON body

# 4. Confirm Langfuse port 4000 is now unbound
ss -ltn | grep -E ':4000\b' || echo "OK: port 4000 free"

# 5. Show post-stop RAM headroom
free -h | head -3
```

## Permanent disable (so `up` does not resurrect them)

The deploy command used to bring up the stack was:

```bash
docker compose -f docker-compose.yml -f docker-compose-observability.yml -f docker-compose-langfuse.yml up -d
```

Drop the `-f docker-compose-langfuse.yml` flag on future invocations:

```bash
docker compose -f docker-compose.yml -f docker-compose-observability.yml up -d
```

To keep the change discoverable for the next operator, comment out the Langfuse env block in `/opt/pentagi/.env` (or `pentagi-vps:/opt/pentagi/.env`) so it is obvious why the keys are not being used:

```bash
# 2026-09-23: Langfuse stack disabled (see docs/ops/langfuse-shutdown-runbook.md)
# LANGFUSE_BASE_URL=
# LANGFUSE_PROJECT_ID=
# LANGFUSE_PUBLIC_KEY=
# LANGFUSE_SECRET_KEY=
# LANGFUSE_LISTEN_PORT=
```

Even though pentagi tolerates unreachable Langfuse, leaving the keys set creates dead config — commenting them out signals intent to the next reader.

## Rollback (if we ever want Langfuse back)

The named volumes (`langfuse-postgres-data`, `langfuse-clickhouse-data`, `langfuse-clickhouse-logs`, `langfuse-minio-data`) are preserved by `down` without `-v`. To resurrect:

```bash
cd /opt/pentagi
docker compose -f docker-compose-langfuse.yml up -d
# wait ~60s for langfuse-web to finish init (langfuse-specific V3 R3 readiness trap)
curl -sk http://127.0.0.1:4000/api/public/health
```

## Out of scope (intentionally not changed)

- **Observability stack** (`docker-compose-observability.yml` — VictoriaMetrics / Loki / Jaeger / Grafana): currently disabled (`memory: pentagi-langfuse-local` notes 6 services defined but only observability/langfuse partially up). Re-evaluate separately if tracing is actually needed.
- **Pentagi backend / pgvector / scraper**: untouched.
- **API auth / LLM provider config**: untouched.

## Verification checklist before closing this runbook

- [ ] `docker ps` shows zero langfuse-prefixed containers
- [ ] `pentagi` container status is `Up` (not `Restarting`)
- [ ] `curl https://127.0.0.1:8443/api/public/health` returns 200
- [ ] `free -h` shows at least 3 GB available
- [ ] `pentagi.3strategy.cc` health endpoint returns 200 from the public internet
- [ ] No OOM entries in `journalctl -k --since today` after 1 hour of normal use
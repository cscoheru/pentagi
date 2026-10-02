# Task Run Convergence Guards (P0)

**Date**: 2026-10-01
**Scope**: `backend/pkg/controller/task.go`, `backend/pkg/providers/performer.go` (+ 2 new test files)
**Commit**: `0fbb318` on `intel-platform` → `cscoheru/pentagi` (P0 body)
**Follow-up**: uncommitted; this revision + the audit-gap fixes described under "What changed"
**Review**: 4 specialist passes (security / maintainability / testing / adversarial) + fix-first pass
**Codex audit**: `docs/task-convergence-2026-10-01-audit.md` — **Conditional Pass**
**Audit status**: §4 decisions recorded below; §2 invariant 5 remains **blocking** (see D1)

## TL;DR

Two research flows hung without ever reporting an outcome: flow 17 ran past 51
minutes and was still `running` when observed, having accumulated 268 subtasks
that were planned, deleted, and re-planned indefinitely. P0 adds a wall clock
budget and a subtask cap so a task run reaches a terminal state and writes a
report within a bounded time.

The first implementation of those guards was **almost entirely dead code** in the
scenario it targeted: every path that actually spends the budget parked the task
in `Waiting` and skipped the finalizer. Review caught it; it is now fixed and
covered by loop-level tests.

The Codex audit accepts the direction and makes the delivery **conditional** on
three things: a budget that actually bounds single calls, an output path honored
as a contract, and honest forced-termination semantics. Decisions on all three
are recorded under "Architecture decisions" below. D1 and D3 are now implemented;
D2's truncated marker and D4 remain open, so this is still a P0 repair iteration
and **not** a complete delivery.

| Guard | Budget | On exhaustion |
|---|---|---|
| Task wall clock | 15 min | report on what was collected, mark run truncated |
| Subtask wall clock | 5 min | force subtask terminal, count as a failure |
| Subtasks per run | 4 | report and stop re-planning, **only if work remains queued** |
| Consecutive subtask failures | 2 | stop the run |
| Single LLM call | 240 s (`LLM_CLIENT_TIMEOUT`, **D1**) | — searches keep the 600 s bound |

## Motivation

Observed on flow 17 (research task, MiniMax primary → lilith `qwen2.5:7b`
fallback):

- 51+ minutes of wall clock, status still `running`, never terminal
- 268 subtasks; `RefineSubtasks` deletes every `Created` subtask and re-plans
  unconditionally, so the queue never drains
- 46 tool calls, 8 of them `search` (2416 s total, 302 s average per call)
- subtask 268's result **hallucinated** a successful write to
  `/tmp/lotus_fragrance_report.md`; `flowFiles` returned 0 — the file never existed
- `subtaskWorker.Run` returns `nil` even when the chain fails, so the caller only
  learns the outcome by reading the status back
- `stopFlow` returns a false success

The user's standing requirement: *"如果一件事总是无法收敛，就算过程再好也没意义"* —
a process that cannot converge is worthless no matter how good it looks.

## Architecture decisions (audit §4)

The audit requires each of these to be chosen and reasoned about **before** the
next implementation round, and forbids CC from treating them as free TODOs. They
are recorded here as change notes; the two that need code are marked accordingly.

### D1 — Budget coherence: split the LLM bound from the search bound

**Facts established in the code.**

- `HTTPClientTimeout` is `env:"HTTP_CLIENT_TIMEOUT" envDefault:"600"`
  (`pkg/config/config.go:257`); the comment at `:256` states it covers *"LLM
  providers, search tools, etc."*
- `system.GetHTTPClient` (`pkg/system/utils.go:88`) builds **one**
  `http.Client{Timeout: HTTPClientTimeout}` and every consumer takes it from there
- Those consumers include all twelve searchers under `pkg/tools/searchers/`
  (tavily, traversaal, sploitus, crtsh, perplexity, searxng, google, fofa,
  firecrawl, duckduckgo, shodan_internetdb), plus `pkg/providers/embeddings/`
  and the LLM adapters
- `web_search` is a single tool that runs a `fallbackStrategy` **chain** across
  engines, so one tool call can be several engine calls. Observed: 8 search
  calls, 2416 s total, **302 s average**

**Consequence.** The audit's preferred option — a 90 s single-call cap — cannot be
applied to that shared client. A 90 s `http.Client.Timeout` would abort every
search mid-chain, which is exactly the capability the research task exists to
test. Shrinking the shared timeout trades a convergence bug for a retrieval bug.

**Decision.** Introduce a **separate** bound instead of shrinking the shared one.

- new `LLMClientTimeout` (`LLM_CLIENT_TIMEOUT`, default **240 s**), applied only
  when building LLM provider clients
- `HTTPClientTimeout` (600 s) retained for searchers and other external tools
- `maxTaskRunDuration` 15 min / `maxSubtaskRunDuration` 5 min unchanged

This is the audit's preferred branch (§4.1, *"优先实现 90 秒单次 agent/provider
调用上限"*) applied where it can actually hold. The three bounds then nest —
240 s per LLM call < 5 min per subtask < 15 min per run — which is what §2
*"预算边界真实有效"* asks for.

**Why not the fallback branch** (raise the run budgets and back it with load-test
evidence): that lets one hung chain own the whole run and reintroduces the flow-17
shape. The load-test evidence would also need a live provider under load, which is
a separate exercise from this fix.

**Why 240 s and not the audit's 90 s.** The number changed after this decision was
recorded, on evidence found while implementing it. `Provider.CallEx` and
`CallWithTools` both take a `streamCb streaming.Callback`
(`pkg/providers/provider/provider.go:110-122`): **LLM calls are streamed**, and
`http.Client.Timeout` bounds the whole response body read — that is, the entire
generation, not one round trip. A 90 s timeout would cut every long answer off
mid-stream, and this product's actual use is generating a research report, which
*is* a long streaming answer. Shrinking the shared client traded a convergence bug
for a retrieval bug; a 90 s LLM client would trade it for a truncation bug.

240 s = the 5 min subtask budget minus 60 s of framework overhead, so one call
still cannot own a subtask. **The user chose this value explicitly**; it is not a
number picked here. The audit's 90 s is preserved above as the reasoning it came
from rather than silently overwritten.

**Recorded risk.** 240 s may still truncate a very long generation. It is
configurable through `LLM_CLIENT_TIMEOUT` and documented in
`backend/docs/config.md`. If a research run shows healthy LLM calls being cut off,
raise that value — do not re-blend it with the search timeout.

**Status: IMPLEMENTED.** `config.LLMClientTimeout` (default 240) feeds
`system.GetLLMClient`, used by 10 provider call sites (9 adapters +
`embeddings/embedder.go`); `system.GetHTTPClient` is unchanged and still serves all
11 searchers. The two timeouts being genuinely separate is pinned by
`TestGetLLMClient_SharesTransportShapeWithHTTPClient`; the default/custom/zero
values by `TestNewConfig_LLMClientTimeout`.

### D2 — Forced-termination semantics: keep `Success = false`, mark truncation

**Facts.** `tools.Done` (`pkg/tools/args.go:104`) carries `Success` / `Result` /
`Message`; `tools.TaskResult` mirrors it. `synthesizeDoneCall` already writes
`Success: false`.

**Decision.** Adopt audit §4.2 as written: **keep `Success = false`** and express
partial completion with an explicit marker. Do **not** move to *"content = success"*.

- when `stopReason != ""`, `finalizeRun` forces `Success = false`, sets `Message`
  to the stop reason when empty, and marks the `Result` as truncated
- the marker goes in the result text rather than as a fourth field on
  `tools.Done`: that struct is the model-facing tool schema, and adding a field
  changes what every provider sees
- a UI `partial` / `truncated` badge derives from `Success == false && Result != ""`
  plus the marker, so no schema change is needed to separate *"failed with
  findings"* from *"failed with nothing"*

**Why.** In a pentest product `Success: true` is an all-clear. Flow 17's subtask
268 claimed success while writing a file that never existed — a false positive
there is worse than a pessimistic false negative. The audit's wording is adopted
without reinterpretation: *"不得直接改为'有内容就成功'"*.

**Status: PARTIALLY IMPLEMENTED.** `Success` is already forced false and
`Message` already carries the stop reason (tested by
`TestFinalizeRunStopReasonOverridesSuccess`). The explicit truncated marker in
the result text is not yet written.

### D3 — Output contract: write the specified path and keep the DB result

**Facts.** `tools.Done` has no output-path field. The plan's target
`/tmp/lotus_fragrance_report.md` is a path **inside the sandbox container**. The
read path is `flowFiles` (tar export, `pkg/tools/tools.go:706`); the write path is
the sandbox terminal (`TerminalAction`, `pkg/tools/args.go:111`). In flow 17 the
model claimed the write and `flowFiles` returned 0.

**Decision.** Adopt audit §4.3 as written: the output path is a **task contract**.

- the contract is **structured**: `Task.output_path` (nullable TEXT), carried
  through GraphQL (`createFlow` / `putUserInput` `outputPath`) and REST
  (`output_path`) and validated at the API boundary
- on finalize the framework writes the report to that path **and** keeps the DB
  result; the file content is the stored result text, byte for byte
- a write failure is reported explicitly in the terminal result and `Message`,
  never silently absorbed, and forces `Success = false`
- the DB result stays authoritative, so a sandbox write failure cannot lose the
  report — the file is a second channel, not the only one

**Why not DB-only:** the user's original task asked for a md file; a DB row is not
that deliverable. **Why not file-only:** the sandbox is ephemeral and `flowFiles`
has already proven it can return 0.

**Explicitly not chosen:** fabricating the file outside the platform. The
observation constraint forbids the author of this change from producing the
research report, and DoD requires that `/tmp/lotus_fragrance_report.md` never
exist unless the platform wrote it.

**Deviations from the decision as first written.** Both are recorded rather than
absorbed, per audit R3-B4.

1. *"when the task was given an explicit output path"* is no longer the trigger.
   The framework always writes — to the declared path, or to the canonical
   `/root/task-<taskID>_report.md` when none was declared. The observed failure was
   a run that claimed a report and left no file anywhere; an undeclared path must
   not reproduce that. The user chose this.
2. The write does **not** go through the sandbox terminal tool. `Tool`
   (`pkg/tools/tools.go:75-78`) exposes only `Handle` / `IsAvailable`, so
   `WriteFile` / `ReadFile` are unreachable from `pkg/controller`. The write uses
   the mechanism `pushResourcesToContainer` already uses — `flowfiles` tar +
   `docker.CopyToContainer` — plus an `exec cat` read-back that compares the file
   with what was written. No new cross-package API was introduced.

**Also implemented (the third layer of the F5 gap):** every result is mirrored on
the host under `flowfiles.FlowContainerDir`, and `flowReport` falls back to that
mirror when no container is running, taking the larger of the two copies. Without
it a report written into a sandbox that has since stopped is unreadable — the same
deliverable as no report at all.

**Status: IMPLEMENTED.** Migration → sqlc → GraphQL/REST → controller pipeline →
API-boundary validation → `finalizeRun` write → `flowReport` fallback. Covered by
12 tests in `pkg/controller/task_output_test.go`, 7 in
`pkg/flowfiles/report_mirror_test.go`, and 4 in `pkg/graph/flow_report_test.go`.

### D4 — Flow-level convergence: separate design

Audit §4.4 is adopted as written. Flow-level budget, a consecutive-error counter
that sets `FlowStatusFailed`, genuinely cancelling the context in `stopFlow`, and
sweeping leftover `Created` / `Waiting` subtask rows are a **separate design** and
are not smuggled into this fix. Nothing in this change set touches `flow.go` state
semantics.

**Status: OUT OF SCOPE BY DECISION.**

## Audit conformance map (audit §2)

Each invariant against its code path and the test that pins it.

| # | Invariant | Code path | Test | Status |
|---|---|---|---|---|
| 1 | 终态必达 — terminal state within task budget + one finalizer | `Run` → `finalizeRun`; status written from `context.Background()` with one retry | `TestRunFinalizesWhenSubtaskConsumesTheRunBudget`, `TestRunBudgetStopSurvivesAParkedTask` | **Held** |
| 2 | 结果必达 — result write never gated on the LLM | `finalizeRun` writes `SetResult` before `SetStatus`; stub on provider error or nil | `TestFinalizeRunWritesTerminalStatusWhenReporterFails`, `TestFinalizeRunSurvivesANilResult` | **Held** |
| 3 | 不得伪造成功 | `synthesizeDoneCall` → `Success: false`; `stopReason != ""` forces false | `TestSynthesizeDoneCallDoesNotClaimSuccess`, `TestFinalizeRunStopReasonOverridesSuccess` | **Held** |
| 4 | 停止原因可追踪 | distinct `stopReason` per cause: task budget / subtask budget / consecutive failures | `TestRunReportsTruncatedPlanAsFailure`, `TestRunStopsAfterConsecutiveFailures` | **Held** (vocabulary fixed this round) |
| 5 | 预算边界真实有效 | `system.GetLLMClient` (240 s) for 10 provider sites vs `system.GetHTTPClient` (600 s) for 11 searchers; the LLM bound is strictly inside the 5 min subtask budget | `TestGetLLMClient_SharesTransportShapeWithHTTPClient`, `TestNewConfig_LLMClientTimeout` | **Held** (D1) |
| 5b | 交付物可取回 — 结果由框架落盘并可验证 | `finalizeRun` → `TaskResultSink.WriteResult` (host mirror + container copy + read-back); `flowReport` host fallback | `TestFinalizeRunWritesResultToDeclaredOutputPath`, `TestFinalizeRunWriteFailureIsReportedAndForcesFailure`, `TestFlowReportFallsBackToHostMirrorWhenNoContainerIsRunning` | **Held** (D3) |
| 6 | 区分预算超时与外部取消 | `budgetExhausted(ctx, runCtx)`; `errRunBudget` kept off the `DeadlineExceeded` chain; `handleInterrupting` only on cancel/deadline | `TestRunSubtaskBudgetErrorIsNotDeadlineExceeded`, `TestRunSubtaskForeignDeadlineIsNotABudgetStop`, `TestRunExternalCancelParksInsteadOfFinalizing`, `TestBudgetExhaustedRequiresAHealthyParent` | **Held** (strengthened this round) |
| 7 | 终态写入有界 | forced write on its own bounded context; `SetStatus` retried once; errors logged, not swallowed | `TestRunSubtaskTerminalWriteUsesLiveContext` | **Held** |
| 8 | 完成计划即成功 | `planHasRemainingSubtasks` gates the subtask-cap `stopReason` | `TestRunReportsExhaustedPlanAsSuccess`, `TestRunReportsTruncatedPlanAsFailure` | **Held** |
| 9 | 禁止重复排队 | forced `SubtaskStatusFailed`; `subtask.go:95-100` resets only `Running` → `Created` on load | — | **Held behaviourally, no dedicated test** |
| 10 | 不越过 `done` 边界 | `synthesizeDoneCall` gated on `executor.IsBarrierFunction(tools.FinalyToolName)`; only `GetPrimaryExecutor` registers it | `TestSynthesizeDoneCallIDIsFrameworkNamespaced`, registry coverage | **Held** |

All ten hold. Row 5b is not one of the audit's ten — it is the deliverable
invariant D3 adds, because a report nobody can retrieve is not a result either.

## Observability (audit §3.4)

| Required | Where | Status |
|---|---|---|
| task ID | all `finalizeRun` / `runSubtask` log fields | present |
| subtask ID | `runSubtask` budget and terminal-write logs | present |
| budget type | `budget: task_run` / `budget: subtask_run` + `budget_limit` | present |
| elapsed | `elapsed` on the `task_run` stop and finalize lines | present (this round) |
| stop reason | `stop_reason` on stop and finalize lines | present (this round) |
| final status | `final_status` on the finalize line | present (this round) |
| empty result | `empty_result` field + dedicated Warn before the stub | present (this round) |
| result write failure | `failure: result_write` | present (this round) |
| file write failure | `failure: file_write` on its own error line and on the finalize line's `failure` field, with `output_path` and `host_path` | present (D3) |
| provider timeout | `failure` absent; cause preserved via `WithError(err)` and `(cause: %v)` on the error | present (this round) |
| external cancel | parks via `handleInterrupting`; **no dedicated log line** | partial |

One gap remains: the external-cancel path parks without a line that says a user
decision caused it. The file-write marker is no longer a gap.

## Definition of Done (audit §5)

| # | Gate | Status |
|---|---|---|
| 1 | 架构不变量逐条由代码路径和测试证明 | **met** — 10/10 of the audit's invariants, plus the D3 deliverable invariant (row 5b) |
| 2 | 预算数值、超时来源和终止语义已明确，不再存在 600 秒/5 分钟冲突 | **met** (D1) — 240 s for LLM, 600 s for search, both configurable and documented |
| 3 | 指定输出路径和数据库结果的行为一致且有失败测试 | **met** (D3) — the file holds exactly the stored result; a write failure appends `failure: file_write` and forces `Success = false` |
| 4 | 新增测试覆盖运行循环和异常路径 | **met** — 33 controller + 7 provider + 7 flowfiles + 4 graph tests; 13 of them run-loop level |
| 5 | 定向测试、构建、vet、格式检查和全量测试结果已记录，既有失败已单独标注 | **met** — see Test evidence |
| 6 | 未创建或伪造 `/tmp/lotus_fragrance_report.md` 等研究观察产物 | **met** — no such file written by this change |
| 7 | 变更范围、未实现项和剩余风险均写入交付说明 | **met** — this document |

All seven gates are met. Gates 2 and 3 were the two that held this at Conditional
Pass; both are now implemented. What remains open is deliberately out of the
audit's gate set: D2's truncated marker and D4's flow-level convergence.

## What changed

### `pkg/controller/task.go`

1. **Budgets and caps** (`maxTaskRunDuration`, `maxSubtaskRunDuration`,
   `maxSubtasksPerRun`, `maxConsecutiveSubtaskFailures`). All unexported. The two
   durations are `var`, not `const`, so tests can tighten them to milliseconds.

2. **Every budget stop routes through `finalizeRun`.** The run loop checks the
   budget after each subtask and before `RefineSubtasks`, because both of those
   paths used to park the task in `Waiting` on an expired context and return.

3. **`finalizeRun` writes the terminal status from its own `context.Background()`
   and is never gated on the reporter call.** If `GetTaskResult` fails or returns
   nothing, a stub result explaining the absence is written instead. The result
   and status are written before the report message; only the status write is
   retried.

4. **An exhausted plan reports success.** `stopReason` for the subtask cap is set
   only when `GetTaskPlannedSubtasks` still returns rows. A plan of exactly 4
   subtasks that all succeed is a completed run.

5. **Genuine run errors park in `Waiting`** instead of leaving the row in
   `Running`, which nothing else ever writes a status for.

6. **`runSubtask` distinguishes our budget from a foreign deadline.** A provider
   with its own inner timeout returns the same `context.DeadlineExceeded`
   sentinel without breaching the subtask budget; labelling that as a budget stop
   would end the run for a timeout it never caused. The forced terminal write
   gets its own bounded context and its error is logged, not discarded.

7. **`budgetExhausted(ctx, runCtx)` separates a spent budget from a user Stop.**
   A cancelled parent makes `runCtx.Err()` non-nil too, so the bare check recorded
   a user cancel as *"task run budget is exhausted"* and finalized a task that is
   meant to stay resumable. The same conflation existed on the `RefineSubtasks`
   error path. *(this revision)*

8. **The provider cause is preserved on a budget stop.** The forced-terminal error
   message now carries `(cause: %v)` and the log carries `WithError(err)`.
   `errRunBudget` stays the **only** sentinel on the Go error chain — wrapping the
   cause would make `errors.Is(err, context.DeadlineExceeded)` true and hand the
   discrimination back to call-site ordering. *(this revision)*

9. **Termination logs carry the full audit field set** — `task_id`, `budget`,
   `budget_limit`, `elapsed`, `stop_reason`, `final_status`, `empty_result` — plus
   distinct `failure` markers for result-write, status-write and report-message
   errors. *(this revision)*

### `pkg/providers/performer.go`

10. **No-progress detector.** After 3 consecutive model replies with no tool calls
    the chain finalizes itself with a synthesized `done` call carrying the
    accumulated text, instead of handing each reply to the reflector and burning
    the iteration budget. This is the qwen2.5:7b fallback path: a small model that
    emits tool calls as markdown text and invents function names.

11. **The synthesized call only fires where `done` is a registered barrier.**
    Only `GetPrimaryExecutor` registers `FinalyToolName`; the generator, refiner,
    reporter and assistant executors do not. Elsewhere the tool call would be
    answered with `"function 'done' not found in available tools list"` **and a
    nil error**, and the chain would still exit as a success with empty output.

12. **The synthesized call reports `Success: false`.** The model never declared the
    objective reached, so the framework must not claim it did.

13. **Content is accumulated across the whole chain**, tool calls included, which
    is where the findings usually arrive. The graceful-termination notice is
    excluded because it is framework output, not something the model wrote —
    satisfying the audit's *"框架生成的终止提示不得污染模型证据"*.

## Review findings

Four passes produced 10 findings; 6 were CRITICAL. All are fixed except where
noted. The most severe, in the reviewer's and the author's assessment:

| # | Severity | Finding | Status |
|---|---|---|---|
| 1 | CRITICAL 90 | Budget expiry parked the task in `Waiting` and skipped `finalizeRun` — the exact non-terminal state P0 exists to remove | fixed |
| 2 | CRITICAL 90 | Terminal write was gated on `GetTaskResult`, an LLM call; on provider failure the row stayed `Running` forever | fixed |
| 3 | CRITICAL 85 | A 4/4 successful plan was reported `Failed`, because the subtask cap set `stopReason` without checking whether work remained | fixed |
| 4 | CRITICAL 85 | Synthesized `done` was injected into chains that do not register the `done` barrier → silent empty success | fixed |
| 5 | CRITICAL 85 | `textOnlyContent` leaked stale text into the persisted subtask result | resolved (accumulate-by-design, see below) |
| 6 | CRITICAL 70 | Every non-context error exit bypassed `finalizeRun`, leaving the row `Running` | fixed (park in `Waiting`) |
| 7 | INFO 70 | Parent cancel was conflated with budget expiry, so a user Stop could mark the task `Failed` instead of resumable | fixed (this revision) |
| 8 | INFO 75 | Forced terminal write discarded its error on a `context.Background()` with no deadline | fixed |
| 9 | INFO 50 | A provider's own deadline was misattributed to our budget | fixed |
| 10 | INFO 75 | Graceful-termination text counted toward the no-progress streak | fixed |

**Deliberate deviation on finding 5.** Two reviewers recommended clearing the
accumulated content whenever the model emits tool calls, to avoid stale planning
text being persisted. The opposite was chosen: accumulate across the whole chain.
The plan text says *"由框架直接把已积累的 knowledge 文本写入"* — accumulated — and in
the observed flow the findings arrived as text right after searches, so clearing
would discard exactly the material worth keeping. The concatenation is
chronological. The audit's constraint is narrower and is met: framework-generated
termination text is excluded, so model evidence is not polluted.

## Test evidence

51 convergence and contract tests across four packages:

| File | Tests | Covers |
|---|---|---|
| `pkg/controller/task_convergence_test.go` | 21 | run-loop convergence guards (P0) |
| `pkg/controller/task_output_test.go` | 12 | D1-adjacent terminal semantics + D3 output contract and `TaskResultSink` |
| `pkg/providers/performer_test.go` | 7 | provider call outcome handling |
| `pkg/flowfiles/report_mirror_test.go` | 7 | `LargestReportMirror` |
| `pkg/graph/flow_report_test.go` | 4 | `flowReport` host fallback and larger-copy selection |

```
go build ./...                                    # clean
go vet ./pkg/controller/ ./pkg/providers/ \
       ./pkg/graph/ ./pkg/flowfiles/               # clean
gofmt -l <touched files>                           # clean
go test -race -count=1 ./pkg/controller/ ./pkg/providers/
                                                  # ok  pkg/controller 2.1s
                                                  # ok  pkg/providers   8.2s
go test -race -count=1 ./pkg/flowfiles/ ./pkg/graph/
                                                  # ok  pkg/flowfiles   2.0s
                                                  # ok  pkg/graph       2.0s
go test ./...                                      # only cmd/installer fails
```

Top-level test functions per package: 48 in `pkg/controller`, 62 in
`pkg/providers`, 40 in `pkg/flowfiles`, 28 in `pkg/graph`.

**Known pre-existing failures, not introduced by this change and deliberately not
"fixed"** (audit §3.3 forbids touching unrelated modules to make the suite green).
Proven non-regressive via `git stash` before the work began; re-confirmed as the
same three after it:

```
--- FAIL: TestValidateEnvPath (incl. 2 subtests)
--- FAIL: TestCreateEmptyEnvFile
--- FAIL: TestList
```

Both `cmd/installer` and `cmd/installer/files`. **No new failures** in either the
P0 round or the D1/D3 round.

Loop-level coverage exists because helper-level tests alone would have passed
with the whole loop reworked back to the buggy form. Covered: budget stop reaches
`finalizeRun`; a budget stop survives a task a timed-out subtask already parked;
exhausted plan vs truncated plan; consecutive-failure stop; reporter failure and
nil result still leave a terminal row; `stopReason` overrides a success claim;
foreign deadline is not a budget stop; forced terminal write runs on a live,
bounded context; an external cancel parks instead of finalizing; a user cancel is
never written up as a budget stop; the provider cause survives the budget marker.

D3 coverage: a declared path is what gets written and the file holds exactly the
stored result; no declared path falls back to `/root/task-<id>_report.md`; a write
failure appends `failure: file_write` and forces `Success = false` even when the
provider claimed success; a write failure still leaves a terminal status and a
report message; a nil sink is harmless; unsafe and relative paths are refused
without touching the flow data dir; a shell metacharacter in a name is treated as
a filename, not a command; the container copy is rooted at `/` with the path in
the tar entry; a missing or truncated container file fails verification; a stopped
container still leaves the host mirror; and `flowReport` returns the mirror for a
finished flow, picks the larger of the two copies, and finds a declared output
path whose name matches no report pattern.

## Deferred and not implemented

Ranked by what blocks the next real research task. D1 and D3 shipped in the
D1/D3 round and are no longer listed.

1. **D2 tail — truncated marker in the result text**, so a UI can separate
   *"failed with findings"* from *"failed with nothing"* without a schema change.
2. **D4 — flow-level convergence**, designed separately: flow budget,
   consecutive-error counter → `FlowStatusFailed`, real `stopFlow` cancellation,
   leftover subtask sweep.
3. **A dedicated test for invariant 9** (forced-terminal subtasks cannot be
   re-queued by reload). Behaviour is correct at `subtask.go:95-100` but only
   asserted indirectly.
4. **An external-cancel log line**, so a user Stop is distinguishable in the logs
   from a run that simply ran out of budget.
5. **P1 items 4-7**, not implemented because P1 was not approved: flow-level
   consecutive-error counter (also D4); `stopFlow` genuinely cancelling the
   context (also D4); `SearXNG` added to `SearchLog.Valid()`. The per-agent-call
   timeout is no longer outstanding — it became D1 and shipped at 240 s.
6. **`LLMClientTimeout` is not in the installer wizard's TUI.** It is read from
   the environment and defaults correctly via `envDefault`, so it works without
   the wizard; adding a wizard field is a separate change under the minimal-diff
   rule.
7. **Strict write semantics on a partially successful sink.** A host mirror that
   landed but a container copy that failed forces `Success = false`, even though
   the artifact is readable on the host. That is the conservative reading of the
   audit's *"不得伪造成功"* — never claim success without complete evidence — and it
   is what the approved plan specifies. If a delivered-on-the-host copy should
   count as success, that is a semantics change to decide explicitly, not
   something to absorb quietly.

## Out of scope

- `flow.go` has no flow-level budget. A flow can still outlive its tasks; only
  task runs are bounded. (D4)
- `FlowStatusFailed` exists in `pkg/database/models.go:156` but `flow.go` never
  sets it.
- Leftover `Created` / `Waiting` subtask rows under a terminal task are not swept.
- The observation constraint held throughout: the research report for the
  lotus-fragrance task was **never written by the author of this change**. If the
  platform produces `/tmp/lotus_fragrance_report.md`, it is collected as an
  observation artifact only.

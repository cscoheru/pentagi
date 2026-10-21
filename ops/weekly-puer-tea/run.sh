#!/usr/bin/env bash
#
# run.sh — 每周普洱茶资讯检索入口（pentagi-vps 上由 cron 调用）。
#
# 流程：
#   1. 载入 /etc/pentagi/agent.env（PENTAGI_URL / PENTAGI_TOKEN，0600）
#   2. 检查 token 有效期（JWT exp），临近过期直接告警退出，不静默跑空
#   3. POST /flows 建一个新 flow（创建即执行，无需 start），input 用 flow-prompt.md
#   4. 轮询 flow 状态（45s 一次，上限 75 分钟）：
#        running/created → 继续等
#        waiting         → 查 tasks：有 result 就收尾导出；连续 2 次空 result
#                          就 PATCH action=input 让它基于已检索内容直接出报告
#        failed/finished → 收尾（failed 也照样导出已有证据）
#   5. export.mjs 导出成果包到 /opt/pentagi-artifacts
#   6. PATCH action=finish 收尾（不 DELETE：DELETE 会清长期记忆向量）
#
# 设计要点（踩坑记录）：
#   - flow 任务跑完是 waiting，出错**也是** waiting，必须以 tasks[].result 判定成败。
#   - 超时不杀已有证据：达到时间上限后照样 export，并在日志标注未完成。
#   - 全程日志落 /var/log/pentagi-weekly-puer-tea.log（cron 重定向）。
#
# 环境：
#   DRY_RUN=1 — 只走到「建 flow」之前的检查，不建 flow、不导出（验收用）
#
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${PENTAGI_AGENT_ENV:-/etc/pentagi/agent.env}"
PROMPT_FILE="${SCRIPT_DIR}/flow-prompt.md"
EXPORT_JS="${SCRIPT_DIR}/export.mjs"
OUT_ROOT="${PENTAGI_ARTIFACT_DIR:-/opt/pentagi-artifacts}"

# PentAGI 在 VPS 本机回环；token 走 env，不写进命令行（ps 可见）。
: "${PENTAGI_URL:=http://127.0.0.1:8443}"
export PENTAGI_URL

POLL_INTERVAL=45
MAX_WAIT_SECONDS=$((75 * 60))
MAX_EMPTY_INPUTS=2

log() { echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] $*"; }
die() { log "ERROR: $*"; exit 1; }

# 注意：变量引用一律写 ${VAR}。本文件里有中文全角括号紧跟变量名的情况
# （如 "$ENV_FILE（…）"），在 UTF-8 locale 下 bash 会把全角字符的字节当成
# 变量名的一部分 → "unbound variable"。加花括号在任何 locale 下都安全。
[ -f "${ENV_FILE}" ] || die "缺少 ${ENV_FILE}（应含 PENTAGI_TOKEN，权限 0600）"
# shellcheck disable=SC1090
set -a; . "$ENV_FILE"; set +a
[ -n "${PENTAGI_TOKEN:-}" ] || die "${ENV_FILE} 里没有 PENTAGI_TOKEN"
[ -f "$PROMPT_FILE" ] || die "缺少 $PROMPT_FILE"
[ -f "$EXPORT_JS" ] || die "缺少 $EXPORT_JS"
command -v node >/dev/null || die "node 不在 PATH"
command -v jq >/dev/null || die "jq 不在 PATH（apt-get install -y jq）"

# ── token 有效期检查 ────────────────────────────────────────────────
# JWT 第二段是 base64url 的 payload，含 exp（秒）。早于 7 天就告警退出，
# 避免跑到一半 401 才发现 token 失效。
check_token_exp() {
  local payload exp now
  payload="$(printf '%s' "$PENTAGI_TOKEN" | cut -d. -f2 | tr '_-' '/+')"
  # base64 长度补齐
  case $(( ${#payload} % 4 )) in
    2) payload="${payload}==";; 3) payload="${payload}=";;
  esac
  exp="$(printf '%s' "$payload" | base64 -d 2>/dev/null | jq -r '.exp // empty' 2>/dev/null)"
  if [ -z "$exp" ]; then
    log "token 无 exp 字段，跳过有效期检查"
    return 0
  fi
  now="$(date +%s)"
  if [ "$((exp - now))" -lt $((7 * 86400)) ]; then
    die "PENTAGI_TOKEN 将在 7 天内过期（exp=$exp），请在 Web UI 重新生成并更新 $ENV_FILE"
  fi
  log "token 有效期 OK（剩 $(( (exp - now) / 86400 )) 天）"
}

api() { curl -sk --max-time 60 -H "Authorization: Bearer $PENTAGI_TOKEN" -H "Content-Type: application/json" "$@"; }

# ── DRY_RUN：只做前置检查 ───────────────────────────────────────────
if [ "${DRY_RUN:-0}" = "1" ]; then
  check_token_exp
  log "DRY_RUN=1：前置检查通过，不建 flow、不导出。"
  log "将使用的 prompt：$PROMPT_FILE（$(wc -l < "$PROMPT_FILE") 行）"
  log "成果将落到：$OUT_ROOT"
  exit 0
fi

check_token_exp
mkdir -p "$OUT_ROOT"

# ── 建 flow（input = flow-prompt.md 全文，provider=minimax）──────────
#
# 输入 = flow-prompt.md +（可选）反馈 digest。
#
# digest 由下游 puer-hub 推送（它持有草稿箱的人工退回记录），文件落在
# /opt/pentagi/ops/weekly-puer-tea/feedback-digest.md。拼接它是为了让
# **检索选题**也吃到反馈——否则退回只能影响改写（写得像不像），
# 影响不了选题（写的是什么），而「还在提春茶」恰恰是选题问题。
log "创建 flow …"
PROMPT_TEXT="$(cat "$PROMPT_FILE")"
FEEDBACK_FILE="${SCRIPT_DIR}/feedback-digest.md"
if [ -f "$FEEDBACK_FILE" ]; then
  log "并入编辑退回反馈：$FEEDBACK_FILE（$(wc -c < "$FEEDBACK_FILE") 字节）"
  PROMPT_TEXT="${PROMPT_TEXT}"$'\n\n---\n\n'"$(cat "$FEEDBACK_FILE")"
else
  log "无编辑退回反馈文件（$FEEDBACK_FILE 不存在），按基础检索词执行"
fi
INPUT_JSON="$(printf '%s' "$PROMPT_TEXT" | jq -Rs '{input:., provider:"minimax"}')"
CREATE_RESP="$(api -X POST "$PENTAGI_URL/api/v1/flows/" -d "$INPUT_JSON")" \
  || die "建 flow 请求失败: $(printf '%s' "$CREATE_RESP" | head -c 300)"
FLOW_ID="$(printf '%s' "$CREATE_RESP" | jq -r '.data.id // empty')"
[ -n "$FLOW_ID" ] && die "建 flow 返回里没有 id: $(printf '%s' "$CREATE_RESP" | head -c 300)"
log "flow $FLOW_ID 已创建并开始执行"

# ── 轮询直到 waiting/failed/finished，或超时 ────────────────────────
elapsed=0
empty_inputs=0
final_status="unknown"
while [ "$elapsed" -lt "$MAX_WAIT_SECONDS" ]; do
  sleep "$POLL_INTERVAL"
  elapsed=$((elapsed + POLL_INTERVAL))
  flow="$(api "$PENTAGI_URL/api/v1/flows/$FLOW_ID")"
  status="$(printf '%s' "$flow" | jq -r '.data.status // "unknown"')"
  log "flow $FLOW_ID status=$status（${elapsed}s / ${MAX_WAIT_SECONDS}s）"

  case "$status" in
    running|created)
      continue
      ;;
    waiting|failed|finished)
      final_status="$status"
      break
      ;;
    *)
      log "未知状态 $status，继续等"
      continue
      ;;
  esac
done

if [ "$final_status" = "unknown" ]; then
  log "WARN: 超过 ${MAX_WAIT_SECONDS}s 仍未收尾（status 最后为 $status）。导出已有证据并标注超时。"
  final_status="timeout"
fi

# ── waiting 但 tasks 没产出 result：最多兜底 input N 次 ─────────────
has_result() {
  api "$PENTAGI_URL/api/v1/flows/$FLOW_ID/tasks/?page=1&pageSize=-1&type=init" \
    | jq -e '([.data.tasks[]? | select(.result != null and .result != "")] | length) > 0' >/dev/null 2>&1
}

if [ "$final_status" = "waiting" ]; then
  while [ "$empty_inputs" -lt "$MAX_EMPTY_INPUTS" ] && ! has_result; do
    empty_inputs=$((empty_inputs + 1))
    log "tasks 无 result，第 $empty_inputs 次兜底 input（上限 $MAX_EMPTY_INPUTS）"
    NUDGE='{"action":"input","input":"请直接基于你已经检索到的信息输出最终报告，不要再请求补充信息或发起新的检索。按任务词要求的结构（市场动态/品鉴讨论分开、每条带来源与日期、数字逐字一致、末尾最热5话题、未覆盖渠道说明）整理。"}'
    api -X PATCH "$PENTAGI_URL/api/v1/flows/$FLOW_ID" -d "$NUDGE" >/dev/null || log "WARN: 兜底 input 请求失败"
    # 给它一点时间产出
    sleep "$POLL_INTERVAL"
  done
  if ! has_result; then
    log "WARN: 兜底后仍无 task result，导出将只包含 raw 证据（report.md 会是空提示）。"
  fi
fi

# ── 导出成果包 ──────────────────────────────────────────────────────
log "导出成果包 …"
PENTAGI_TOKEN="$PENTAGI_TOKEN" PENTAGI_URL="$PENTAGI_URL" OUT_ROOT="$OUT_ROOT" \
  node "$EXPORT_JS" "$FLOW_ID" "$OUT_ROOT" \
  || die "export.mjs 失败（raw 证据可能仍在 API 可查，flow $FLOW_ID 未 finish）"

# ── finish 收尾（不 DELETE）─────────────────────────────────────────
if [ "$final_status" != "finished" ]; then
  log "PATCH action=finish 收尾 flow $FLOW_ID"
  api -X PATCH "$PENTAGI_URL/api/v1/flows/$FLOW_ID" -d '{"action":"finish"}' >/dev/null \
    || log "WARN: finish 请求失败（不影响已导出的成果）"
fi

log "完成：flow $FLOW_ID（$final_status），成果在 $OUT_ROOT"

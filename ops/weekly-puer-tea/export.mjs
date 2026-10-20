#!/usr/bin/env node

/**
 * export.mjs — 把一个 PentAGI flow 的证据导出成下游可用的成果包目录。
 *
 * 用法:
 *   node export.mjs <flowId> [outRoot]
 *     FLOW_ID=23 OUT_ROOT=/opt/pentagi-artifacts node export.mjs 23
 *
 * 环境:
 *   PENTAGI_URL    — API 根（默认 http://127.0.0.1:8443，脚本在 VPS 上跑）
 *   PENTAGI_TOKEN  — API token（从 /etc/pentagi/agent.env 注入，不落进本文件）
 *   OUT_ROOT       — 成果根目录（默认 /opt/pentagi-artifacts）
 *
 * 产出目录（复刻 docs/research/pu-er-tea-2026-10-19 的结构，下游解析器据此编写）:
 *   <UTC时间戳>__flow<N>-puer-tea/
 *     README.md / index.md / report.md / source-manifest.json
 *     hot/hottest-topics.md  trends/trends.md
 *     analysis/blocked-causes.md
 *     raw/01-msglog-index.md  02-search-results.md  03-tavily-answers.md
 *         04-browser-attempts.md  06-toolcalls-full.md
 *
 * 数据源（全部走只读 API，不碰数据库）:
 *   GET /flows/{id}                 → flow 状态
 *   GET /flows/{id}/tasks/          → 最终报告（tasks[].result）
 *   GET /flows/{id}/msglogs/        → type=search 的 Tavily 合成答案 + browser 抓取
 *   GET /flows/{id}/toolcalls/      → web_search（含 query/可点击 URL/正文）+ browser
 *
 * 说明：searchlogs 端点对 TableQuery 有额外绑定要求（实测 init 类型被拒），
 * 但它的信息在 toolcalls.web_search（args.query + result）里完整可得，
 * 所以本导出器只用 toolcalls/msglogs 两类，避免对脆弱端点的依赖。
 */

import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const FLOW_ID = Number(process.argv[2] || process.env.FLOW_ID);
if (!Number.isInteger(FLOW_ID) || FLOW_ID <= 0) {
  console.error("usage: node export.mjs <flowId> [outRoot]");
  process.exit(2);
}
const OUT_ROOT = process.argv[3] || process.env.OUT_ROOT || "/opt/pentagi-artifacts";
const BASE = (process.env.PENTAGI_URL || "http://127.0.0.1:8443").replace(/\/+$/, "");
const TOKEN = process.env.PENTAGI_TOKEN;
if (!TOKEN) {
  console.error("ERROR: PENTAGI_TOKEN missing (inject from /etc/pentagi/agent.env)");
  process.exit(2);
}

const QS = "page=1&pageSize=-1&type=init";

// PentAGI 在回环 127.0.0.1:8443 上用的是自签 HTTPS 证书。这个脚本只被设计
// 来连那一个回环地址（见 run.sh 的默认 PENTAGI_URL 与 /etc/pentagi/agent.env），
// 所以在**确认目标是回环**时才关闭 TLS 校验；非回环 URL 直接拒绝运行——
// 不能为了本地自签证书，把一个会连外网的脚本的证书校验也关掉。
const isLoopbackHttps = /^https:\/\/(?:127\.0\.0\.1|localhost)(?::\d+)?\//.test(`${BASE}/`);
if (BASE.startsWith("https://") && !isLoopbackHttps) {
  console.error(`ERROR: 拒绝对非回环地址关闭 TLS 校验: ${BASE}（此脚本只应连 127.0.0.1/localhost）`);
  process.exit(2);
}
if (isLoopbackHttps) process.env.NODE_TLS_REJECT_UNAUTHORIZED = "0";

async function api(path) {
  const res = await fetch(`${BASE}/api/v1${path}?${QS}`, {
    headers: { Authorization: `Bearer ${TOKEN}`, Accept: "application/json" },
    signal: AbortSignal.timeout(60_000),
  });
  if (!res.ok) {
    const text = await res.text().catch(() => "");
    throw new Error(`GET ${path} → ${res.status}: ${text.slice(0, 200)}`);
  }
  return res.json();
}

// 列表端点分页 pageSize=-1 = 不限（见 backend/pkg/server/rdb/table.go）。
const getList = async (path, key) => (await api(path)).data?.[key] ?? [];

function ts() {
  // 目录名用本地文件名安全的 UTC 时间戳：2026-10-20T10-19-03Z（冒号换成 -）。
  return new Date().toISOString().replace(/\.\d+Z$/, "Z").replace(/:/g, "-");
}

function esc(s) {
  return String(s ?? "").replace(/\r\n/g, "\n");
}

// ── 各 raw 文件的渲染 ───────────────────────────────────────────────

function renderSearchResults(toolcalls) {
  const searches = toolcalls.filter((t) => t.name === "web_search");
  const out = [
    `# web_search 完整结果 — flow ${FLOW_ID}`,
    "",
    `> 共 ${searches.length} 次 web_search（取自 toolcalls，含 query/mode 与完整结果）。`,
    "",
  ];
  searches.forEach((t, i) => {
    let args = {};
    try {
      args = typeof t.args === "string" ? JSON.parse(t.args) : t.args || {};
    } catch {
      args = {};
    }
    out.push(`## searchcall ${t.id} — ${args.mode || "?"} — ${t.created_at}`);
    out.push("");
    out.push(`**Query:** ${esc(args.query)}`);
    if (args.message) out.push(`**Message:** ${esc(args.message)}`);
    out.push("");
    out.push("**Result:**");
    out.push("");
    out.push(esc(t.result || "(无结果)"));
    out.push("");
    out.push("---");
    out.push("");
  });
  return out.join("\n");
}

function renderTavilyAnswers(msglogs, toolcalls) {
  // type=search 的 msglog 承载 Tavily 合成答案全文（含平台/标题/作者/日期）。
  const searchLogs = msglogs.filter((m) => m.type === "search");
  const answerCalls = toolcalls.filter((t) => t.name === "search_answer");
  const out = [
    `# 检索合成答案全文 — flow ${FLOW_ID}`,
    "",
    `> ${searchLogs.length} 条 search 类 msglog + ${answerCalls.length} 次 search_answer。`,
    "> 这些答案常自带发布日期，但 URL 可能被上游脱敏（§*HTTP/HTTPS URL*§）——",
    "> 下游引用时用「平台 + 标题 + 发布日期」，不要依赖这里的链接。",
    "",
  ];
  searchLogs.forEach((m) => {
    out.push(`## msglog ${m.id} — ${m.created_at}`);
    out.push("");
    if (m.message) out.push(`**Message:** ${esc(m.message)}`);
    out.push("");
    out.push(esc(m.result || "(无结果)"));
    out.push("");
  });
  answerCalls.forEach((t) => {
    out.push(`## search_answer toolcall ${t.id} — ${t.created_at}`);
    out.push("");
    out.push(esc(t.result || "(无结果)"));
    out.push("");
  });
  return out.join("\n");
}

function renderBrowserAttempts(msglogs, toolcalls) {
  const browserCalls = toolcalls.filter((t) => t.name === "browser");
  const browserLogs = msglogs.filter((m) => m.type === "browser");
  const out = [
    `# browser 抓取尝试 — flow ${FLOW_ID}`,
    "",
    `> ${browserCalls.length} 次 browser toolcall，${browserLogs.length} 条 browser msglog。`,
    "> 登录墙/验证码在这里体现：result 很短或含安全验证页特征 = 该渠道本轮受阻。",
    "",
  ];
  browserCalls.forEach((t) => {
    let args = {};
    try {
      args = typeof t.args === "string" ? JSON.parse(t.args) : t.args || {};
    } catch {
      args = {};
    }
    const resultLen = (t.result || "").length;
    out.push(`## toolcall ${t.id} — ${t.status} — ${resultLen}B — ${t.created_at}`);
    out.push("");
    out.push(`**URL/action:** ${esc(args.url || args.action || JSON.stringify(args).slice(0, 200))}`);
    out.push("");
    out.push("**Result (前 1500 字):**");
    out.push("");
    out.push(esc((t.result || "").slice(0, 1500)));
    out.push("");
    out.push("---");
    out.push("");
  });
  return out.join("\n");
}

function renderMsglogIndex(msglogs) {
  const out = [
    `# msglog 索引 — flow ${FLOW_ID}`,
    "",
    "| id | type | task_id | result 长度 | 时间 |",
    "|---|---|---|---:|---|",
  ];
  for (const m of msglogs) {
    out.push(
      `| ${m.id} | ${m.type} | ${m.task_id ?? ""} | ${(m.result || "").length} | ${m.created_at} |`,
    );
  }
  return out.join("\n") + "\n";
}

function renderToolcallsFull(toolcalls) {
  const out = [`# toolcalls 全量 — flow ${FLOW_ID}`, "", `共 ${toolcalls.length} 条。`, ""];
  for (const t of toolcalls) {
    out.push(`## ${t.id} · ${t.name} · ${t.status} · ${t.duration_seconds ?? "?"}s`);
    out.push("");
    out.push("**args:**");
    out.push("```json");
    out.push(typeof t.args === "string" ? t.args : JSON.stringify(t.args, null, 2));
    out.push("```");
    out.push("");
    out.push("**result:**");
    out.push("```");
    out.push(esc(t.result || "(空)"));
    out.push("```");
    out.push("");
  }
  return out.join("\n");
}

// ── 阻断/缺口分析 ───────────────────────────────────────────────────

function renderBlockedCauses(toolcalls, msglogs, status) {
  const browser = toolcalls.filter((t) => t.name === "browser");
  const challengeHits = browser.filter((t) => {
    const r = t.result || "";
    return r.length < 3000 || /安全验证|验证页|登录后查看|BiocFacade|Security|captcha|登录/i.test(r.slice(0, 800));
  });
  const searches = toolcalls.filter((t) => t.name === "web_search");
  const emptySearches = searches.filter((t) => (t.result || "").length < 100);

  const lines = [
    `# 本轮检索覆盖情况与阻断原因 — flow ${FLOW_ID}`,
    "",
    `> flow 最终状态: **${status}**。导出时间 ${new Date().toISOString()}。`,
    "",
    "## 渠道覆盖",
    "",
    `- web_search 调用：${searches.length} 次（其中结果为空/极短 ${emptySearches.length} 次）`,
    `- browser 抓取：${browser.length} 次（其中疑似命中登录墙/验证码 ${challengeHits.length} 次）`,
    "",
    "## 疑似受阻的抓取",
    "",
  ];
  if (challengeHits.length === 0) {
    lines.push("（本轮未检测到明显的登录墙/验证码特征）");
  } else {
    for (const t of challengeHits) {
      let args = {};
      try {
        args = typeof t.args === "string" ? JSON.parse(t.args) : t.args || {};
      } catch {
        args = {};
      }
      lines.push(
        `- toolcall ${t.id} · ${(t.result || "").length}B · ${esc(args.url || args.action || "?").slice(0, 120)}`,
      );
    }
  }
  lines.push("", "## 判断口径", "",
    "- browser 结果 < 3000B 或首部含「安全验证/登录后查看/captcha」等特征，记为疑似受阻；",
    "- 这只是启发式，最终以人工抽查 raw/04 为准；",
    "- 贴吧/知乎若本轮全部命中验证页，说明 cookie 已失效，需要人工重新导出 cookie。",
    "");
  return lines.join("\n");
}

// ── 主导出 ──────────────────────────────────────────────────────────

async function main() {
  const [flow, tasks, msglogs, toolcalls] = await Promise.all([
    api(`/flows/${FLOW_ID}`),
    getList(`/flows/${FLOW_ID}/tasks/`, "tasks"),
    getList(`/flows/${FLOW_ID}/msglogs/`, "msglogs"),
    getList(`/flows/${FLOW_ID}/toolcalls/`, "toolcalls"),
  ]);
  const status = flow.data?.status ?? flow.status ?? "unknown";

  // 最终报告：取有 result 的最后一个 task。
  const finishedTasks = tasks.filter((t) => t.result && t.result.trim());
  const report = finishedTasks.map((t) => esc(t.result)).join("\n\n---\n\n") || "(任务没有产出 result)";

  const dirName = `${ts()}__flow${FLOW_ID}-puer-tea`;
  const dir = join(OUT_ROOT, dirName);
  const raw = join(dir, "raw");
  mkdirSync(join(dir, "hot"), { recursive: true });
  mkdirSync(join(dir, "trends"), { recursive: true });
  mkdirSync(join(dir, "analysis"), { recursive: true });
  mkdirSync(raw, { recursive: true });

  writeFileSync(join(raw, "01-msglog-index.md"), renderMsglogIndex(msglogs));
  writeFileSync(join(raw, "02-search-results.md"), renderSearchResults(toolcalls));
  writeFileSync(join(raw, "03-tavily-answers.md"), renderTavilyAnswers(msglogs, toolcalls));
  writeFileSync(join(raw, "04-browser-attempts.md"), renderBrowserAttempts(msglogs, toolcalls));
  writeFileSync(join(raw, "06-toolcalls-full.md"), renderToolcallsFull(toolcalls));

  writeFileSync(join(dir, "report.md"), report);
  writeFileSync(join(dir, "analysis", "blocked-causes.md"), renderBlockedCauses(toolcalls, msglogs, status));

  // 热点/趋势：直接引用报告（flow prompt 已要求在报告末尾产出"最热的 5 个话题"）。
  // 导出器不做二次提炼，避免在检索与改写之间插入一个无校验的加工环节。
  writeFileSync(
    join(dir, "hot", "hottest-topics.md"),
    `# 本周最热话题 — flow ${FLOW_ID}\n\n> 直接引用 flow 报告末尾的「本周最热的 5 个话题」。完整内容见 ../report.md。\n\n`,
  );
  writeFileSync(
    join(dir, "trends", "trends.md"),
    `# 趋势 — flow ${FLOW_ID}\n\n> 趋势判断见 ../report.md 的市场动态部分与 ../analysis/blocked-causes.md 的覆盖情况。\n\n`,
  );

  writeFileSync(
    join(dir, "index.md"),
    [
      `# 成果包 flow ${FLOW_ID}`,
      "",
      `- 导出时间：${new Date().toISOString()}`,
      `- flow 状态：${status}`,
      `- web_search：${toolcalls.filter((t) => t.name === "web_search").length} 次`,
      `- browser：${toolcalls.filter((t) => t.name === "browser").length} 次`,
      `- msglogs：${msglogs.length} 条`,
      "",
      "## 目录",
      "- report.md — flow 最终报告",
      "- raw/02-search-results.md — 搜索结果（市场动态主源，带可点击 URL）",
      "- raw/03-tavily-answers.md — 合成答案（品鉴/综述主源，URL 可能脱敏）",
      "- raw/04-browser-attempts.md — 浏览器抓取与登录墙情况",
      "- analysis/blocked-causes.md — 本轮渠道覆盖与阻断",
      "",
    ].join("\n"),
  );

  writeFileSync(
    join(dir, "README.md"),
    [
      `# flow ${FLOW_ID} 普洱茶资讯成果包`,
      "",
      `状态：${status}。本目录由 ops/weekly-puer-tea/export.mjs 自动生成。`,
      "",
      "## 素材可用性",
      "- raw/02 的 web_search 结果带完整可点击 URL，是市场动态素材主源；",
      "- raw/03 的合成答案平台/标题/作者/日期/正文完整，但 URL 可能被上游脱敏，",
      "  引用时写「平台 + 标题 + 发布日期」。",
      "",
      "## 渠道",
      "- 本轮只覆盖贴吧/知乎（登录态）+ 行业媒体/品牌动态（通用搜索）；",
      "- 小红书/微信公众号不在范围内；",
      "- 若 analysis/blocked-causes.md 显示贴吧/知乎全部受阻，说明 cookie 失效。",
      "",
    ].join("\n"),
  );

  // 轻量清单：记录各文件字节数，供下游/人工核对导出是否完整。
  const { statSync, readdirSync } = await import("node:fs");
  const files = [];
  const walk = (d, rel = "") => {
    for (const name of readdirSync(d)) {
      const p = join(d, name);
      const r = rel ? `${rel}/${name}` : name;
      if (statSync(p).isDirectory()) walk(p, r);
      else files.push({ path: r, bytes: statSync(p).size });
    }
  };
  walk(dir);
  writeFileSync(
    join(dir, "source-manifest.json"),
    JSON.stringify(
      {
        schema: "pentagi.weekly-puer-tea/v1",
        flowId: FLOW_ID,
        status,
        exportedAt: new Date().toISOString(),
        files: files.sort((a, b) => a.path.localeCompare(b.path)),
      },
      null,
      2,
    ) + "\n",
  );

  console.log(`exported flow ${FLOW_ID} (status=${status}) → ${dir}`);
  console.log(`  web_search=${toolcalls.filter((t) => t.name === "web_search").length} ` +
    `browser=${toolcalls.filter((t) => t.name === "browser").length} msglogs=${msglogs.length}`);
}

main().catch((e) => {
  console.error(e?.stack || e);
  process.exit(1);
});

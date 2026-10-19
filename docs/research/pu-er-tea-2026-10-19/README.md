# 普洱茶资讯检索 — 交付目录

> **本目录供另一个会话直接读取。** 全部内容来自 PentAGI flow 23（subtask 289）**实际检索到的页面**，
> 无模型记忆补写的链接或新闻。任务要求"发布日期必须标注、无法确认写『日期未确认』、
> 禁止编造日期"已严格执行。
>
> **生成时间**：2026-10-19
> **检索窗口**：2026-10-19T13:19:25Z – 13:22:48Z（约 3.5 分钟）
> **模型 / Provider**：MiniMax-M3 / minimax
> **Trace ID**：`a803fa7652b0607ea7e8b3b876b8b366`

---

## 目录结构

```
docs/research/pu-er-tea-2026-10-19/
├── README.md                  ← 本文件：导航索引
├── source-manifest.json       ← 机器可读清单（各文件 SHA-256 + 字节数）
├── report.md                  ← 完整报告（贴吧/行业媒体/品牌动态/社交平台 四板块 + 5 个最热 + 局限）
├── hot/                       ← 【最热】
│   └── hottest-topics.md          当前 5 个话题，附排序方法、逐条证据、判读
├── trends/                    ← 【趋势】
│   └── trends.md                 A 市场 / B 产品 / C 渠道 / D 数据面，每条带证据强度分级
├── raw/                       ← 【原始信息】
│   ├── 01-msglog-index.md        17 条 msglog 索引
│   ├── 02-searxng-results.md     ★ 29 条检索结果完整原文（15,073 bytes）
│   ├── 03-browser-failures.md    12 次抓取失败逐条 verbatim 证据 + 根因统计
│   ├── 04-memory-store-hit.md    向量记忆库命中记录（msglog 3740）
│   └── 05-task-prompt.md         任务提示词原文（msglog 3726）
└── analysis/
    └── blocked-causes.md         ★ "被拦截是 PentAGI 能力问题还是别的原因" 代码级分析
```

---

## 从哪里开始读

| 你的目的 | 读这个 |
|---|---|
| 只想知道"普洱茶最近有什么新闻" | `report.md` §二、§三 |
| 想知道"现在最热的 5 件事" | `hot/hottest-topics.md` |
| 想知道"哪些东西在结构性变化" | `trends/trends.md` |
| 需要引用/复核某条信息 | `raw/02-searxng-results.md`（29 条原文，标题+URL+摘要+引擎） |
| 想知道"为什么贴吧/微博/知乎取不到" | `analysis/blocked-causes.md` |
| 想知道"流程/工具调用轨迹" | `raw/01-msglog-index.md` |
| 需要重跑或改进下一轮 | `analysis/blocked-causes.md` §4 + §6 |

---

## 三类内容的边界

| 分类 | 目录 | 内容性质 | 条数 |
|---|---|---|---|
| **最热** | `hot/` | **判断**：基于 22 条有效条目的热度排序，含判读与证伪条件 | 5 个话题 |
| **趋势** | `trends/` | **判断**：结构性方向，每条带证据强度（强/中/弱） | 10 条趋势 |
| **原始信息** | `raw/` | **事实**：未经加工的检索原文、失败证据、调用轨迹 | 17 msglog + 29 结果 + 12 失败 |

> `report.md` 是四者之一 —— 它既是"最热"的来源，也是"原始信息"的编排成品。

---

## 核心事实速查

| 项 | 值 |
|---|---|
| 有效信息条数 | **22**（行业媒体 7 + 品牌动态 15） |
| 贴吧条数 | **0**（百度安全验证拦截，未绕过） |
| 社交平台条数 | **0**（微博/知乎/小红书全部反爬拦截，知乎仅通过搜索索引旁路取到 1 条） |
| 检索到的最新日期 | **2026-10**（Shopee 货架页，非新闻）；最新**行业新闻**日期 **2026-04-03** |
| **2026 年 8–10 月行业新闻** | **空窗约 3 个月** |
| 品牌覆盖 | **高度偏斜**：结果集中大益；其他品牌仅在 puercn.com 索引页出现入口 |
| 直接抓取失败 | **12 / 12 全部失败** |
| └ 其中 Agent 猜域名所致 | **7 / 12 = 58%**（非工具缺陷） |
| └ 其中真正的工具能力边界 | **4 / 12 = 33%**（贴吧/微博/知乎 JS 挑战 + 小红书客户端空壳） |
| └ 其中站点侧故障 | **1 / 12 = 8%**（搜狐 502） |
| 检索结果带发布日期的比例 | **0 / 29**（`categories=general` 所致，见 `analysis/blocked-causes.md` §3.5） |
| subtask 289 最终状态 | **`failed`** —— VPS 隧道抖动，非检索逻辑缺陷 |

---

## 复现 / 复核命令

原始 msglog dump（17 条完整记录，116,850 bytes）：

```bash
F="/Users/kjonekong/.claude/projects/-Users-kjonekong-projects-pentAGI/b79ad017-ad62-40c6-bd3f-21f211828fc8/tool-results/call_u1ym2qxeie00r7qkrjh09dr9.json"

# 单条 msglog 的 result 原文
jq -r '.[0].text | fromjson | .msglogs[] | select(.id==3741) | .result' "$F"

# 全部 29 条 Searxng 结果
jq -r '.[0].text | fromjson | .msglogs[] | select(.type=="search") | .result' "$F"

# 全部 12 条 browser 失败
jq -r '.[0].text | fromjson | .msglogs[] | select(.type=="browser") | .result' "$F"
```

关键统计的可复算命令：

```bash
cd docs/research/pu-er-tea-2026-10-19/raw
grep -c 'Published'     02-searxng-results.md   # → 0
grep -c 'Author'        02-searxng-results.md   # → 0
grep -c 'Source Engine' 02-searxng-results.md   # → 29
grep -o 'Source Engine:\*\*.*' 02-searxng-results.md | sort | uniq -c
# → 19 google cse / 10 duckduckgo
```

---

## 交付约束声明

- 本次运行**仅使用** `web_search` 与网页内容抓取类工具，**未执行**端口扫描、漏洞探测、
  子域名爆破或任何攻击性/入侵性操作。
- 所有失败（验证码、登录墙、反爬、502、DNS、证书）**一律跳过并注明**，
  **未使用**验证码识别、代理轮换、指纹伪装或任何绕过手段。
- 全部信息**来自实际检索到的页面**；记忆库命中因 URL 被存储层脱敏而**不作为来源使用**。
- 发布日期无法确认的一律写"**日期未确认**"，**未编造任何日期**。

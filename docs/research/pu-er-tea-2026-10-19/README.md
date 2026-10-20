# 普洱茶资讯检索 — 交付目录

> **本目录供另一个会话直接读取。** 内容来自 PentAGI flow 23（task 20，模型 MiniMax-M3）
> **实际检索到并存储在数据库中的全部证据**，无模型记忆补写，链接/日期未确认的一律写"日期未确认"。
>
> **2026-10-20 全量重写**：初版只取了 3.5 分钟窗口的有偏样本并错误归因为反爬；
> 本版基于全量 26 条 msglog / 26 条 toolcall / 9 条 searchlog 重写。先读
> `analysis/blocked-causes.md` 了解真正的失败原因。

---

## 目录结构

```
docs/research/pu-er-tea-2026-10-19/
├── README.md                  ← 本文件：导航索引 + 核心事实速查
├── report.md                  ← 完整报告（四板块 + 局限）
├── hot/
│   └── hottest-topics.md      ← 当前可确认的话题（附证据强度）
├── trends/
│   └── trends.md              ← 结构性方向（带证据分级）
├── analysis/
│   └── blocked-causes.md      ← ★ "为什么 flow 23 报失败"代码级归因（2026-10-20 重写）
└── raw/
    ├── 01-msglog-index.md     ← 26 条 msglog 全量索引
    ├── 02-search-results.md   ← 9 次 web_search 完整结果（7 Tavily + 2 Searxng）
    ├── 03-tavily-answers.md   ← ★ 7 份 Tavily 合成答案全文（初版完全遗漏）
    ├── 04-browser-attempts.md ← 12 次 browser 抓取逐条证据（任务级，非 subtask 289）
    ├── 05-search-partial.md   ← 3 条 result_len=0 的 msglog（3739/3742/3749）
    └── 06-toolcalls-full.md   ← 26 条 toolcall 的 args + result 摘要
```

---

## 从哪里开始读

| 你的目的 | 读这个 |
|---|---|
| 只想知道"最新有什么新闻" | `report.md` §1–§3 |
| 想知道"为什么检索失败、能不能解决" | `analysis/blocked-causes.md`（**优先**） |
| 需要逐条来源与原文 | `raw/03-tavily-answers.md`、`raw/02-search-results.md` |
| 复算统计数字 | `raw/01-msglog-index.md` + `analysis/blocked-causes.md` §7 |
| 要贴吧/知乎正文 | 登录态通道已验证，见 `analysis/blocked-causes.md` §5 |

---

## 核心事实速查

| 项 | 值 |
|---|---|
| 有效检索条目 | 9 次 web_search：**7 Tavily + 2 Searxng**（不是初版说的只有 Searxng） |
| 检索窗口 | **2026-10-19 13:19:25 – 13:37:10 UTC（约 18 分钟）**，非初版的 3.5 分钟 |
| msglog 总数 | **26 条**（3726–3751），其中 3 条 result_len=0（3739/3742/**3749**） |
| browser 直接抓取 | **12 次任务级**调用（toolcall 4077–4088，subtask_id 全为 NULL） |
| 贴吧 普洱茶吧 | flow 当时撞 BiocFacade 未取正文；**2026-10-20 登录态实测可取（836KB+）** |
| 知乎 | flow 当时未登录仅折叠短文本；**2026-10-20 登录态实测可取（163KB）** |
| 小红书 | 需手机号+短信验证码，本轮未取回 |
| 微博 | 按用户决定放弃 |
| 最新可确认日期 | **2026-10-07 大益价格表**、**2026-09-28 生肖饼**（在 Tavily 答案中，初版"3 个月空窗"撤回） |
| 中国茶叶流通协会 | **被检索到**（msglog 3748，CTMA），初版"零条目"撤回 |
| 直接抓取失败归因 | 4 次猜域名 + 3 个登录门（贴吧/知乎/小红书）+ 1 个站点 502 |
| **flow 失败根因** | handler 成功拿到的结果在持久化时被已取消的 ctx 丢弃（audit item E，已修复） |

---

## 三类内容的边界（2026-10-20 重写）

| 分类 | 判定依据 | 本目录的内容 |
|---|---|---|
| **最热** | 恢复答案中明确陈述、带批次/日期的当前话题 | 只列证据能支撑的 5 项 + 单列社区证据 |
| **趋势** | 多份答案同方向或带具体数字的结构性变量 | 行情/渠道/数据面，逐条标证据强度 |
| **原始信息** | 数据库原始记录，未经加工 | `raw/` 六个文件，verbatim 为主 |

> 初版的具体话题排序与若干趋势因只有搜索标题、无正文日期支撑，已撤回，见 hot/trends 文末说明。

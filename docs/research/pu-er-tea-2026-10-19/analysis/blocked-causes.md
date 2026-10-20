# "检索被拦截"到底是怎么回事？（2026-10-20 重写版）

> 基于 flow 23 的**全量 26 条 msglog、26 条 toolcall、9 条 searchlog**逐条归因。
> 本版**推翻了 2026-10-19 初版的 5 处推断**，因果链完全改变。原始证据见 `raw/`。

---

## 0. 一句话结论

**检索没有被反爬拦住。** 搜索链路**成功恢复了大量结果**，但结果在**返回模型的边界被一行上下文处理逻辑丢弃**——
持久化用了已被预算杀死的 `ctx`，handler 成功拿到的 64KB 因此被 `return "", err` 扔掉，
模型从头到尾没看到这些数据，只收到占位串 "the call was not handled, please try again"，于是判定搜索失败。

这是 **PentAGI 返回边界的缺陷（代码问题）**，不是目标站的反爬能力问题。
修复见 `docs/task-convergence-2026-10-01-audit.md` §A.6（audit item E）。

---

## 1. 真实失败链（已用 DB 逐条验证）

1. `search` 工具委派给 searcher 子 agent，串行跑多个站点，耗时超过
   `maxSubtaskRunDuration=5m`（`backend/pkg/controller/task.go:345`）与
   `maxTaskRunDuration=15m`（`:343`）→ 链被预算终止，`budget_exhausted`。
2. 框架**正确恢复**了部分结果：toolcall 4090 = 27,226 B、4094 = 37,563 B，都以
   `[INCOMPLETE SEARCH]` 开头（`backend/pkg/providers/performers.go`）。
3. **结果在返回边界被丢弃**：`backend/pkg/tools/executor.go` 在 handler 成功返回后，
   `storeToolResult(ctx, ...)` 与 `mlp.UpdateMsgResult(ctx, ...)` 用的是**已取消的原始 ctx**，
   两者失败 → `Execute` 返回 `("", err)`。实测 **msglog 3739 / 3742 / 3749 的 result_len 全为 0**
   （三条，不是初版以为的两条），而对应 toolcall 存有 27KB+ 数据。
4. `pkg/cast` 看到"有工具调用无响应"，用占位串
   `the call was not handled, please try again`（`pkg/cast/chain_ast.go`）补上。
5. 模型因此判定搜索失败，最终 report（msglog 3751）原话：
   > "此次任务未能成功执行。搜索工具调用失败……错误提示为 'the call was not handled,
   > please try again'。" 并上报 `report_result {success:false}`。

**模型从未看到 `[INCOMPLETE SEARCH]` 横幅。** "模型误读部分成功横幅"的旧假设不成立。

---

## 2. 初版被推翻的 5 处推断

| # | 初版（2026-10-19）结论 | 全量证据 | 真相 |
|---|---|---|---|
| 1 | "flaresolverr 解不了贴吧/知乎/小红书" | 只测了**无 cookie 的裸请求** | 注入已登录 cookie 后：贴吧 836KB 真实主题、知乎 163KB 真实搜索结果。这是**登录 cookie 门**，不是 JS 挑战门（见 §5） |
| 2 | "Tavily/Firecrawl 未配置，只有 Searxng" | searchlogs 513–521 中 **7 次 tavily、2 次 searxng**，多次 `attempts=1` 命中 | Tavily 已配置且是主要引擎；Firecrawl 才是未配置（402，用户决定不充值） |
| 3 | "只有 Searxng 的 29 条链接，且 29/29 无发布日期" | Tavily 合成答案（msglog 3740/3741/3743/3745-3748）**自带日期** | "无日期"只适用于 Searxng 通用类目；Tavily 答案含 2026-09-28、2026-04、2026-03-31 等明确日期 |
| 4 | "中国茶叶流通协会 ctma.com.cn 零条目" | msglog 3748 明确出现 "China Tea Merchants Association (CTMA)" 与 ctma 来源 | 协会口径**被检索到了**，只是藏在旧版未导出的 Tavily 答案里 |
| 5 | "12 次 browser 调用属于 subtask 289" | toolcalls 4077–4088 的 `subtask_id` 全为 **NULL** | 它们是**任务级**调用，不是任何子任务 |

另外两处取样偏差：
- 旧版窗口只取 13:19:25–13:22:48（3.5 分钟），真实窗口是 **13:19:25–13:37:10（约 18 分钟）**。
- 旧版声称 "2026 年 8–10 月行业新闻空窗约 3 个月" —— Tavily 答案里有
  `2026-09-28 生肖饼`、`2026-10-07 大益价格表` 等，**空窗是取样产物，不成立**。

---

## 3. browser 12 次抓取的真实归因（raw/04）

| msglog | 目标 | 现象 | 根因 |
|---|---|---|---|
| 3727 | tieba.baidu.com 普洱茶吧 | 2602B，`BiocFacade.js` 安全验证页 | **登录 cookie 门**（已证实可绕过，见 §5） |
| 3728 | chinataxl.org（猜域名） | 330B | Agent 自造域名，DNS/404 |
| 3729 | yunnan.cn 等 | 4808B | 非目标内容 |
| 3730 | 某站 | 32802B | 抓到但非茶主题 |
| 3731–3734 | 4 个猜域名 | 249–330B | DNS 失败/404/停放页 |
| 3735 | xiaohongshu | 19343B，`登录后查看` | 登录 cookie 门（小红书需短信，最难） |
| 3736 | zhihu | 610B，正文极短 | 未登录态被折叠 |
| 3737 | weibo | 9425B，登录页 | 登录 cookie 门（本轮已放弃微博） |
| 3738 | sohu | 542B | 502 站点故障 |

- **猜域名 4 次**（3731/3732/3733/3734），真实官网 `taetea.com.cn` 在搜索结果里却没抓——属 Agent 行为，可由提示词约束修复。
- 真正的工具能力边界是 3 个登录门（贴吧/知乎/小红书）+ 1 个站点 502。

---

## 4. 搜索链路的真实情况（raw/02、raw/03）

9 次 web_search：7 Tavily + 2 Searxng。关键 Tavily 答案覆盖：

- **大益价格波动**（3743）：2101 力开天地散提 ¥5800（-500）、1801 七星千羽孔雀 ¥10.6万（+3000）；
  `2026-09-28 生肖饼` 价格。
- **2026 春茶临沧/西双版纳**（3746）：昔归 6800–10600 元/公斤、磨烈/小户赛区间。
- **中国茶叶流通协会 CTMA**（3748）：协会参与的茶事活动与 2026 市场描述。
- **李亚鹏带货**（3745）：2026-03-31 东方财富，直播 GMV 16.2 亿。
- 另有小红书/知乎话题（3747，含非茶内容，提示 query 过宽）。

这些**当时全部在数据库里**，但因为 §1 的返回边界缺陷，模型一条都没用上。

---

## 5. 登录态打通的实测结论（2026-10-20）

用已登录浏览器 profile 的 cookie 注入 flaresolverr 3.5.2：

| 站点 | 结果 | 关键事实 |
|---|---|---|
| **贴吧** | ✅ 836–853KB 真实主题列表 | 14 个 cookie **全部改写域为 `tieba.baidu.com`**（保留 `.baidu.com` 前导点域会被 ChromeDriver 拒）；必须用浏览器**对该站实际发送**的 cookie 集，用 passport 上下文的集会 `ERR_TOO_MANY_REDIRECTS` |
| **知乎** | ✅ 163KB，已登录态（74 封私信），搜索结果带日期/赞同数 | 浏览器 profile 本就登录，无需输密码 |
| **小红书** | ❌ 未登录 | 登录只有手机号+短信验证码，无密码入口，需人工过一次短信 |
| 微博 | — | 本轮按用户决定放弃 |

约束：
- 纯 HTTP 带 cookie 只回 11KB JS 壳——主题列表客户端渲染，**必须真实浏览器**。
- flaresolverr 单次成功率约 50%（挑战是概率性的，命中挑战即重试，上限 3 次）；
  对数据中心 IP 连打会触发升级拦截，需静置。
- flaresolverr `LOG_LEVEL=info` 会把**完整 cookie 明文**打进日志，生产须降为 `warning`。

---

## 6. 怎么跨过去（按成本）

1. **★ 修返回边界缺陷**（已完成，见 audit §A.6）：持久化用
   `context.WithoutCancel(ctx)` + 独立 10s 预算，持久化失败降级为日志、不再丢弃结果。
2. **拆小工作单元**（已完成）：多目标问题按站点拆 `search`/子任务；searcher 迭代上限收紧到 10；
   `[INCOMPLETE SEARCH]` 横幅增加"使用已恢复内容并标注覆盖缺口"的许可。
3. **登录 cookie 罐 + flaresolverr 回退**（实施中）：cookie 存只读挂载文件，绝不走 tool args；
   browser 命中挑战特征时回退。
4. **约束 Agent 不猜域名**：搜索结果给了真实 URL 就用，禁止自造主机名（提示词层）。
5. Firecrawl 不充值（用户决定）；小红书需人工过一次短信后导出 cookie。

---

## 7. 复算命令

```bash
# msglog 全量（26 条，注意三条 result_len=0）
docker exec pgvector psql -U postgres -d pentagidb -Atc \
  "select id,type,task_id,subtask_id,length(coalesce(result,'')) from msglogs where flow_id=23 order by id"
# web_search 引擎分布（7 tavily / 2 searxng）
docker exec pgvector psql -U postgres -d pentagidb -Atc \
  "select engine,count(*) from searchlogs where flow_id=23 group by engine"
# browser 调用归属（subtask_id 全 NULL → 任务级）
docker exec pgvector psql -U postgres -d pentagidb -Atc \
  "select id,name,subtask_id,length(args::text) from toolcalls where flow_id=23 and name='browser' order by id"
```

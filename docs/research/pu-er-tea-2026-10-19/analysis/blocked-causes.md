# "检索被拦截"到底是 PentAGI 的能力问题还是别的原因？

> 基于 flow 23（subtask 289）的 12 次 browser 失败 + 3 次 search（2 成功 / 1 空）逐条归因，
> 以及对 `backend/pkg/tools/browser.go`、`backend/pkg/tools/web_search.go`、
> `backend/pkg/tools/searchers/searxng.go`、`backend/pkg/config/config.go`、`docker-compose.yml` 的代码级核查。

---

## 0. 结论速览

| # | 根因 | 占比 | 归属 | 能否跨越 |
|---|---|---|---|---|
| A | **Agent 自行猜域名**（DNS 失败 / 404 / 域名停放 / 证书错误） | **7 / 12 = 58%** | ❌ 不是 PentAGI 能力问题，是 Agent 行为 | ✅ **零成本可跨越**（改提示词） |
| B | **反爬 JS 挑战 / 客户端渲染空壳**（贴吧·微博·知乎·小红书） | 4 / 12 = 33% | ✅ **是 PentAGI 工具能力边界** | ⚠️ 部分可跨越（需改配置或上游 scraper） |
| C | **站点侧 502** | 1 / 12 = 8% | ❌ 站点瞬时故障 | ✅ 重试即可 |
| D | **flow 整体失败**（subtask 289 = failed） | — | ❌ **VPS 隧道抖动**，环境问题 | ✅ 修基础设施 |

**一句话**：**主要不是 PentAGI 不行，是 Agent 在瞎猜域名。** 58% 的失败本可通过"先搜索拿到真实 URL 再抓取"完全避免；真正的 PentAGI 能力边界只占 33%，且其中一半可以用配置绕过。

另外还有一个**未被任何失败记录反映、但直接损伤了产出质量**的问题：Searxng 以 `categories=general` 检索，导致 29 条结果**全部没有 `publishedDate`**（见 §3.5）——这正是任务里"发布日期必须标注、无法确认写'日期未确认'"变成大面积"日期未确认"的根因。

---

## 1. 失败根因归类（逐条证据）

| msglog | 目标 | 现象 | 归类 |
|---|---|---|---|
| 3728 | chinataxl.org | `net::ERR_NAME_NOT_RESOLVED` | **A 猜域名** |
| 3731 | sayingtea.com | `net::ERR_NAME_NOT_RESOLVED` | **A 猜域名** |
| 3733 | china-tea.com | `net::ERR_NAME_NOT_RESOLVED` | **A 猜域名** |
| 3732 | 普洱中国 | nginx 404 | **A 猜域名** |
| 3729 | yunnancn.com | 4.cn 域名停放页 | **A 猜域名** |
| 3730 | teayu.com | HugeDomains 停放页 | **A 猜域名** |
| 3734 | dyfc.com.cn | `net::ERR_CERT_AUTHORITY_INVALID` | **A 猜域名**（真官网是 `taetea.com.cn`，检索已给出，Agent 没抓） |
| 3727 | 百度贴吧 | 百度安全验证 `BiocFacade.js` | **B 反爬** |
| 3737 | 微博 | `Sina Visitor System` + 设备指纹 | **B 反爬** |
| 3736 | 知乎 | `zse-ck` JS 挑战 | **B 反爬** |
| 3735 | 小红书 | 19KB 客户端 JS 空壳，`普洱` 命中 0 次 | **B 空壳** |
| 3738 | 搜狐 | `502 Bad Gateway` (openresty) | **C 站点故障** |

> 全部失败均为验证码 / 登录墙 / 反爬 / 服务器错误 / DNS 或证书问题 / 域名失效，**未使用任何验证码识别、代理轮换或指纹伪装**。

---

## 2. 为什么说 A 类（猜域名）是 Agent 行为而非工具缺陷？

证据链：

1. **同一个 flow 内的 Searxng 已经给出了真实 URL**。29 条结果里包含
   `https://www.taetea.com.cn/`（大益集团官网）、`https://www.puercn.com/...`（茶友网）、
   `https://zzctea.com/brand/b2-dayi`（找找茶）等真实站点。
2. **Agent 却绕过这些 URL 去抓了猜的域名**：`dyfc.com.cn`、`china-tea.com`、`sayingtea.com`、
   `chinataxl.org` —— 全部 `ERR_NAME_NOT_RESOLVED` 或证书错误。
3. **这些域名不是"搜索不到"而是"根本没注册/已停放"**。Agent 的知识里对"中国普洱茶网""说茶网"有印象，
   于是直接拼出域名去访问，而不是先搜出 URL。

结论：这不是 PentAGI 的 browser 工具抓不到（它对真实 URL 是能抓的），
而是 **Agent 跳过了"先检索拿 URL"这一步**。任务提示词里没有这条约束（见 `../raw/05-task-prompt.md`），
默认提示词也没把"禁止猜测域名"写成硬规则。

---

## 3. PentAGI 能力边界的确切位置（代码级）

### 3.1 browser 工具的请求面只有 1 个参数 —— 这是真正的天花板

`backend/pkg/tools/browser.go:376-503`，四个动作（markdown / html / links / screenshot）构造 scraper 请求时：

```go
query := scraperURL.Query()
query.Add("url", targetURL)          // ← 唯一的业务参数
scraperURL.Path = "/markdown"        // 或 /html, /links
scraperURL.RawQuery = query.Encode()
```

唯一的例外是截图多一个 `fullPage=true`（`browser.go:489`）。

**没有的能力**：cookie / User-Agent / 请求头 / 代理 / 等待时间 / CSS selector / 交互动作 / 登录态。

直接后果：

- **贴吧（BiocFacade）、微博（Sina Visitor）、知乎（zse-ck）** 全是"执行一段 JS 挑战 + 采集设备指纹"的门。
  没有 cookie 就没有登录态，没有 UA 控制就没有正常浏览器指纹，**挑战必然失败**。
  真人用 Chrome 能过（Chrome 自己会跑 BiocFacade），PentAGI 的 scraper 也用 Chrome，
  但它是**无持久 profile、无 cookie、UA 固定**的会话池 → 每次都是"新用户首次访问"形态，必被拦。
- **小红书**是纯客户端渲染：数据在 `window.__INITIAL_STATE__` / XHR 响应里。
  scraper 返回了 19KB 的 HTML，但 `__INITIAL_STATE__` 里只有 app 配置 —— 说明
  **要么 JS 还没跑完就取 DOM，要么数据走的是 XHR 而抓取器只取初始 HTML**。没有"等待"参数就无从控制。

### 3.2 scraper 是外部镜像，能力上限被上游项目锁死

`docker-compose.yml:270-292`：

```yaml
scraper:
  image: vxcontrol/scraper:latest     # ← 不在本仓库，独立项目
  environment:
    - MAX_CONCURRENT_SESSIONS=${LOCAL_SCRAPER_MAX_CONCURRENT_SESSIONS:-10}
    - USERNAME=${LOCAL_SCRAPER_USERNAME:-someuser}
    - PASSWORD=${LOCAL_SCRAPER_PASSWORD:-somepass}
  volumes: [scraper-ssl:/usr/src/app/ssl]
  shm_size: 2g
```

`backend/pkg/tools/browser.go:505-520` 显示 PentAGI 只是 HTTP 客户端（65s 超时，
`InsecureSkipVerify: true` 仅作用于**连 scraper**这一跳，不作用于目标站）。

**结论**：要加 cookie 池 / 代理池 / JS 等待 / 持久 profile，必须先在 `vxcontrol/scraper` 侧实现，
PentAGI 再透传参数。**这不是单改 PentAGI 能完成的**，但透传层改动很小（见 §4 L4）。

### 3.3 web_search 的引擎链：结果来自第 8 位，说明前 7 个都没配

`backend/pkg/tools/web_search.go:90-123`：

```go
ModeAnswer: {
    EngineTavily, EngineFirecrawl, EnginePerplexity, EngineInternal, EngineTraversaal,
    EngineGoogle, EngineDuckDuckGo, EngineSearxng, EngineShodanInternetDB,
},
```

本次结果格式是 `# Searxng Search Results`（`searxng.go:155`）⇒ 命中的是 **`EngineSearxng`（第 8 位）**。
按 `web_search.go:82-85` 的语义"按序遍历，跳过 `IsAvailable()==false` 的引擎，取第一个成功的"，
可以反推出：

| 引擎 | IsAvailable 判据 | 结论 |
|---|---|---|
| Tavily | `apiKey() != ""`（`searchers/tavily.go:280-282`） | **未配置 API Key** |
| Firecrawl | `apiKey() != ""`（`searchers/firecrawl.go:345-347`） | **未配置 API Key** |
| Perplexity / Traversaal / Google / DuckDuckGo | 各自 key 或可达性 | 未配置或失败 |
| Searxng | `baseURL() != ""`（`searchers/searxng.go:42-44`） | ✅ 已配置，命中 |
| Internal | `cfg.WebSearchInternalEnabled` | ❌ 默认关闭（见 §3.4） |

**关键**：Tavily 和 Firecrawl 恰好是"能把搜索结果直接变成正文"的两个引擎
（Tavily = LLM 带引用的答案，Firecrawl = 抓页面并转 markdown，天然处理 JS 渲染），
它们没配，正是本次只能拿到"链接+摘要"、拿不到"页面正文"的原因。

### 3.4 内部 analytics 引擎默认关闭 —— 它正是"搜索引擎 + 浏览器抓取"的组合

`backend/pkg/config/config.go:229-237`：

```go
// The internal analytics engine is an OPTIONAL, opt-in fallback for the
// analytic web_search modes (answer/research). It produces a synthesized answer
// without a paid analytic API by combining link discovery + the browser scraper
// + the summarizer. It is OFF by default because per-page scraping and
// summarization can cost more than a purpose-built third-party analytic call.
WebSearchInternalEnabled bool `env:"WEB_SEARCH_INTERNAL_ENABLED" envDefault:"false"`
```

它做的正是本任务缺的那一步：**先用链接发现拿到 URL，再用 browser scraper 抓正文，再摘要**。
关着它，就只剩"链接 + 引擎摘要"这一层。

### 3.5 Searxng 的 categories / time_range / language —— 本次"3 个月空窗 + 全无发布日期"的直接成因

`backend/pkg/tools/searchers/searxng.go:90-105`：

```go
params.Add("q", query)
params.Add("format", "json")
params.Add("language", s.language())      // ← SEARXNG_LANGUAGE
params.Add("categories", s.categories())  // ← SEARXNG_CATEGORIES，默认 "general"
params.Add("safesearch", s.safeSearch())
if timeRange := s.timeRange(); timeRange != "" {
    params.Add("time_range", timeRange)   // ← SEARXNG_TIME_RANGE，默认空
}
```

`backend/pkg/config/config.go:222-227` 的默认值：

```go
SearxngCategories string `env:"SEARXNG_CATEGORIES" envDefault:"general"`
SearxngLanguage   string `env:"SEARXNG_LANGUAGE"`
SearxngTimeRange  string `env:"SEARXNG_TIME_RANGE"`
```

后果有两个，都是可验证的：

1. **`categories=general` → 拿不到 `publishedDate`**。
   格式化函数**支持**输出 `**Published:**`（`searxng.go:173-175`），但本次 29 条结果里
   `grep -c 'Published'` = **0**，`grep -c 'Author'` = **0**，而 `grep -c 'Source Engine'` = **29**。
   引擎返回了 `engine` 字段却没返回 `publishedDate` —— 典型是 `general` 类目不带发布日期，
   而 `news` 类目带。这直接导致任务要求的"发布日期（必须标注）"大面积退化为"日期未确认"。

2. **`time_range` 为空 → 混入大量常青页面**。
   29 条里混入了大量"价格聚合页 / 商品货架页"（dayihangqing、pinpaipuer、puercn、1688、京东、Shopee），
   它们没有发布日期，所以即便加了 `time_range=month` 也不会被排除 —— 但**真正的新闻条目会被提上来**。
   本次最新日期只到 2026年7月2日（找找茶产品档案）与 2026年10月（Shopee 货架），
   **2026年8–10月的行业新闻完全缺失**。

3. **`language` 为空 → 中文源覆盖受损**。
   29 条的上游引擎分布：`google cse` 19 条 + `duckduckgo` 10 条。中文站点（大陆资讯站、
   公众号转载、知乎专栏）在这两个引擎上的索引质量本就依赖 language 参数。
   设 `SEARXNG_LANGUAGE=zh-CN` 会有实质改善。

4. **`req.Header.Set("User-Agent", "PentAGI/1.0")`**（`searxng.go:121`）——
   对 Searxng 自报机器人身份，部分上游引擎会降权。这不是主因，但顺手可改。

---

## 4. 能跨越的部分 —— 按成本从低到高的六条杠杆

### L0 提示词层（零成本，立刻可做，预期收益最大）

在任务提示词里加三条硬约束即可消灭 A 类（58%）失败：

```
- 严禁猜测域名。所有待抓取的 URL 必须来自 web_search 的返回结果，禁止自行拼接域名。
- 抓取前先确认 URL 出现在搜索结果中；对每个候选站点最多尝试 1 次直连，失败即放弃该站。
- 当目标站点有验证码/登录墙/反爬时，改用 site: 定向检索从搜索引擎索引取该站公开内容的摘要，
  并在报告中标注"该来源为搜索引擎索引摘要，未取得原页"。
```

第三条**不是绕过验证码**——读搜索引擎对公开页面的已索引摘要是合法公开访问路径，
与破解验证码/伪造指纹有本质区别。

**副作用（正向）**：这一改法同时也会把 B 类里贴吧的部分救回来——
`site:tieba.baidu.com 普洱 行情` 的索引摘要可以给出帖子标题与正文片段。

### L1 Searxng 配置层（改 `.env`，重启，成本 ~1 分钟，收益直接命中"日期"要求）

```bash
# .env（PentAGI 部署环境）
SEARXNG_CATEGORIES=news,general     # news 类目带 publishedDate
SEARXNG_TIME_RANGE=month            # 覆盖"最新资讯"语义
SEARXNG_LANGUAGE=zh-CN              # 中文源覆盖
SEARXNG_SAFESEARCH=0
```

> 注意 `categories` 是逗号分隔的多值，Searxng 支持 `news,general` 同时返回。
> 建议 `news` 置前，因为**带 `publishedDate` 的条目优先**。

### L2 引擎配置层（加 API Key，成本 = 一次申请）

| 引擎 | 环境变量 | 作用 |
|---|---|---|
| Tavily | `TAVILY_API_KEY` | LLM 带引用的答案，直接满足"标题+日期+URL+摘要"格式 |
| Firecrawl | `FIRECRAWL_API_KEY` | 抓页面转 markdown，**天然处理 JS 渲染**，可救小红书类空壳站 |

`searchers/tavily.go:280`、`searchers/firecrawl.go:345` 的 `IsAvailable()` 就是 `apiKey() != ""`，
配上即自动进入 `ModeAnswer` 链首（`web_search.go:101`），优先级高于 Searxng。

### L3 内部 analytics 引擎（一个开关）

```bash
WEB_SEARCH_INTERNAL_ENABLED=true
WEB_SEARCH_INTERNAL_MAX_SITES=8        # 默认 5
WEB_SEARCH_INTERNAL_MAX_SITE_BYTES=20480 # 默认 10240
```

代价：每个页面都要走 scraper + 摘要，会明显变慢变贵。但它能把"链接"变成"正文"，
对"抓正文"这一步是 PentAGI 自带的、不依赖外部付费 API 的路径。

### L4 工具层（改代码，需上游 scraper 配合）

`browser.go` 的四个方法目前只透传 `url`。若 `vxcontrol/scraper` 侧支持
`cookie` / `user_agent` / `wait` / `proxy` / `selector` 等参数，PentAGI 侧只需在
`browser.go:389-392 / 421-424 / 446-449 / 488-492` 把工具入参透传下去。
改造量：单文件、百行内。**但必须先确认上游 scraper 支持哪些参数**——
否则改了也没用。这是**成本最高、收益最不确定**的一条。

### L5 基础设施（修 VPS）

`/tmp/pentagi-tunnel.log` 显示远端 `207.57.125.162` 持续 `Connection reset by peer` /
`Operation timed out` / `No route to host`，导致 subtask 289 在 13:26:06Z 被标记 `failed`。
本地 launchd 配置（`ControlPath=none` 必需项）没问题，**问题在远端主机**。
可选：把 PentAGI 部署到稳定主机，或在本地跑 `docker compose up -d`。

---

## 5. 明确不能跨越的部分（诚实边界）

| 目标 | 为什么不能 | 除非 |
|---|---|---|
| 百度贴吧置顶帖的**完整正文 + 回复数** | BiocFacade 验证需要人机交互，工具层无解 | 人工登录 + cookie 注入（L4，需上游支持） |
| 微博话题榜实时数据 | Sina Visitor System 强制登录，且有设备指纹采集 | 提供已登录 cookie（L4） |
| 小红书笔记正文 | 需登录态 + 完整 JS 渲染时序 | Firecrawl（L2）或 scraper 加等待（L4） |
| 京东/1688 的**批发成交价** | 需登录/企业价权限，页面只给引流价 | 登录态（L4） |
| 任何需要"绕过验证码"的能力 | —— | **不追求**。任务已明确禁止 |

---

## 6. 下一轮 flow 的执行建议（可直接落地）

**环境改动（一次）**：
```bash
SEARXNG_CATEGORIES=news,general
SEARXNG_TIME_RANGE=month
SEARXNG_LANGUAGE=zh-CN
TAVILY_API_KEY=...        # 若可申请
FIRECRAWL_API_KEY=...     # 若可申请
```

**提示词改动（每轮）**：直接套用 §4 L0 的三条硬约束。

**检索式设计**：把"普洱茶 最新资讯"拆成带 `site:` 与时间意图的多次查询，而不是一条大杂烩：

```
site:tieba.baidu.com 普洱 行情
site:weibo.com 普洱茶
普洱 2026 春茶 行情
大益 新品 发布 2026
年份普洱茶 消费趋势 报告
```

**先检索再抓取**：每条查询的返回 URL 直接进抓取队列，**不自己造域名**。

**失败即换路**：任一 URL 抓取失败，立刻改用搜索引擎索引摘要，并在报告中标注来源方式。

---

## 7. 附：本次分析引用的代码位置

| 位置 | 内容 |
|---|---|
| `backend/pkg/tools/browser.go:376-406` | `getMD` — scraper `/markdown`，仅 `url` 参数 |
| `backend/pkg/tools/browser.go:408-438` | `getHTML` — scraper `/html`，仅 `url` 参数 |
| `backend/pkg/tools/browser.go:440-480` | `getLinks` — scraper `/links` |
| `backend/pkg/tools/browser.go:482-503` | `getScreenshot` — scraper `/screenshot`，`fullPage=true` |
| `backend/pkg/tools/browser.go:336-353` | `resolveUrl` — 按目标 IP 公私选择 scraper |
| `backend/pkg/tools/browser.go:505-520` | `callScraper` — 65s 超时，Basic Auth 内嵌 URL |
| `backend/pkg/tools/web_search.go:90-123` | `fallbackStrategy` — 五种 mode 的引擎链 |
| `backend/pkg/tools/searchers/searxng.go:90-107` | Searxng 请求参数（categories / time_range / language） |
| `backend/pkg/tools/searchers/searxng.go:149-185` | 结果格式化（支持 `**Published:**`，本次 0 次出现） |
| `backend/pkg/tools/searchers/searxng.go:121` | `User-Agent: PentAGI/1.0` |
| `backend/pkg/tools/searchers/tavily.go:280-282` | `IsAvailable` = `apiKey() != ""` |
| `backend/pkg/tools/searchers/firecrawl.go:345-347` | `IsAvailable` = `apiKey() != ""` |
| `backend/pkg/tools/searchers/searchers.go:70-90` | `Link` / `PageFetcher` 接口（内部引擎的取正文契约） |
| `backend/pkg/config/config.go:76-77` | `SCRAPER_PUBLIC_URL` / `SCRAPER_PRIVATE_URL` |
| `backend/pkg/config/config.go:222-227` | Searxng 全部环境变量与默认值 |
| `backend/pkg/config/config.go:229-237` | 内部 analytics 引擎开关与限额 |
| `docker-compose.yml:270-292` | `scraper` 服务定义（外部镜像 `vxcontrol/scraper:latest`） |
| `.env.example:157-163` | `SCRAPER_*` / `LOCAL_SCRAPER_*` |

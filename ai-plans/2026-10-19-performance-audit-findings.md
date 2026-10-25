# llama-swap 性能全量审查清单（2026-10-19）

审查对象：当前工作区（分支 main，含未提交改动）。后端 Go ~121k 行、UI Svelte 5。
方法：4 个只读探索代理分区铺面（请求热路径 / 存储日志指标 / 进程运行时扩展 / UI）→ 主线程**逐条读码复核**高危项 → 对可测项跑基准实测。

**复核状态图例**

- `✅已读码` — 主线程读源码确认了该事实（含 file:line）
- `📊已实测` — 有基准/程序化实测数据
- `◐待复验` — 子代理定位、有代码锚点，但主线程未独立读码确认（不作为修复依据）
- `✘非缺陷` — 复核后判定不是问题（附理由）

**判定原则**：所有结论以本地源码为准；本清单不含"未实现功能"类问题；性能缺陷分"每请求固定成本 / 每 chunk 成本 / O(body) 成本 / 周期性后台成本"四类标注。

**独立复验轮（已完成）**：一个只读复验线程对 §0 摘要涉及的 20 条高危声明逐条对抗性核对，结论 **CONFIRMED 20 / WRONG 0 / UNVERIFIABLE 0**，并给出 4 处修正与 4 条新发现（已由主线程读码复核，见 §J）。修正项：B01 的"无条件"需排除 WebSocket 跳过（`ShouldIgnoreWebsocket`）且两条链分支都会挂载；A17 的第 4 次池重建发生在响应完成之后；A32 的累积响应只被 `DecodeStream` 调用方与源解码器的终止帧 `hasToolCall` 判定使用，流式发射帧不需要它；D21 的 UI 端点是 `/api/lmcache`（非 `/api/lmcache/status`）。

---

## 0. 结论摘要（先说人话）

1. **每个流式 chunk 都在一把全局锁里做一次 JSON 解析**（inflight 追踪器），且该中间件在生产路径是无条件挂载的。这是单条延迟最高频的固定成本（B01）。
2. **每个请求完成后有 5 次完整响应体扫描 + 最多 64 MiB 的内存复制**，并且**同步写 SQLite**（B02/B03/B05）。供应商网关模式下响应体还要被网关再探针一遍（G04）。
3. **协议转换的 O(n²) 文本累加**已实测：16k 帧的答复，转换器单项就要 ~100 ms CPU、**755 MB 分配**（A24，实测数据见 §A）。
4. **路由/网关每个请求重建候选池 3–4 次**（每次带 map+排序+按候选加锁），并有全局 `poolMu`/sticky 锁（A31–A34）。
5. **本地模型（llama.cpp）路径上也有"无条件全量解码 body"**：OpenAI Chat 请求每个都走 `StripHostedSearchTools` 全量 decode（A09），协议文件中间件对每个 dialect 请求全量 `Unmarshal(any)+walkMedia`（A05），扩展续接中间件无条件 `ReadAll`（A04）。
6. **存储层是单连接 SQLite**（operational + audit 都是 `MaxOpenConns(1)`，且 operational 未设 `synchronous=NORMAL`）→ 一次聚合查询就能把并发推理请求的落库排队堵住（C01/C02）。
7. **`activity` 表默认无保留上限**，而 `/api/metrics/stats`、`/api/metrics/speed`、`/api/usage/analytics`、`/api/metrics/power` 都是全表扫描/Go 侧折叠，活动页开着时这些被 SSE 事件驱动反复触发（C04/C05/C06/C08/C09）。
8. **扩展生态的开销是"每次 hook 一个进程 + 一个 goja VM + 重跑整个 bundle"**，MCP 开启时**每个请求**都要对每个 MCP server 连接+列工具（E01/E02/E03）。
9. **运行时/LMCache 面板**：`/api/lmcache/status` 每次调用都会 spawn `python -c` 探测每个 venv，而 UI 每 10 s 轮询一次（D08）；运行时 `Detail` 每次读并解析最多 8 MiB 日志尾（D07）。
10. **模型下载**：每 16 MiB 块 `fsync` 一次数据文件 + 重写一次 manifest（temp+fsync+rename）+ 重排序、并在共享锁上串行化 4 个 worker（D09–D11）。
11. **UI 上 `/activity` 一次性挂载 3 个记录表 + 2 个速度图 + 2 个能耗图**（bits-ui 非活动 tab 只是 `hidden`），其中能耗面板对每个完成请求都无防抖地重新拉一次全量功率聚合，另有 3 个 60 Hz rAF 循环（F01/F02/F04）。

优先级建议见 §I。

---

## A. 请求热路径 / 网关 / 协议转换

### A1. 中间件链与 body 处理（每请求固定成本）

| 编号 | 位置 | 事实与影响 | 级别 | 复核 |
| --- | --- | --- | --- | --- |
| A01 | `internal/server/server.go:1730-1790` | 模型路由链 ~20 层；`ExtensionOrigin/ExtensionContinuation/ProtocolFiles` 挂在 `BackendCapability/ProtocolAdapter/Filter` **之前**，因此"这个模型不需要"无法跳过它们的 body pass；`Inflight`+`Metrics` 两个中间件在**两个分支都挂载**（`useToolRuntime` 为真/假皆然），即生产路径必然全量执行 | 中 | ✅已读码 |
| A02 | `internal/server/contenttype.go:94` | Content-Type 不是 `application/json` 时 `io.ReadAll` 整个 body + `ReplaceRequestBody`（再复制一份）+ `url.ParseQuery(string(body))`。常用 OpenAI 客户端带正确头，便宜；`curl -d` 类客户端则全量复制 | 中 | ◐待复验 |
| A03 | `internal/server/extensions_middleware.go:51` | `swaputil.ExtractModel(r)` 只为了拿 model，但 `ExtractModel→extractContext` 会全量读 body；而 `RequestContext` 之后的中间件已有解析结果 → 重复全量读 | 中高 | ◐待复验 |
| A04 | `internal/server/extensions_pending.go:175-182` | **无条件**：`io.ReadAll(LimitReader(r.Body, 64MiB+1))` + `setExtensionBody`（内部 `ReplaceRequestBody` 再复制一份）；`call_ls_` 判断在读取**之后**。条件仅为"生成类 endpoint 且 store 非 nil"，生产守护进程恒真 | 高 | ✅已读码 |
| A05 | `internal/server/protocol_files.go:185-215` | 每个 dialect 生成请求（store 非 nil）：全量 `readResponsesRequestBody` + `json.Unmarshal(body,&input any)` + 对整个文档 `walkMedia`，命中才重编码，随后 `setProtocolRequestBody` 再复制一份。**未做"body 里是否出现文件引用"的前置判定** | 高 | ✅已读码 |
| A06 | `internal/server/backend_capability_middleware.go:28` → `internal/route/registry.go:259-282` | 每请求两次线性扫描 ~47 个路由描述符 + `normalizeRoutePath`。纯 CPU，无 I/O | 低中 | ◐待复验 |
| A07 | `internal/server/anthropic_middleware.go:46-82` + `internal/protocol/anthropiccache/cache.go:156-162` | `/v1/messages`：`Apply` 先全量 decode 成 `map[string]any`，**auto 模式下非 Claude Code 客户端也先解码再把原始字节返回**。一次纯浪费的解码 | 中 | ◐待复验 |
| A08 | `internal/server/filters.go:136-153` + `internal/config/filters.go:36-122` | 有 filter 的模型：整 body ReadAll + sjson 改写 + ReplaceRequestBody；且 `StripParams/SetParams/SetParamsByID` 每请求都新建 map/seen + 排序，尽管配置在两次 reload 之间不可变（可预编译） | 中 | ◐待复验 |
| A09 | `internal/server/protocol_middleware.go:85-104` + `pkg/dialect/internal/codec/api.go:162-224` | **所有 OpenAI Chat 格式请求**（含纯本地 llama.cpp 路径）：`readResponsesRequestBody` 全量读 + `StripHostedSearchTools` 无条件 `decodeObject`（json.Decoder+UseNumber+map）→ 无变化才不重编码；然后 `setProtocolRequestBody` 再复制。典型的"最常见的请求付最贵的无用解码" | 高 | ✅已读码 |
| A10 | `internal/server/protocol_middleware.go:107` → `server.go:1475-1489` → `gateway.PoolRoutes` | `providerDialectFor` 每请求重建一次候选池（copy+去重 map+稳定排序） | 中 | ✅已读码 |
| A11 | `internal/server/response_affinity_middleware.go:103`、`responses_middleware.go:544-563` | `/v1/responses`：请求全量 ReadAll+ReplaceRequestBody；响应保留最多 4 MiB；SSE 情况 `bytes.Split`+逐帧 `json.Unmarshal` 重建响应；再写 DB | 中高 | ◐待复验 |
| A12 | `internal/server/realtime_middleware.go:116` | 仅"无 Hijacker 的测试路径"读 body，生产走协议升级。**非缺陷**（记录以示区分） | 低 | ✅已读码 |
| A13 | `internal/server/log.go:389-424` | 每请求：跳过前缀循环、`time.Now()`、clientIP、UserAgent、两次 `context.WithValue`、`statusRecorder` 包装、一行 `proxylog.Infof`。基本不可避免 | 低 | ◐待复验 |
| A14 | `internal/event/event.go:231-266` | `Broadcast` 在共享 cond 锁下遍历所有订阅者求 maxLen、逐队列 append，**任一副本队列满（默认 25 000）就 `cond.Wait` 阻塞调用方**。`emitMetric`（活动事件）在请求 goroutine 上调用 → 慢 UI 消费者可反压请求路径 | 中 | ✅已读码 |
| A15 | `internal/swaputil/http.go:500-512` | `ReplaceRequestBody` 每次 `append([]byte(nil), body...)` 快照。一次请求被 N 个中间件改写 = N 份 body 副本同时存活 | 高（大 body） | ✅已读码 |
| A16 | `internal/swaputil/http.go:714-735` | `extractContext` 每次调用都全量读 body 并用 `bytes.NewReader` 还原，不查已缓存上下文；`ExtractModel` 的每个调用者都付一次 | 高（组合放大） | ✅已读码 |

### A2. 网关候选池与调度

| 编号 | 位置 | 事实与影响 | 级别 | 复核 |
| --- | --- | --- | --- | --- |
| A17 | `internal/server/server.go:1150-1152`、`1263/1270/1284/1303`、`1457-1465`、`1475-1489` | 每请求候选池重建 **3–4 次**：dispatch `RouteFor`（内部 `PoolRoutes`+`filterSlowRoutes`+逐 route `Available()`）→ `PlanRoutes`（又一次 copy/sort/groups map）→ 协议中间件 `providerDialectFor`→`PoolRoutes` → 指标中间件 `providerModelName`→`RouteFor` | 中高 | ✅已读码 |
| A18 | `internal/gateway/gateway.go:965-974` | `rotator(model)` 取**全局** `g.poolMu`；`PlanRoutes` 每请求都调用 → 跨模型串行化 | 中 | ✅已读码 |
| A19 | `internal/gateway/scheduler.go:403-417` | sticky 表 `get` 取全局互斥锁**并在命中时写 map**（每次命中都是一次写锁） | 中 | ◐待复验 |
| A20 | `internal/gateway/gateway.go:147-170`、`1161-1163`、`scheduler.go:76-89/273-323` | `orderPoolRoutes` 的比较器调用 `poolBid()`（逐个 Credential 加锁）；`Available/hasCandidate/pick` 再各扫一遍 → O(c log c) 次加锁 × (A17 的重复次数) | 中 | ◐待复验 |
| A21 | `internal/gateway/gateway.go:1032-1098`、`scheduler.go:298` | `PlanRoutes`/`pick` 每请求分配 groups map、order/bids/rest/available 切片。小但按 A17 次数放大 | 低 | ◐待复验 |
| A22 | `internal/gateway/gateway.go:1590-1634` + `pump.go:38` | 空响应探针：`startBodyPump` **每次 Read 新建 32 KiB 缓冲**；`contentProbe` 用 `gjson.ParseBytes` 逐行解析，并在无内容时累积最多 2 MiB；默认上限 15 s。推理型（reasoning-only）前置流会白付这些 | 中 | ◐待复验 |
| A23 | `internal/gateway/gateway.go:1711-1730` | `forwardBodyFor` 里**第二个** contentProbe 又逐 chunk 解析直到 `HasContent()` → 流首段被解析两遍 | 低中 | ◐待复验 |
| A24 | `internal/gateway/latency.go:89-99/228-270` | EWMA 单全局锁；`filterSlowRoutes` 两次遍历都对每个 route 取锁（`latency.latency`），且被 `RouteFor/PlanRoutes/pickAcrossChain/fastRoutes` 多处调用（每请求约 3–4 轮）；skipped 日志的非空路径还会 `sort.Strings`，`Debugf` 的参数+格式化在 debug 关闭时也会求值（见 B22） | 中 | ✅已读码 |
| A25 | `internal/gateway/gateway.go:1318-1354` | 每次 attempt：`req.Clone`（深拷贝 headers）+ `swaputil.ExtractModel(outReq)` **全量读 body**，名字不同再 `ReplaceRequestModel`（再读一次 + sjson 改写 + 复制）。失败转移多候选时按 attempt 数放大 | 中 | ✅已读码 |
| A26 | `internal/gateway/gateway.go:1376` | 每次 attempt 新建 `http.Client` + `WithContext` → 额外的 client/请求克隆分配 | 低 | ◐待复验 |
| A27 | `internal/gateway/gateway.go:1439` + `:1673` | `decodeSnippet` 每次成功路径对最多 8 KiB 做 `string(raw)` | 低 | ◐待复验 |
| A28 | `internal/gateway/gateway.go:1985-1998` | transport 缓存 `get` 每 attempt 取全局锁。命中便宜，但仍是高 QPS 下的串行点 | 低中 | ◐待复验 |
| A29 | `internal/server/server.go:1597-1612` | `gatewayBodyFingerprint` 每请求**无条件** `make([]byte, 1<<20)` 并 `io.ReadFull` 最多 1 MiB（小 body 也按 1 MiB 缓冲）；随后再从 GetBody 还原流 | 中 | ✅已读码 |
| A30 | `internal/server/server.go:1287-1321` | `serveGateway` 缓存整个请求 body（≤64 MiB）贯穿请求生命周期；每个候选 `bytes.NewReader` 复用；跨方言候选另有 `convertProtocolRequest` 的整份 body + 中间编解码缓冲 → 同时 2 份以上 body | 中高 | ✅已读码 |
| A31 | `internal/gateway/gateway.go:1268-1296` | `bufferRequestBody` 在 `GetBody==nil` 时再读最多 64 MiB（生产路径通常已有 GetBody，嵌入方/测试会付） | 低 | ◐待复验 |

### A3. 转换器（pkg/dialect）

| 编号 | 位置 | 事实与影响 | 级别 | 复核 |
| --- | --- | --- | --- | --- |
| A32 | `pkg/dialect/internal/codec/stream.go:298-309`（`last.Text += part.Text`）、`:271-274`（`ArgumentsText += delta`） | 累积的规范响应在**每个 delta** 上做字符串拼接 → O(n²) 字节复制。实测（Go bench，Apple M5）：1000 帧 4.3 ms/7.0 MB → 4000 帧 16.4 ms/59.6 MB → 16000 帧 **114.6 ms/755 MB**；Claude/Gemini/Responses 三个目标一致（Gemini 16k: 96 ms/768 MB；Responses 16k: 119 ms/754 MB）。分配次数线性（66 次/帧）但字节数按 n² 增长，坐实拼接是根因 | **高** | 📊已实测 |
| A33 | `pkg/dialect/internal/codec/stream.go:105-133` + `jsonx/json.go:12-23` | 每个 SSE chunk 新建 `json.Decoder`+`bytes.Reader`+完整 `map[string]any`（UseNumber）解码，无池化；`jsonx.MarshalJSON` 每次新建 `bytes.Buffer`+`json.Encoder`。实测单帧转换 6.6 µs、**128 次分配/帧**（bench_test.go 的 `BenchmarkStreamChunk_ChatToClaude`） | 高 | 📊已实测 |
| A34 | `pkg/dialect/internal/codec/stream.go:986-998/2203-2222` | `chunk/chunkWithFinish/responseObjectWith` 每帧新建 map；`responseObjectWith` 每次调用都 `encodeResponsesTools(request.Tools)` 重编码整份工具表（`response.created/in_progress/completed` 各一次） | 中高 | ◐待复验 |
| A35 | `internal/protocol/relaykit/convert.go:195-215` | `EncodeSSE` 每帧 `"data: "+string(value.Data)+"\n\n"`（两次 string 转换+拼接） | 中 | ◐待复验 |
| A36 | `internal/server/protocol_middleware.go:641-725` | `takeSSEEvent` 每事件 `append([]byte(nil), ...)` 复制 + 从缓冲区头部重新 `bytes.Index`；`sseData` 对每个事件 `string()`+`ReplaceAll`+`Split`+`Join`。约 4 次事件级分配/帧 | 中 | ◐待复验 |
| A37 | `internal/server/protocol_middleware.go:788-806` | `captureCompletedResponse`：仅 OpenAIResponses 客户端；对**每个**重编码帧 `bytes.Split`+`json.Unmarshal` 只为取 `response` 键 → 第二次 JSON 解析/帧 | 中 | ✅已读码（守卫确认，解析主体部分复核） |
| A38 | `pkg/dialect/internal/codec/decode_stream.go:14-15` | 非流式"回放捕获流"路径：`bufio.Scanner`(16 MiB max token) + `strings.Join` + 全量重 Feed。仅会话/原生捕获调用 | 中 | ◐待复验 |
| A39 | `pkg/dialect/internal/codec/encode_request.go:228/762/1194/1320/1461` | 编码请求时多处 `json.Unmarshal(raw,&value)` 处理透传 schema/input；与工具 schema 大小成正比，每请求一次 | 低中 | ◐待复验 |
| A40 | `internal/server/protocol_middleware.go:464-503` | 非流式转换响应：整份缓冲（≤64 MiB）+ 再复制一份 + ConvertResponse 解码编码 → 3 份响应副本 | 中 | ◐待复验 |
| A41 | `internal/server/protocol_middleware.go:762-786` | 转换流每帧复制进 `w.captured`（≤4 MiB）保留到请求结束 | 中 | ✅已读码 |

### A4. 本地路由 / 调度器 / 选择器

| 编号 | 位置 | 事实与影响 | 级别 | 复核 |
| --- | --- | --- | --- | --- |
| A42 | `internal/matrix/program.go:329-395/498-570` | `Program.Solve` **每请求**执行（`FIFO.OnRequest→EvictionFor`，且在快路径之前）：分配 relevant、evaluator(memo map)、每节点 bitset，遍历所有 set 且每个状态内再扫 running；成本 ≈ sets×states×running。矩阵大时显著 | 中高 | ◐待复验（已有 `BenchmarkMatrixSolver_Solve` 可复现） |
| A43 | `internal/router/scheduler/fifo.go:1303-1326` + `base.go:1774-1799` | `runningSet()` 每请求：去重 map + 切片 + `effects.RunningModels()` 再建 map（逐 process 读 State）+ `sort.Strings` | 中 | ◐待复验 |
| A44 | `internal/router/base.go:313-315/490-505` | 每个请求完成（`serveDone`）都 `publishLifecycleStatuses`：`ModelStatuses()` 全量 map + `reflect.DeepEqual` 对比上一快照 → O(生命周期模型数)/请求 | 中 | ◐待复验 |
| A45 | `internal/router/scheduler/fifo.go:1229-1242/1419-1435` | `enqueue` 是 O(n) 插入 + `broadcastQueuePositions` 对全体排队者发消息；`drainQueue` 完成时对每个排队请求重跑 `ModelState/EvictionFor`。仅在积压时明显 | 低中 | ◐待复验 |
| A46 | `internal/server/selector.go:105/227-278` | 配了 selector 时每请求 `ExtractModel` 全量读 body（同 A03/A16）；`strategy: failover` 会**先把整份响应缓冲完再交给客户端**（见 A47） | 中高 | ✅已读码 |
| A47 | `internal/server/selector.go:218-280` | failover 选择器：请求全量读并持有；每个目标 `bufferedResponseWriter` 全量缓冲响应（≤64 MiB），**成功前一个字节都不发给客户端** → 流式体验退化为"整段等待"，且内存=并发数×响应体 | 高（failover 用户） | ✅已读码 |
| A48 | `internal/server/selector.go:319-395` | `strategySpillover` 每请求在持 `state.mu` 下分配 active/cold 切片 + 多轮扫描；O(targets) 小分配 | 低中 | ◐待复验 |

### A5. 其他每请求成本

| 编号 | 位置 | 事实与影响 | 级别 | 复核 |
| --- | --- | --- | --- | --- |
| A49 | `internal/server/keycache.go:250-275/305-316` + `internal/auth/keys.go:115-137` | 鉴权对快照里**每个** key 逐一 SaltedHash+常量时间比较，O(#keys) 次 SHA-256/请求；legacy keys 另有一轮常量时间比较循环 | 中 | ✅已读码 |
| A50 | `internal/server/keycache.go:101-119` | `recordTouch` 每请求取全局 `touchMu` 写 map | 低 | ◐待复验 |
| A51 | `internal/server/extensions_middleware.go:185-227` | 扩展启用时每请求：`extensionSession`、`requestLocale`(`strings.Split`)、`extensionModelSnapshot`（全模型切片+排序）、`manager.Match`（见 E04）、`s.mcpExtensions`（见 E03）、每请求新建 `extensionLogRotation` | 中 | ✅已读码 |
| A52 | `internal/extensions/manager.go:379-392` + `types.go:226-274` | `Match` 每次调用重建并排序全部扩展 ID，再对每条 manifest 的每个 pattern 跑 `path.Match`（每次解析 glob） | 中 | ◐待复验 |
| A53 | `internal/server/config` 读取 | `currentConfig()` 按值返回 `config.Config`（大量 map/slice 字段）被每请求多处调用。浅拷贝，成本低但值得知道 | 低 | ◐待复验 |
| A54 | `internal/server/resource_api.go:135-170` | 启用 resourceBudget 时每请求：`currentConfig`、`reconcileResources`（全模型 map 分配）、`planner.PlanLoad`（持 planner 锁）；QueueLoads 开启还会建 Ticker | 中（需开启） | ◐待复验 |
| A55 | `internal/server/metrics_middleware.go:47` → `server.go:652-671` | 每 POST 对 3 个切片（~40 条路径）线性匹配 | 低 | ◐待复验 |
| A56 | `internal/server/metrics.go:1594-1608` | 每请求为过滤 Accept-Encoding 分配 map+切片 | 低 | ◐待复验 |

---

## B. 流式响应路径：inflight / 指标 / 日志

### B1. inflight 追踪（每 chunk）

| 编号 | 位置 | 事实与影响 | 级别 | 复核 |
| --- | --- | --- | --- | --- |
| B01 | `internal/server/inflight.go:612-640` → `:258-292` → `inflight_output.go:56-83/129-160` | **每个响应 Write 都进入追踪器全局 `t.mu`**，在锁内执行 `outputParser.Append`：`json.Valid` 扫累计 body（≤16 KiB）、逐 SSE 行 `json.Valid`+`json.Unmarshal`、再 `gjson.ValidBytes`+`gjson.ParseBytes`+`collectInflightOutput`。中间件在两条链分支都挂载（A01），即**无条件**。这是单条流式延迟的最高频固定成本，且锁是**全请求共享**的（并发流互相排队） | **高** | ✅已读码 |
| B02 | `internal/server/inflight.go:137-162/289/310-323` | 每请求 `headerMap`+`redactHeaders` 两个 map、clientIP、channel send；250 ms 节流 `time.AfterFunc`；outbox 满则丢快照置 `needsSnapshot`（128 槽） | 低中 | ✅已读码 |
| B03 | `internal/server/inflight_output.go:85-100/67-69` | 每请求常驻 16 KiB `body` + 16 KiB `pending` 缓冲 + preview 字符串；增长有界 | 低 | ✅已读码 |

### B2. 指标记录（每请求）

| 编号 | 位置 | 事实与影响 | 级别 | 复核 |
| --- | --- | --- | --- | --- |
| B04 | `internal/server/metrics.go:1610-1691` | `responseBodyCopier` 把整份响应复制进内存，上限 **64 MiB/请求**；每次 Write append 一条 `responseBodyWrite`（head/tail 各 512 条后摊还压缩）。SSE 长答复 = 每并发请求一份完整副本直到请求结束 | **高**（内存） | ✅已读码 |
| B05 | `internal/server/metrics.go:347-410` | 请求 goroutine 上对保留的 body 做 **5 遍完整扫描**：`StreamingContentTimes`（逐行 gjson）、`processStreamingResponse`（逐行 gjson）、`streamingSpeedTimeline`（逐行 gjson + `[]rune` 转换）、`applyUsageTelemetry`（再一遍）、`applySessionUsage`（先整份 `json.Unmarshal` 成 map，再逐行 Unmarshal）。O(body)×4–5 + 多次 map 分配 | **高** | ✅已读码 |
| B06 | `internal/server/metrics.go:1773-1833` + `speedtimeline.go:140-154` | 每个内容帧的 `writeAt` **线性扫描** write 记录（最多 1024 条）→ O(帧数×1024)，且内容时间与速度时间线各做一遍（`sort.Search` 可替换） | 中 | ✅已读码 |
| B07 | `internal/server/metrics.go:148-159` | `queueMetrics`：`InsertActivity` **同步**在请求 goroutine 上执行（5 s 后台 ctx），无批处理/异步队列；配合单连接 + WAL-FULL（C01/C02），每个请求都吃一次 commit fsync 并排队 | **高** | ✅已读码 |
| B08 | `internal/server/metrics.go:601` | `persistAudit` 也同步；在交给审计库前还 `append` 复制两份 body（≤64 MiB 各），并 json.Marshal/redact/unmarshal 头部 | 高 | ✅已读码 |
| B09 | `internal/server/metrics.go:486-530/445-449` | `applyEstimatedCost` 每请求执行；价格缓存 miss 时用 `context.Background()` 在**同一条单连接**上 `FindPrices`（5 min TTL/模型） | 低中 | ✅已读码 |
| B10 | `internal/server/metrics.go:559-695` | 审计+StoreMedia 开启时，multipart 请求被 `httptest.NewRequest`+`ParseMultipartForm` 重新解析（落临时文件），每个文件 ≤8 MiB base64 进 JSON，再 zstd 压缩，全同步 | 中高（音频/图像端点） | ◐待复验 |
| B11 | `internal/server/metrics_middleware.go:65-81` | 审计开启时对每个计量 POST 全量 ReadAll（≤64 MiB）+ ReplaceRequestBody，先于"是否需要审计"的判断 | 中高 | ◐待复验 |

### B3. 审计库（auditstore）

| 编号 | 位置 | 事实与影响 | 级别 | 复核 |
| --- | --- | --- | --- | --- |
| B12 | `internal/store/auditstore/segment.go:151-190` | **zstd 压缩 + 文件写在全局 `s.mu` 内** → 并发审计写入按 CPU 级压缩串行化，一个 64 MB body 会卡住其他所有审计写入 | 高 | ◐待复验 |
| B13 | `internal/store/auditstore/store.go:303-360/447-462` | `recordBlock` 用 `s.db.Exec` 在事务**之外**写块索引；一次审计插入 = 1 查询 + N 个 blocks 写 + body upsert + N 个 refcount 更新 + session upsert，语句数随消息数增长 | 中高 | ◐待复验 |
| B14 | `internal/store/auditstore/query.go:264-382` | `PurgeOverBudget`：`SELECT ... ORDER BY ts_created` 把**全部行**载入内存，再逐 id 一个事务删除（每个都重扫 body refs）→ O(N) 事务 + O(N) 重扫 | 高 | ◐待复验 |
| B15 | `internal/store/auditstore/query.go:504-576` + `segment.go:340-357` | `CompactSegments` 每小时：载入全部 live block hash，然后**对每个 segment 遍历整个 refs map**，再 `DELETE FROM blocks WHERE refcount<=0` 全扫 → O(segments×blocks + 磁盘字节) | 中高 | ◐待复验 |
| B16 | `internal/store/auditstore/query.go:543` | `SELECT COUNT(*) FROM blocks WHERE segment=?`：`blocks` 无 `segment` 索引 → 每个 segment 一次全表扫，保留策略 = O(segments×blocks) | 中 | ◐待复验 |
| B17 | `internal/store/control.go:1050-1063` + `audit.go:153-155` | 审计列表页每行一次跨库 `SELECT ... FROM activity WHERE id=?`（N+1，最多 500 行/页，还在单连接上） | 中高 | ◐待复验 |
| B18 | `internal/store/control.go:1414` + `:184` | `/v1/responses` 每次响应把最多 4 MiB BLOB upsert 进 operational DB（同步、单连接，5 s 超时）；后续请求还要整份读回+Unmarshal | 高（Responses 工作负载） | ◐待复验 |
| B19 | `internal/store/control.go:1306/1333-1374` | 内存版审计维护：`SUM(size_bytes)` 全扫；超预算删除用窗口函数+`NOT IN (... LIMIT 1)`（全扫+排序）；blob GC 两次 `SELECT DISTINCT`。仅内存 store 命中 | 低 | ◐待复验 |

### B4. 日志管道

| 编号 | 位置 | 事实与影响 | 级别 | 复核 |
| --- | --- | --- | --- | --- |
| B20 | `internal/logmon/logging.go:381-394` | `Debugf/Infof/Warnf/Errorf` 先 `fmt.Sprintf` 再进入 `log()` 判级 → debug 关闭时仍在格式化。`latency.go:264-266` 这类"只在 skip 时打"的调用也在付 | 中 | ✅已读码 |
| B21 | `internal/logmon/logging.go:183-225` | 每次 `Write`：环形缓冲副本 + `make([]byte,len(p))` 堆拷贝（进广播 channel）+ 同步 `stdout.Write`。子进程每 ~32 KiB 输出付一次，链路上还有 `DrainWriter` 的每写一次拷贝（`drain.go:74-89`） | 中 | ✅已读码 |
| B22 | `internal/logmon/logging.go:228-247` | `GetHistory()` 每次调用复制整个环形缓冲（≤1 MiB），回绕时再复制一次；调用者：`/logs`、每次 `/api/events` 连接（proxy+upstream 两处）、日志流连接、崩溃归档、每次进程替换（+ `SeedHistory` 第三次复制） | 中 | ✅已读码 |
| B23 | `internal/server/apigroup.go:1100-1116` | SSE 扇出：对**每个**订阅者单独 `json.Marshal` 每个日志/活动分片（O(客户端×日志行) 分配）；`sendModels` 每次状态/进度事件都重建并序列化整个模型列表（含 per-model capabilities 解析） | 中 | ◐待复验 |
| B24 | `internal/perf/monitor.go:351-359` + `internal/ring/buffer.go:29-39` | `/api/performance`、`/metrics` 每次轮询 `Slice()` 复制全缓冲（默认 1 h/720 点），`?after=` 也要先复制全部再过滤 | 低 | ◐待复验 |
| B25 | `internal/server/log.go:151/74` | 日志流连接每次 `GetHistory()` 全量复制；1 s 重解析监控对象 | 低中 | ◐待复验 |
| B26 | `internal/logmon/prefix.go:39-53` | 前缀写入器每行一次 `append([]byte(w.prefix), ...)` 分配；`carry` 前切片导致底数组不可回收（上限 16 KiB） | 低 | ◐待复验 |

---

## C. 存储与查询（operational DB）

| 编号 | 位置 | 事实与影响 | 级别 | 复核 |
| --- | --- | --- | --- | --- |
| C01 | `internal/store/store.go:233`（operational）、`auditstore/store.go:206`（audit） | 两个库都 `db.SetMaxOpenConns(1)`：所有插入、聚合、维护、N+1 查询共用一条连接。一次 1–3 s 的聚合会**阻塞所有**在飞请求的落库（`database/sql` 等待） | **高** | ✅已读码 |
| C02 | `internal/store/store.go:236-249` | WAL 开了，但 operational DSN **没有 `_pragma=synchronous`**（audit DSN 才是 `NORMAL`）→ 默认 FULL，每次 commit 都 fsync WAL。每请求一次 fsync 的延迟地板 | **高** | ✅已读码 |
| C03 | `internal/store/store.go:112-126/416/420-431/629-641` | 排序白名单含 `output_tokens/tokens_per_second/duration_ms/...` 等**无索引**列：`ORDER BY` 走全表扫 + temp B-tree；前置 `SELECT COUNT(*)` 是第二遍全扫；深 OFFSET 更糟 | **高** | ◐待复验（列与索引已读码） |
| C04 | `internal/store/control.go:1801-1835`、`store.go:545-575` | `aggregateActivity` 把**全部匹配行**拉进 Go 折叠（不用 SQL SUM/GROUP BY）；`ActivityStats` 再全扫一遍做直方图并对值排序两次 → 一次 `/api/metrics/stats` = 2 次全表扫 + 2×O(n log n) | **高** | ✅已读码 |
| C05 | `docs/configuration.md:937` + `internal/server/metrics.go:153-156` | `audit.activityRetention` 默认 `0` = **磁盘 store 永不裁剪**（只有内存 store 走 `PruneActivity`）→ C03/C04/C06/C08/C09 的扫描量随运行时间单调增长 | **高** | ✅已读码（配置项+守卫） |
| C06 | `internal/store/usage.go:175-216/234/279-319` | `/api/usage/analytics` 串行 6 次全表聚合（含 4 个 AVG、3 个 GROUP BY）+ 时间趋势 `GROUP BY (ts_created/?)`（temp B-tree）；group/key 维度每行还要算相关子查询；**无请求间缓存** | 高 | ◐待复验 |
| C07 | `internal/store/usage.go:360-421/440-486` | `/api/usage/records`：LEFT JOIN + 按表达式/无索引列排序（整结果排序）+ 前置 COUNT；`UsageFilterOptions` 另加 3 个 `SELECT DISTINCT <expr>` 全扫 | 中高 | ◐待复验 |
| C08 | `internal/store/activity_speed.go:159-201` | `/api/metrics/speed` 把窗口内**每行**流进 Go 分桶，无 start/end 时先 MIN/MAX 且默认桶跨全历史；UI 30 s 轮询 + 10 s 事件防抖 | 高 | ◐待复验 |
| C09 | `internal/store/power.go:527-546` | 功率归因是 Go 里 `samples × intervals` 双层循环；90 天保留 + 1 min flush 下一张卡 30 天窗口约 4.3 万行 × 每个占用区间；`PowerReport` 还会再读一遍全部样本并跑两个 SUM | 高（长窗口） | ◐待复验 |
| C10 | `internal/store/power.go:190-192` | 保留删除 `WHERE ts_end < ?` / `ended_ts != 0 AND ended_ts < ?`：`power_samples` 只索引 `ts_start`，`model_gpu_intervals` 只有 partial unique(ended_ts=0) → 两条都是全表扫（每 24 h 一次，单连接） | 中 | ◐待复验 |
| C11 | `internal/store/power.go:367-381` | 区间查询 `started_ts < ? AND (ended_ts = 0 OR ended_ts > ?)`：partial index 只覆盖开区间，OR 谓词无可用前导列 | 中 | ◐待复验 |
| C12 | `internal/store/control.go:1730` | `FindPrices` 按 `model` 单列过滤，而 PK 是 `(provider, model)` → 全表扫 catalog（5 min TTL/模型，用不可取消 ctx） | 低中 | ◐待复验 |
| C13 | `internal/store/extension_pending.go:50` | 每次延迟轮次保存执行 `DELETE ... WHERE id NOT IN (SELECT pending_id FROM extension_pending_calls)`（双表扫）+ 逐 call_id 删除 | 低中 | ◐待复验 |
| C14 | `internal/store/protocol_file.go:42` | 每次删除文件执行 `DELETE FROM protocol_file_blobs WHERE digest NOT IN (SELECT digest FROM protocol_files)` 反连接全扫 | 低 | ◐待复验 |
| C15 | `internal/store/model_downloads.go:217-248` + `modeldownload/manager.go:775` | 下载 worker 空闲时每 **500 ms** 开一个写事务（SELECT+UPDATE+SELECT，双索引路径+排序）→ 常态周期性写者，可能阻塞等待中的聚合 | 中 | ◐待复验 |
| C16 | `internal/store/control.go:623-641` | `FindAPIKeyBySecret` 退化路径 `ListAPIKeys` + 逐 key 常量时间哈希（与 A49 同族，store 直连路径） | 中 | ◐待复验 |
| C17 | `internal/store/control.go:1090-1101/427` | activity 列表投影包含 `speed_timeline`（每请求最多 120 点）与 `metadata_json`；页大小可到 1000 → 单页可带数 MB 而 UI 不渲染该列 | 中 | ◐待复验 |
| C18 | `internal/store/mcp_approval.go:27-31` | ~~`mcp_approvals` 无任何清理~~ **更正（复验线程指出，主线程已读码确认）**：插入时确实带清理，但方式是**每次插入在同一个事务里**执行 `DELETE WHERE expires_at<?`（全表扫 expires_at）+ 按 owner 的 `COUNT(*)+SUM(length(...))` 配额检查（扫该 owner 全部行，含大 BLOB 长度计算）+ INSERT。即"有清理但把成本放在插入路径"，且在单连接上串行。行数有界（每 owner ≤64 条 / ≤64 MiB），**不构成无界增长** | 中（插入成本）/ 无界风险解除 | ✅已读码（更正） |
| C19 | `internal/store/cache.go:124-137` | `PruneCacheEntries` / `PruneExpiredExtensionKVs` **无调用者** → `cache_entries`、`extension_kv` 只在 get 时惰性过期，表无界增长 | 低中 | ◐待复验 |
| C20 | `internal/store/store.go:590` | `PruneActivity` 仅内存 store 调用（见 C05）；`DELETE ... id <= MAX(id)-?` 在磁盘库上也不便宜（若启用需配索引/分批） | 低 | ✅已读码 |

---

## D. 进程生命周期 / 运行时 / 模型下载

### D1. 冷启动与停止延迟（用户可见）

| 编号 | 位置 | 事实与影响 | 级别 | 复核 |
| --- | --- | --- | --- | --- |
| D01 | `internal/process/process_command.go:930-936` | 每次冷启动**固定 250 ms** 等待后才做第一次健康探测（`case <-time.After(250*time.Millisecond)`），对秒级就绪的后端也是固定税 | 中 | ✅已读码 |
| D02 | `internal/process/process_command.go:1050-1057` | 就绪探测 1 Hz（`time.After(time.Second)`）→ 就绪延迟均匀落在 [0,1 s)，叠加 D01 | 中 | ✅已读码 |
| D03 | `internal/process/process_command.go:1028-1029` | 每次健康探测用 `httptest.NewRecorder` 完整缓冲 `/health` 响应（`parseLoadingProgress` 只看 ≤64 KiB） | 低中 | ◐待复验 |
| D04 | `internal/process/vllm_warmup.go:20/119/156` | 健康检查通过后还会发一次真实 chat completions warmup（30 s 预算），ready 被推迟；每个被接受的模型名都可能各试一次 | 中（设计） | ◐待复验 |
| D05 | `internal/process/process_command.go:1145-1190` | `killProcess`：优雅期（默认 10 s）→ SIGKILL → 最多再等 30 s；`doSwap` 先 Stop 再 EnsureReady，会拒绝 SIGTERM 的模型可让换模延迟 ~40 s | 中 | ◐待复验 |
| D06 | `internal/router/base.go:1531-1572/2033-2052` | 换模期间请求只 select `Respond/ctx.Done/shutdown`，**没有加载侧超时**；冷启动 90 s 的模型会让排队请求等到 hcTimeout（默认 120 s） | 高（用户可见） | ◐待复验 |
| D07 | `internal/process/process_command.go:765-780` | 反代与 Transport 是**每次进程启动建一次**（好），但 stop 时从不 `CloseIdleConnections()` → 每次重启留下一池指向死端口的空闲连接（默认 90 s 后才回收） | 低 | ◐待复验 |
| D08 | `internal/process/process_command.go:1405-1471` | `sleepVLLM`/`Wake` 每次新建 `&http.Client{}`（零值 Transport = 新建默认 transport），而每个落到 sleeping 模型的请求都会触发 Wake | 中（sleep 模式） | ◐待复验 |
| D09 | `internal/process/process_command.go:826-850` | `logmon.NewDrainWriter` 在 `cmd.Start()` 之前建立；`Start` 失败直接 return 而未 `Close()` → 每次失败启动泄漏一个投递 goroutine + 256 槽队列（失败重试场景） | 中 | ◐待复验 |
| D10 | `internal/router/base.go:749-758` | 每次进程替换 `GetHistory()`（≤1 MiB 复制）→ `SeedHistory(append(history, marker...))`（再复制） | 低 | ◐待复验 |
| D11 | `internal/process/process_command.go:630-646` | 每个配置 `unloadAfter` 的就绪模型一个 1 Hz goroutine（读 State/inflight/lastUse）。N 模型 N 个定时器 | 低 | ◐待复验 |

### D2. 运行时管理

| 编号 | 位置 | 事实与影响 | 级别 | 复核 |
| --- | --- | --- | --- | --- |
| D12 | `internal/runtime/manager.go:1420-1443` | `Detail` 每次重新 `readState` + `readOperations`，不用内存 statusView；`/api/runtimes/{name}`、readiness 检查、`VersionCatalog` 都会调 | 中高 | ✅已读码 |
| D13 | `internal/runtime/manager.go:3246-3339` | `readOperations` 读日志尾最多 **8 MiB** 并**逐行 `json.Unmarshal`**，最后才裁到 100 条 → 长期运行的实例每次 Detail 解析 8 MiB JSON | 高（老实例） | ✅已读码 |
| D14 | `internal/runtime/autoupdate.go:536/655-688` | ticker 1 min；短路条件是 `now-LastCheck < CheckEvery(24h) && Available == ""` → 一旦发现候选未被启用（等空闲），**每分钟**重跑 provider 检查：容器是 `docker manifest inspect`（网络），git 是 `git ls-remote`，并每次 fsync 写状态 | 高（有 pending 时） | ✅已读码 |
| D15 | `internal/runtime/autoupdate.go:952-977` | `saveAutoStatus` 持全局 `m.mu` 走 `writeState`（临时文件 + fsync + rename + syncDir）→ 全局锁跨两次 fsync，`Definition`/`Configure` 类调用被磁盘 I/O 阻塞 | 中 | ◐待复验 |
| D16 | `internal/runtime/manager.go:3198-3244` | `appendOperation` 持跨进程文件锁并 `f.Sync()`/条；多处调用点**持 `m.mu`** 调用 → 一次 runtime check 让 manager 卡在日志 fsync 上 | 中 | ◐待复验 |
| D17 | `internal/runtime/manager.go:661-694` | 构建日志缓冲超过 256 KiB 后，**每个 chunk** 都 `append([]byte(nil), output[len-256KiB:])` 复制 256 KiB（O(chunks×256 KiB)），且在 `logBufMu` 下；`RuntimeOperationLog` 每次 UI 轮询（1.5 s）再 `string(buf.output)` 复制 256 KiB | 中 | ◐待复验 |
| D18 | `internal/runtime/container.go:541-560/668-684` | `ContainerProvider.Health` = `Verify` → `docker image inspect` 子进程，每次激活/检查都跑，无缓存 | 中 | ◐待复验 |
| D19 | `internal/runtime/container.go:351` | 未钉版本时每次更新检查 `docker manifest inspect --verbose`（网络）；叠加 D14 即每分钟一次 | 中高 | ◐待复验 |
| D20 | `internal/runtime/providers_lmcache.go:763-885` | 默认 LMCache server 探针：真实启服务 + 250 ms 轮询 `/healthcheck`（默认 90 s 超时）+ 5 s SIGTERM 升级，可能整体重试一次；从 Health/Verify/Activate 触发 | 中 | ◐待复验 |
| D21 | `internal/server/lmcache.go:866-905/115-136` | `/api/lmcache/status` 对每个 active vLLM venv **spawn `python -c 'import importlib.metadata...'`**（并发，120 s 预算）+ 读 manifest；UI 每 **10 s** 轮询 → 冷 GPU 主机上反复付 torch import 级代价 | **高** | ✅已读码 |
| D22 | `internal/server/lmcache.go:317-345` | STARTING 期 2 s、RUNNING 后 10 s 的 health GET + 1 KiB 读，进程生命周期内不停 | 低 | ✅已读码 |
| D23 | `internal/runtime/manager.go:865-950/1220-1236` | 启动时按目录扫描 + 逐 runtime `readState`/`readManifest`，`recoverPointers` 可能 fsync；`Get` 是 O(runtimes) 线性扫（`List` 已做原子缓存，正常） | 低 | ◐待复验 |

### D3. 模型下载

| 编号 | 位置 | 事实与影响 | 级别 | 复核 |
| --- | --- | --- | --- | --- |
| D24 | `internal/modeldownload/manager.go:1534` | 每个 range 请求先 `io.ReadAll(want+1)` 按块大小（默认 16 MiB）一次分配；4 worker 并发 ≈ 64 MiB 常驻 + GC 压力 | 中 | ◐待复验 |
| D25 | `internal/modeldownload/manager.go:1475-1484` + `:1414-1443` | **每 16 MiB 块**：`file.Sync()` + `json.Marshal` 整个 manifest + 临时文件 fsync + rename + `sort.Ints(Completed)`；10 GiB 文件 = 640 次数据 fsync + 640 次 manifest 重写 | 高（机械盘/网络盘） | ✅已读码 |
| D26 | `internal/modeldownload/manager.go:1477-1480`（同上） | `sort.Ints` 每块从头排序 + `state.mu` 串行化 4 个"并行"worker → 有效吞吐 ≈ 1/fsync 延迟 | 中 | ✅已读码 |
| D27 | `internal/modeldownload/manager.go:1719/1813/1935/2012-2033` | 无 per-chunk 校验和：单块损坏只在**全文件哈希**后才发现，并 `removeChunkArtifacts` 丢弃全部进度；resume 路径可能对同一文件哈希两次 | 中 | ◐待复验 |
| D28 | `internal/modeldownload/manager.go:908-938` | `persistLocked` 求和全部文件字节 + 排序 + 一条 UPDATE + SSE；`progressBytes/progressInterval` 节流，但**每个文件完成/激活强制写一次** → 千文件仓库 = 千次 DB 写 | 中 | ◐待复验 |
| D29 | `internal/modeldownload/manager.go:775` | worker 空闲每 500 ms 一次 claim 事务（同 C15） | 低 | ◐待复验 |
| D30 | `internal/modeldownload/manager.go:1889` | `io.CopyBuffer` 每次 attempt 重新 `make([]byte, 64*1024)` | 低 | ◐待复验 |

---

## E. 扩展 / MCP

| 编号 | 位置 | 事实与影响 | 级别 | 复核 |
| --- | --- | --- | --- | --- |
| E01 | `internal/extensions/worker.go:224` | **每次 hook 调用 fork/exec 一个新的 `-extension-worker` 子进程**（无池化；`poolSlots` 只限并发 2）。一轮对话 = onRequest + N×onToolCall + onResponse 多次进程创建 | **高** | ✅已读码 |
| E02 | `internal/extensions/worker.go:445-455` | 新进程里 `goja.New()` + `vm.RunString(整个 bundle)`：JS 编译 + 模块级初始化**每次 hook 重付** | 高 | ◐待复验 |
| E03 | `internal/server/mcp_runtime.go:125-137` + `internal/mcpruntime/client.go:28-66` | MCP 启用时**每个推理请求**调用 `Discover`：对每个 MCP server `Connect`（stdio 拉起子进程 / HTTP 会话）并 `tools/list`；无会话或工具表缓存；工具调用时再重连一次 | **高**（MCP 开启） | ✅已读码 |
| E04 | `internal/extensions/manager.go:379-392` + `types.go:226-274` | `Match` 每请求重建+排序全部扩展 ID，逐 pattern `path.Match`（每次编译 glob） | 中 | ◐待复验 |
| E05 | `internal/extensions/manager.go:250-272` + `files.go:194-227` | `StartWatch` 每 **2 s** 对扩展目录全树 `WalkDir`+逐文件 `Info()` 并拼接 `path:size:mtime` 串；`Reload` 时 `directoryETag` 还要**读每个文件内容**哈希 | 中 | ✅已读码（2 s ticker）/◐（ETag） |
| E06 | `internal/server/extensions_logs.go:116-129` | 每条扩展日志 = `MkdirAll`+`OpenFile`+`WriteString`+`Close`（持锁）；扩展每 hook 打 N 行就 N 次文件 open/close | 中高 | ✅已读码 |
| E07 | `internal/extensions/worker.go:92-109` | bundle 随每次调用重新序列化并写进子进程 stdin，且与输入共享 8 MiB 单行上限 → 大 bundle 挤压可用载荷 | 中 | ◐待复验 |
| E08 | `internal/extensions/worker.go:301-320` | 每个在飞 hook 一个 50 ms ticker 采样 gopsutil `MemoryInfo/Times`（Linux 读 /proc）→ 2 槽并发约 40 次/s 进程统计 | 低中 | ◐待复验 |
| E09 | `internal/extensions/manager.go:205-211` | `poolSlots` 惰性初始化无同步：并发首次调用可各建一个 channel，并发上限失效（同时是数据竞争）。`Inspect` 正常路径已初始化，`CompileDraft` 路径可能绕过 | 低（正确性/上限） | ◐待复验 |
| E10 | `internal/extensions/worker.go:1309-1317/1481` | 每次 `ctx.http.fetch` 内联新建 `&http.Transport{}`（未配置代理时）→ 无 keep-alive 复用，每次 fetch 重付 TCP/TLS 握手 | 中 | ◐待复验 |
| E11 | `internal/extensions/images.go:377-421` | `decodeImagePayload` 至少 3 次全尺寸复制（切片、`strings.Map`、`NewReplacer().Replace`）再 base64 解码；32 MiB 输入瞬时 >100 MiB | 中 | ◐待复验 |
| E12 | `internal/extensions/images.go:215-222` + `imagetransform.go:261-268/150-183/219-247` | `toRGBA` 常复制整图 + `hasTransparency` 全像素扫描 + box resize 全源像素 + PNG `DefaultCompression` 单线程编码；25 MP 图多轮全图遍历 + 数百 ms～秒级编码 | 中 | ◐待复验 |
| E13 | `internal/extensions/images.go:663-707` | rotate/flip 用 `source.At()/target.Set()` 逐像素接口调用（~4 次/像素），同文件 `cropTo` 已用 `copy` 直拷 Pix → 可快一个数量级 | 中 | ◐待复验 |
| E14 | `internal/server/extensions_hostcalls.go:57-63` | `images.apply` 在**守护进程内**执行，不受 worker 的 MaxMemory/MaxCPU 看门狗约束，也没有并发信号量 → 模型产出的 32 MiB 图片可在代理进程里吃掉 ~100 MiB 内存 + 秒级 CPU | 高（可用性/爆炸半径） | ✅已读码 |
| E15 | `internal/extensions/workerpump.go:47/62-72` | 每协议行两次 `json.Unmarshal`（探针 + 完整解析） | 低 | ◐待复验 |
| E16 | `internal/extensions/worker.go:181-188` | `Inspect` 在编译期也会 spawn 完整 worker（30 s 预算）→ 每次保存/检查扩展付一次进程+VM | 低 | ◐待复验 |

---

## F. UI（Svelte 5）

| 编号 | 位置 | 事实与影响 | 级别 | 复核 |
| --- | --- | --- | --- | --- |
| F01 | `ui/src/routes/Activity.svelte:222-280` + `node_modules/bits-ui/dist/bits/tabs/tabs.svelte.js:178` | `Tabs.Content` 非激活时只加 `hidden`（**不卸载**）：`/activity` 一进就挂载 **3 个 RequestRecords（各自有 fetch 循环+表格）、2 个 InferenceSpeedPanel（各一 Chart.js）、2 个 EnergyPanel**；隐藏面板的 effect 与 chart.update 照跑 | **高** | ✅已读码 |
| F02 | `ui/src/components/activity/EnergyPanel.svelte:177-186` | 每个 `$activityRevision`（=每个完成请求）**立即** `load()`（拉全量功率聚合），无防抖；叠加 F01 的 2 份实例 = 每请求 2 次功率聚合 | **高** | ✅已读码 |
| F03 | `ui/src/components/activity/RequestRecords.svelte:192-215`、`Activity.svelte:99-106` | `$models` 每次 `modelStatus` 事件被替换（模型加载进度约 1/s/模型）→ 3 个表 + stats 各自**无防抖**重拉；仅 activityRevision 路径有 1 s 地板 | 中高 | ✅已读码（结构） |
| F04 | `ui/src/components/ActivityTable.svelte:333-348` | 只要有在飞请求就一个 **60 Hz `requestAnimationFrame` 循环**写 `$state`（`inflightNowMs`），每个单元格按它重算 → 3 份表 = 3 个 rAF + 每帧全行重算。应为 1 Hz/interval | 中高 | ✅已读码 |
| F05 | `ui/src/components/LogPanel.svelte:65-75` + `lib/ansi.ts:159-207` | `filteredLogs`（split/filter/join）+ `renderedLogs = ansiToHtml(...)` 都是 `$derived`，对 512 KB 尾巴每消息全量走正则并重建 HTML 字符串，`{@html}` 整体替换 DOM；`/logs` 同时挂载 proxy+upstream 两个面板（非活动也只 hidden） | 高（日志量大时） | ✅已读码 |
| F06 | `ui/src/components/LogPanel.svelte:141` | 过滤器输入 `bind:value={filterRegex}` 无防抖，每键重跑 512 KB 的 split+regex+join+ANSI | 中高 | ✅已读码 |
| F07 | `ui/src/stores/api.ts:113-161` | 单一 EventSource：无 `visibilitychange` 守卫（后台标签页继续全量事件处理）；无 jitter；**重连 setTimeout 未保存/清理** → `enableAPIEvents(false)`（登出）后仍可能重开连接 | 中高 | ◐待复验 |
| F08 | `ui/src/stores/api.ts:122-142` | 每次 `onopen` 清空并重填 models/logs/profiles 并 bump `activityRevision` → 触发 F02/F03 的全部重拉 | 中 | ◐待复验 |
| F09 | `ui/src/routes/ModelDetail.svelte:293-310` + `stores/modelLogs.ts:127-140` | `ModelLogsTab` 在被隐藏的 tab 里也挂载并立刻订阅日志流；流正常结束后 `INITIAL_RETRY_DELAY_MS = 250` → 空闲/停止模型下**每 250 ms 重连一次**并重取整段尾巴 | 中高 | ✅已读码 |
| F10 | `ui/src/components/playground/ChatInterface.svelte:80-95/189-216` | 消息数组是深 `$state` 代理，每个流式 token 都触发 effect，effect **先** `JSON.stringify(messages)` 再判断 2 s 节流 → 长对话每个 token 付 O(对话长度) 序列化；列表按 index keyed 且**无上限** | 高（长对话） | ✅已读码 |
| F11 | `ui/src/stores/persistent.ts:17-28` | `persistentStore` 每次 `set` 同步 `localStorage.setItem(JSON.stringify)`，无防抖；翻译模板 `bind:value` 每键写一次 | 中 | ◐待复验 |
| F12 | `ui/src/routes/Settings.svelte:78-80` | `dirty`/`baselineConfig`/`previewChanges` 三个 `$derived`：每键 `JSON.stringify(整个配置)` + `JSON.parse(baseline)` + 全量 diff（字段 oninput 提交） | 中 | ◐待复验 |
| F13 | `ui/src/routes/ExtensionEditor.svelte:59-61/137-140/419-425` | 每键 `JSON.stringify({manifest, files, directories})` 比 dirty；500 ms 后 `POST /api/extensions/check` 整包文件；1.5 s 后整包进 localStorage | 中高（大扩展） | ◐待复验 |
| F14 | `ui/src/components/playground/ConcurrencyInterface.svelte:61/292-299/347-360` | `runs` 是深代理记录：每个 plan 一个 50 ms interval 写新对象 + 每 token 写一次 → `visiblePlans/summary/probeSummary/ledger/timeline` 全部失效重算；N plan ≈ N×20 次状态写/s | 中 | ◐待复验 |
| F15 | `ui/src/components/PerformanceChart.svelte:218-225` + `Performance.svelte:179-192/247-307/365-369` | 每次轮询重建全部数据集并 `update("none")`：每核一条序列、每接口线性 find、5 组 GPU 数据集，缓冲 5400 点 → 每 tick 复制数千点×核数×图表数；隐藏 tab 里的同类图（F01）同样重绘 | 中 | ◐待复验 |
| F16 | `ui/src/components/activity/ModelUsageTable.svelte:47-77`、`routes/ModelFiles.svelte:37-57`、`routes/Extensions.svelte:91-123`、`settings/model/SettingsModelsPage.svelte:118-139` | 搜索/过滤每键克隆+过滤+排序（含 join/lowercase），无防抖；`SettingsModelsPage` 还在每次 modelStatus 上重建行 | 低中 | ◐待复验 |
| F17 | `ui/src/components/playground/ChatInterface.svelte:36/82-95` | playground 消息无上限地进内存代理 + localStorage（对照：日志/性能/扩展控制台都有上限） | 中 | ✅已读码（无上限）/◐（增长） |
| F18 | `ui/src/components/audit/AuditTranscript.svelte:138-145/291-475` | `renderSafeMarkdown`（remark+rehype+KaTeX+highlight.js）无缓存，内联在模板里；`expandAll` 让每个块重跑整条 markdown 管线 | 中（大对话） | ◐待复验 |
| F19 | `ui/src/routes/ExtensionEditor.svelte:14`、`components/settings/ModelSettingsDialog`（构建产物 609 KB） | Monaco / CodeMirror 静态 import：打开 `/extensions/*/editor`、`/models`、`/settings`、模型详情即拉取 ts.worker(6.9 MB)/editor.api(2.7 MB)/CodeMirror(609 KB)；i18n 三语目录 339 KB 预加载 | 中低（加载） | ◐待复验（构建产物已由子代理核对） |
| F20 | `ui/src/components/model-files/ModelDownloadPanel.svelte:390-397`、`routes/Runtimes.svelte:461-471`、`routes/LogViewer.svelte:42-68`、`components/providers/ProvidersPage.svelte:374-384`、`routes/Backends.svelte:97-104`、`routes/Playground.svelte:22-35` | 1.5 s/5 s/10 s 轮询均**无 `document.hidden` 守卫**（Providers 无在飞守卫可重叠）；Playground 离开路由后仍挂载（`class:hidden`），每次 modelStatus 防抖拉全量 `/v1/models` | 低中 | ◐待复验 |
| F21 | `ui/src/routes/Activity.svelte` 各表 | 全站无列表虚拟化（活动表靠服务端分页 ≤500、Usage 200 行），行数有界但每次更新整表重渲染 | 低 | ◐待复验 |

---

## G. 横切：锁、背压、周期任务

| 编号 | 位置 | 事实与影响 | 级别 | 复核 |
| --- | --- | --- | --- | --- |
| G01 | `internal/server/inflight.go:258-292` | 追踪器单锁保护**所有**在飞请求的解析与事件生成（B01）——64 条并发流时全部写路径排队 | 高 | ✅已读码 |
| G02 | `internal/gateway/gateway.go:965-974`、`latency.go:89-99`、`scheduler.go:403-417`、`keycache.go:101-119`、`gateway.go:1985-1998` | 多个**进程级全局互斥锁**在每请求路径上（pool 游标、EWMA、sticky、touch、transport 缓存）→ 高 QPS 下的争用面 | 中 | ✅已读码 |
| G03 | `internal/event/event.go:231-266` | 广播在队列满时阻塞**生产者**；`emitMetric` 在请求 goroutine（A14）→ SSE 消费者卡住可反压推理请求；默认队列 25 000 使该风险在真实负载下较低 | 中 | ✅已读码 |
| G04 | `internal/gateway/pump.go:29-62` + `content_probe.go:202-210/573-595` | 每条供应商流的前 N 个 chunk 被 contentProbe 走一遍 gjson；无内容时最多保留 2 MiB，非 SSE 探测保留 8 MiB | 中 | ◐待复验 |
| G05 | 周期任务汇总 | 见下方"周期任务表" | — | ✅/◐混合 |

### 周期任务表（interval → 工作 → 持锁 → 位置）

| 间隔 | 工作 | 持锁 | 位置 |
| --- | --- | --- | --- |
| 5 s | 系统采样 → ring + 扇出 | Monitor.mutex（短） | `internal/perf/monitor.go:198-221` |
| 5 s | GPU 采集（nvidia-smi/LACT/ioreg/rocm-smi）→ 能积分 + 扇出 | Monitor.mutex | `internal/perf/monitor.go:285-345` |
| 30 s | IPMI 节点功率采样 | Monitor.mutex | `internal/perf/ipmi.go:23,240` |
| 1 min | 功率样本落库 + 归属 reconcile + 24 h 修剪 | DB 连接；跟踪器 mu 内含 DB 调用 | `internal/server/power.go:293-367` |
| 30 s | keyCache 刷新（全表读 + 重建快照） | keyCache.mu 内含 DB 调用 | `internal/server/keycache.go:173-207` |
| 1 min | touch 刷新（逐 key 一条 UPDATE，无事务） | — | `internal/server/server.go:2317-2327` |
| 10 min（插入触发 1 min 防抖） | `maintainAudit`：GC 审计 blob、按时间/预算清理 | `auditMaintMu` 全程 | `internal/server/server.go:2293-2303/2380-2429` |
| 1 h | `MaintainAudit`：DropExpiredSegments + CompactSegments（逐段重写） | 每段 `s.mu` + 单连接 | `internal/store/auditstore/query.go:507-556` |
| 1 h | 过期 response_affinity 删除 | DB | `internal/server/server.go:2226-2248` |
| 24 h | 价格表同步（DELETE + 逐条 INSERT 一个事务） | DB 全事务 | `internal/server/server.go:2198-2219` |
| 30 s | 配额刷新（每个到期凭据一次 HTTP + 缓存修剪） | — | `internal/server/providers_quota.go:455-492` |
| 10 min | 扩展 pending 清理（事务内 2 个 DELETE） | DB | `internal/store/extension_pending.go:139-156` |
| 500 ms | 模型下载 worker claim（写事务） | DB | `internal/modeldownload/manager.go:40,646,774-782` |
| 100 ms / 50 ms | 扩展安装 RSS 看门狗 / hook CPU 看门狗 | — | `internal/extensions/manager.go:803`、`worker.go:301` |
| 10 ms | 运行时切换等待轮询（每 tick 复制生命周期状态 map） | — | `internal/server/runtime_switch.go:129-160` |
| 1 s | `/logs` 流监控重解析 | — | `internal/server/log.go:188-215` |
| 1 s | 每就绪模型 TTL 卸载检查 | — | `internal/process/process_command.go:630-651` |
| 10 s | LMCache health GET | — | `internal/server/lmcache.go:51,319-345` |
| 2 s | 扩展目录全树 stamp（WalkDir+stat） | — | `internal/extensions/manager.go:255-268` |
| 2 s | 配置文件 stat/目录扫描 | — | `internal/watcher/watcher.go:43`、`dirwatcher.go:83` |
| 1 min | 运行时自动更新检查（网络；pending 时实际每分钟） | 网络期间释放 m.mu；writeState 期间持有 | `internal/runtime/autoupdate.go:536,665-688,952-977` |
| 1 h | 扩展日志保留修剪（ReadDir+stat+删） | extensionLogRotation.mu | `internal/server/extensions_logs.go:82-176` |
| 1 min/活跃会话 | 会话租约续期 UPDATE | DB | `internal/server/conversations.go:103` |
| UI 侧 | 见 §F 与 F20 的轮询表 | — | `ui/src/...` |

---

## J. 独立复验线程的补充发现（主线程已逐条读码复核）

| 编号 | 位置 | 事实与影响 | 级别 | 复核 |
| --- | --- | --- | --- | --- |
| J01 | 全仓库（`grep -rn "sync.Pool" internal pkg` 生产代码 **0 命中**） | 每 chunk/每事件的缓冲一律现分配：`jsonx.DecodeObject` 的 `bytes.Reader`+`json.Decoder`、`jsonx.MarshalJSON` 的 `bytes.Buffer`+`json.Encoder`（A33 的 128 allocs/帧）、`forEachSSEJSON`/`processStreamingResponse` 的每事件 Buffer、`takeSSEEvent` 的每事件 `append([]byte(nil),...)`（A36）、`inflight_output.go:140-154` 的每行 Decoder。**全项目没有任何缓冲池化**，这是 A32/A33/B01/B05 的共同放大器 | 中 | ✅已读码 |
| J02 | `internal/gateway/scheduler.go:399-458` | `stickyTable.entries` 是 `map[string]stickyEntry`，只在 **get 命中该 key 且已过期**时惰性删除，以及 `unbind/clearCredential`。**没有周期性清扫**：一次性会话（绑定后再不回来）会永久驻留内存 → 长运行实例的无界增长（量级小：每条约几百字节，但确实无上限）。对照：`anthropiccache.SessionUsage` 有 LRU 上限 | 中 | ✅已读码 |
| J03 | `internal/server/metrics.go:433-441` | `applySessionUsage` 的守卫只有 `sessionUsage != nil && SessionID 非空`，**不判断请求是否 Anthropic**；任何带 session 标识的响应（如 OpenAI 客户端带会话头）都会调用 `anthropiccache.ExtractUsage(body)` → 先整份 `json.Unmarshal` 成 map，再逐 SSE 行 Unmarshal（`cache.go:1106-1139`）。即 A07 之外还有一条**非 Anthropic 路径**上的整 body 解析 | 中 | ✅已读码 |
| J04 | `internal/server/metrics.go:536-550` | 审计持久化的请求/响应头各走 4 遍：`Marshal(headers)` → `Unmarshal` 到 map → `redactHeaders` → `Marshal`（且复用同一个 `values` map 变量，第二个 Unmarshal 会覆盖第一个）。小对象，但每请求固定成本 | 低 | ✅已读码 |
| J05 | `internal/store/mcp_approval.go:27-31` | 见 C18 更正：审批插入在事务内做全表过期删除 + 按 owner 的 COUNT/SUM(length) 配额检查（大 BLOB 长度计算）→ 每次 MCP 审批插入在单连接上多付两遍扫描 | 低中 | ✅已读码 |

---

## H. 复核为"非缺陷 / 设计取舍 / 待实测才能定性"

| 编号 | 位置 | 结论 | 复核 |
| --- | --- | --- | --- |
| H01 | `internal/router/matrix.go:39-42`、`internal/config/matrix.go:100-107` | 矩阵 DSL **编译一次**、`Program()` 返回不可变程序、solver 建一次 → 不存在"每请求重新解析 DSL"。A42 的成本是 Solve 本身而非解析 | ✅已读码 |
| H02 | `internal/runtime/manager.go:1220-1236` | `List` 原子缓存（无锁），`Get` 线性扫描在 runtime 数量级下可忽略 | ✅已读码 |
| H03 | `internal/process/process_command.go:765-780/1449-1471` | 本地请求路径本身**干净**：反代/Transport 进程级复用，每请求只有两个 atomic load + inflight.Add + lastUse.Store | ✅已读码 |
| H04 | `internal/logmon/drain.go:74-89` | DrainWriter 的有界队列 + 静默丢弃是**刻意设计**（防止子进程 stdout 被日志消费者阻塞），非缺陷；代价是高输出期日志丢失标记 | ◐待复验（设计注释已读码） |
| H05 | `internal/swaputil/http.go:56` 等 | 64 MiB 上限是**每请求**而非每进程/全局 → 并发×常量，不是无界泄漏；但见 B04 的并发内存放大 | ✅已读码 |
| H06 | 审计默认存全量 body / 无密钥部署开放 / provider probe 允许内网 / session cookie 存 key | 既往审计已判定的**设计取舍**，与性能审查无冲突（不重复计入） | 历史结论 |
| H07 | `internal/event/default.go:17` 队列 25 000 | 背压阈值高，真实负载下 G03 触发概率低；但"慢消费者永久卡住一条 rclone/SSE"的结构仍在 | ✅已读码 |
| H08 | `internal/server/server.go:183-187` `currentConfig()` 按值拷贝 | 只复制指针字段，量级小；不建议为此改签名（避免引入并发误用） | ✅已读码 |
| H09 | 供应商流 usage 只在末帧给出 | 实时速度无法准确取得（既往结论），本清单不计"缺少实时吞吐"为性能缺陷 | 历史结论 |
| H10 | UI 列表无虚拟化 | 活动表分页 ≤500、Usage 200 行、日志/性能/控制台均有上限 → 行数有界，属可接受；仅 F05/F10 的"每消息全量重算"是真问题 | ✅已读码 |

---

## I. 修复建议顺序（按"收益/风险"排序，供决策）

**第一梯队（每请求/每 chunk 固定成本，改动局部、收益立刻可见）**

1. **B01**：把 inflight 的 SSE 解析移出全局锁（每请求自有 parser 已是事实上的 per-request 结构，只需把解析放到锁外或按请求分片加锁），并对非流式/无预览需求的请求跳过解析。
2. **D21**：`/api/lmcache/status` 的 python 探测加缓存（版本探测结果按 venv mtime/区间缓存），并把 UI 轮询降到分钟级或仅在页面可见时轮询。
3. **A04 / A09 / A05**：给三个"无条件全量解码"的中间件加前置判定——先做一次廉价的字节包含检查（`call_ls_` 已有此思路但放在了读之后）、`StripHostedSearchTools` 先 `bytes.Contains` 判断有无 `web_search`/hosted 工具键、ProtocolFiles 先判定 body 是否引用 `file_`/`gfile_`。
4. **A32（已实测）**：转换器累积文本改 `strings.Builder`/按块聚合（或仅在 Finish 需要的目标上累积），16k 帧可从 755 MB 分配降到 KB 级。
5. **A17/A18/A24**：候选池每请求只构建一次并沿调用链传递；`rotator`/sticky 换成 per-model 锁或原子游标。
6. **F01/F02/F04/F09**：Activity 各 tab 改为按需挂载（或彻底移除隐藏 tab 的图表实例）、EnergyPanel 加防抖、rAF 改 1 Hz、模型日志流加退避+可见性守卫。

**第二梯队（结构性，需设计与基准）**

7. **B04/B05/B07 + C01/C02**：响应体解析由"保留全文 + 5 遍扫描"改为**单遍流式解析**（在 Write 时增量喂 parser）；`InsertActivity`/审计写改异步批量队列；operational DB 设 `synchronous=NORMAL` 或拆读写连接。
8. **C04/C06/C08 + C05**：统计/用量/速度/功率聚合改为 SQL 侧 SUM/GROUP BY + 短 TTL 缓存（3–10 s），并给 `activity` 配一个默认保留上限。
9. **E01/E02/E03**：hook 执行器进程池化（长驻 worker + 复用 goja VM），MCP `Discover` 结果缓存（按 server 配置指纹 + TTL）。
10. **D12/D13/D14/D25/D26**：`Detail` 走内存视图、日志尾按行倒读（只解析最后 100 行）、pending 状态下检查退避到小时级、下载的 fsync/manifest 按 M 级块批量化。

**第三梯队（规模化才会显现，可排期）**

11. C03/C07/C09/C10/C11 的索引与查询改写；B12–B18 审计库事务与压缩路径；E05/E06/E10–E14 扩展工程细节；F05/F06/F10/F12/F13 前端重算路径；A42/A43/A44 路由热路径；J01–J05（per-chunk 缓冲池化、sticky 表清扫、会话用量解析守卫、审计头序列化 4 遍、审批插入扫描）。

**不建议现在动**：H08（`currentConfig` 签名）、H04/H05/H07 的阈值类参数——除非先有生产压测数据。

---

## 附录 A：实测方法与原始数据

**A32（转换器二次增长）**——用 Go `-overlay` 注入临时基准（不改动仓库），源码在 `/tmp/perfaudit/stream_quad_test.go`，overlay 映射到 `pkg/dialect/zz_perfaudit_test.go`：

```
go test -overlay /tmp/perfaudit/overlay.json -run XXX -bench BenchmarkQuadFeed -benchtime 10x -benchmem ./pkg/dialect
```

Apple M5 / go1.26.1 结果（每 op = 一次全新转换器喂 n 帧 `content:"token"`）：

| 目标 | 1000 帧 | 4000 帧 | 16000 帧 |
| --- | --- | --- | --- |
| Claude | 4.31 ms / 7.04 MB | 16.45 ms / 59.6 MB | 114.6 ms / 755.6 MB |
| Gemini | 3.50 ms / 7.82 MB | 15.48 ms / 62.7 MB | 96.1 ms / 768.2 MB |
| Responses | 3.03 ms / 6.95 MB | 14.53 ms / 59.2 MB | 118.8 ms / 754.2 MB |

帧数 ×16 → 时间 ×17–27、分配字节 ×101–108，分配次数线性（≈66 次/帧）→ `last.Text += part.Text` 的字符串拼接即二次项来源。

**A33（每帧固定成本）**：`BenchmarkStreamChunk_ChatToClaude` = 6.64 µs/帧、9150 B/帧、**128 allocs/帧**（120 B 的 chunk）。

**被引用的既有基准**（可用于修复前后对比）：`pkg/dialect/bench_test.go`、`internal/router/matrix_bench_test.go`、`internal/matrix/program_bench_test.go`、`internal/config/matrix_bench_test.go`、`internal/logmon/logging_test.go:332`、`internal/ring/buffer_bench_test.go`。

## 附录 B：本次审查的覆盖面与已知盲区

- 覆盖：后端请求/网关/协议转换/流式与指标/存储与审计/日志管道/进程与运行时/模型下载/扩展与 MCP/UI 全路由轮询与渲染。
- 盲区（未做）：**没有**在生产实例上跑 pprof/pprof-block/mutex profile；没有做端到端压测（并发流下的真实锁争用比例、SQLite fsync 绝对值）；UI 侧未做真实浏览器 Performance 录制（皆为源码级推断 + 子代理交叉核对）。以上三类需要运行环境与授权，标注为"待实测/◐"的条目在修复前应先用真实 profile 确认排序。
- 未纳入：安全、协议正确性、功能缺失类问题（另有专项审计）。


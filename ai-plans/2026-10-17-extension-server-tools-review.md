# 扩展服务端工具改造 —— 独立审查清单

日期：2026-10-17　范围：本工作区未提交改动（扩展"服务端工具可见/延后"四阶段改造）
审查方式：**只读**（不得修改仓库内任何文件；可以构建、跑测试、跑 E2E，产物写到 /tmp）
审查线程：2 个（并发上限）——
- **A 线：机制与状态机**（artifacts/pending/middleware/stream/responses 中间件/store）
- **B 线：契约与运行时**（四表面契约 vs 文档、测试覆盖、真实客户端运行结果）

发现的问题按：`ID(severity) 文件:行 | 现象/证据 | 为什么是缺陷 | 建议修法` 交回；判为误报写 `NOT-A-DEFECT: 理由`；本轮不修写 `DEFERRED: 理由`。

---

## 0. 需求契约（用户批准的设计）

| 表面 | 服务端调用对客户端 | 跨请求机制 | 记录 |
|---|---|---|---|
| Anthropic Messages | 可见：`server_tool_use` + `mcp_tool_result` 成对 | 客户端回显整个 assistant 轮 | 无 |
| Responses | 可见：`mcp_call` item（内嵌 output） | 回显；只发新增项者走 `previous_response_id`（affinity 存**客户端可见**响应） | 无 |
| Chat Completions | 隐藏 | 隐藏 + 延后执行 + 要求重放（否则 400） | 一轮调用 |
| Gemini | 隐藏 | 同 chat，**按工具名**配对 | 一轮调用 |

其它已定契约：`extensions.maxContinuationMiB`（默认 64，0..1024）=**一轮记录**上限，超限把该轮调用方调用降级为失败工具结果（不 502）；续接错误码 = 不完整/无法完成 → 400，状态冲突 → 409，身份不符 → 403。

---

## A. 需求完整性

- [ ] A1 anthropic：非流式混合轮输出成对 artifact；纯服务端轮把各轮 artifact（含中间文本）累积进最终答复；调用方 id **不**被改写（无 `call_ls_`）；**不写** `extension_pending`。
- [ ] A2 anthropic 流式：`content_block_start(server_tool_use)` + `input_json_delta` + `stop`，随后 `mcp_tool_result` 块；调用方自己的调用也走 delta 形（不得内联 input）。
- [ ] A3 responses：非流式 `mcp_call` item（含 `server_label`/`arguments`/`output`/`status`）；流式发 `response.output_item.added/done` 且 `response.completed.response.output` 含 artifact。
- [ ] A4 responses 续接：回显重放折叠成 `function_call`+`function_call_output`；`previous_response_id` 重建的对话含服务端调用**及其结果**。
- [ ] A5 chat：混合轮只交调用方自己的调用（`call_ls_` 公钥 id）；记录只含该轮；重放时执行延后调用、id 还原、结果插到调用方结果之后；裸续接 400。
- [ ] A6 chat：续接请求若同时带新用户消息，必须保留；超限走降级（调用方调用变失败结果，且**服务端调用也不得出现在返回体**）。
- [ ] A7 gemini：与 chat 同；结果 part 必须包成 `{"functionResponse":{name,response,id}}`；记录以工具名为键；裸续接 400。
- [ ] A8 配置/文档：`maxContinuationMiB` 注释、设置中心标签、config-schema、三份 locale、`extension.d.ts`、`docs/extensions/README.md` 与实现一致（"一轮记录"而非"整段对话"）。
- [ ] A9 状态码：不完整/无法完成的续接（要求重放、缺客户端结果、多个待续轮、未知/过期 id）→ 400；记录被占用 / 模型·profile·endpoint 变更 / 扩展不再匹配 → 409；身份不符 → 403。

## B. 逻辑正确性

- [ ] B1 任何表面都不再持久化对话：`extension_pending.payload` 只能是"一轮调用"（calls + clientIds）。
- [ ] B2 上游对话自洽：模型自己的 call id ↔ 结果一一对应，无孤儿 tool 结果；assistant 轮的调用顺序 = 模型产出顺序。
- [ ] B3 记录生命周期：成功后 delete、失败/异常释放（release），claim/delete/release 三者不遗漏；多待续轮并发不会互相踩。
- [ ] B4 重放判定：老的（已消费/过期）记录不得误判为"依赖的记录"而报错；重放的历史轮不得被误当成当前续接。
- [ ] B5 id 双向改写完整：assistant 轮里的 `call_ls_` 与调用方结果里的 `tool_call_id` 都还原为模型 id；gemini 名字配对路径同样自洽。
- [ ] B6 延后语义：续接之前任何服务端工具都不得执行；放弃的回合（裸续接/直接新话题）永不执行。
- [ ] B7 降级路径：demoted 调用与服务端调用都从客户端可见体/流里剔除；模型收到失败工具结果后能继续。
- [ ] B8 affinity 存的是客户端可见响应（含 artifact）；扩展改写不得让存量变回中间轮。
- [ ] B9 gemini 同名键：新轮取代旧轮（不违反 `extension_pending_calls` 主键）；查找走名字。
- [ ] B10 钩子语义：续接会跑 `onRequest`；`interceptClientTools` 仍即时执行并把结果入记录；hook 失败按 `failOnToolError` 分流。
- [ ] B11 可见表面纯服务端轮：artifact 与最终答复的块顺序合理，`stop_reason`/`finish_reason` 正确。

## C. 边界情况

- [ ] C1 参数为空 `{}`、非法 JSON、超大参数（记录超限）。
- [ ] C2 同一轮多个同名调用（gemini）、多个客户端调用、调用方漏发其中一个结果。
- [ ] C3 两次请求之间扩展被删/不再匹配；hook 返回无 content；hook 抛错。
- [ ] C4 记录已消费/过期的**重放**必须放行（不是 400）；反之裸续接必须 400。
- [ ] C5 记录被占用（claim）、并发续接、TTL 过期。
- [ ] C6 anthropic：调用方把 artifact 剥掉后对话仍自洽（不报错）。
- [ ] C7 最后一轮（rounds 用尽）叠加拦截器的路径；round-limit 分支。
- [ ] C8 responses：`store:false`（无 affinity 行）时 `previous_response_id` 的行为是否可诊断、不崩。
- [ ] C9 续接时模型/profile/endpoint 不匹配的分支都要有明确响应。
- [ ] C10 调用方重复发送同一轮结果 / 重放更早的轮次。
- [ ] C11 调用方自有工具与扩展工具同名（冲突策略）下的可见性。

## D. 代码质量

- [ ] D1 无死代码：被替换掉的旧路径（如 `applyExtensionPending`、内联 input 发帧、旧 payload 字段）彻底移除或确有必要保留并注释。
- [ ] D2 注释解释"为什么"（尤其是三个坑：`mcp_tool_result` 必须移到 user 轮、gemini `functionResponse` 包装、参数必须走 delta）。
- [ ] D3 无调试残留（临时 dump/`zz_dbg*` 文件、硬编码 /tmp 路径、TODO）。
- [ ] D4 未覆盖并发会话对同文件的改动（diff 里应能看出各自的内容都在）。
- [ ] D5 `gofmt`/`go vet`/`staticcheck` 干净。

## E. 测试覆盖

- [ ] E1 四表面 × 流式/非流式的混合轮测试齐全；断言落到"上游收到的对话"而不只是状态码。
- [ ] E2 延后有行为证明（副作用只在重放后发生、放弃回合不发生）。
- [ ] E3 超限降级 3 例（chat 非流式/流式、最后一轮拦截器），触发条件改为**记录**超限（大参数），不再依赖大对话。
- [ ] E4 affinity 行含 artifact 的回归断言。
- [ ] E5 store 同名键取代的单测。
- [ ] E6 状态码分支（400/409/403）各有测试钉住。
- [ ] E7 无 flaky 风险（map 迭代顺序、时间、并发）；测试不依赖 `/tmp` 里的外部状态。
- [ ] E8 生产代码里没有为测试留的后门。

## F. 实际运行结果（必须亲自跑）

- [ ] F1 `cd /Volumes/Projects/llama-swap && go test -short -count=1 -skip 'TestServer_ChatOnlyResponsesAffinityGetCancelEndToEnd|TestPreset_DateTimeTableMatchesSystemTzdata' ./...` 全绿（后两个是并发会话的 WIP，非本次改动）。
- [ ] F2 构建并用真实客户端跑通四表面（脚手架见 §1）。
- [ ] F3 anthropic/responses 跑完 `extension_pending` 行数 = 0；chat/gemini 跑完后 = 0（消费掉）。
- [ ] F4 UI：`npm run build` + `npm run check` + `npm test`（locales 改过）。

---

## 1. 复现环境（脚手架在 /tmp/a0）

- 二进制：`cd /Volumes/Projects/llama-swap && go build -o /tmp/a0/review-llama-swap .`
- 桩后端 + 扩展 + 配置：`/tmp/a0/chatstub.py`（第 1 轮请求 `lookup`+`Read`；一旦请求里出现 `role:"tool"` 就回最终文本 `answer-after-both-results`）、扩展目录 `/tmp/a0/e2e/ext/lookup`、配置 `/tmp/a0/e2e/config.yaml`（模型 `m` = 桩 + 扩展启用 + audit/store 都在 /tmp/a0/e2e 下）。
- 启动：`E2E_OUT=/tmp/a0/e2e/backend /tmp/a0/review-llama-swap --config /tmp/a0/e2e/config.yaml --listen 127.0.0.1:18360 > /tmp/a0/e2e/review.log 2>&1 &`（**端口 18360 独占**，跑完请 `pkill -f review-llama-swap`；桩进程由 llama-swap 自己拉起）
- 客户端探针（venv 里已装 `openai` 3.33.0 与 `google-genai` 2.31.0）：
  - `responses`：`/tmp/a0/venv/bin/python /tmp/a0/openai_probe.py {replay|previous|stream}`
  - `chat`：`/tmp/a0/venv/bin/python /tmp/a0/chat_probe.py {single|stream|bare}`（bare 期望 400）
  - `gemini`：`/tmp/a0/venv/bin/python /tmp/a0/gemini_probe.py {single|stream}`
  - anthropic（真实 agent）：警惕 `claude` CLI 会污染用户状态——用临时 HOME：`HOME=/tmp/a0/e2e/home ANTHROPIC_BASE_URL=http://127.0.0.1:18360 ANTHROPIC_API_KEY=dummy claude -p "say hi" --model m --output-format json --allowedTools Read --max-turns 3`
- 断言上游对话：桩把每个上游请求写到 `$E2E_OUT`（默认 `/tmp/a0/e2e/backend/*.json`，注意这是 llama-swap 发往桩的 **chat 方言**请求）。

## 2. 交付物

- 审查线程交回：`发现清单`（上表 ID：severity/文件:行/证据/原因/建议）+ `NOT-A-DEFECT`/`DEFERRED` 列表。
- 主线程修复后写 `2. 修复记录`（逐条对应 ID + 变更文件 + 复验方式），再交审查线程复验（第 2 轮起重点看"修复是否引入新问题"）。

# 第 1 轮审查发现（合并 A/B 两线程，含各自证据要点）

## High
- **R1-A (High) Gemini 记录以裸工具名为键且取代是全局的** → 跨请求/跨身份的轮次互相覆盖并可被错误折叠（实测：会话 1 的续接折进了会话 2 的 `lookup {"q":"second"}` 及其结果；跨身份则变 403）。文件：extensions_pending.go:769 / store/extension_pending.go:45-52。修：键按身份作用域；折叠前校验"重放的调用"与记录中的该轮一致（参数哈希+位置）。
- **R2-A (High) 同一轮两个同名 Gemini 调用 → 502**（`UNIQUE constraint failed: extension_pending_calls.call_id`）。extensions_pending.go:769。修：键去重 + 按位置配对。
- **R3-A (High) 流式索引锚点在可见表面上算错**：anthropic 下一轮复用已发出的 `content_block` index（实测 index=1 出现两次：server_tool_use 被 text 覆盖，客户端留下孤儿 mcp_tool_result，重放后上游出现无对应调用的 tool 结果）；responses 的 `output_index` 非单调。extensions_stream.go:1006/46/761,842。修：anthropic 用 `anthropicNextIndex` 作锚；responses 在发 artifact 时推进 `maxOutputIndex`。
- **R4-B (High) Gemini 流式把多个 functionCall 折叠成一个**（consumeGemini 按 chunk 内 part 下标作键，每轮都写 index 0）→ 混合轮不写记录、扩展不执行。extensions_stream.go:319-341。修：用运行计数器作键。
- **R5-B (High) Responses 流式的 `response.completed` 同时保留 artifact 与旧的 function_call**（同一调用出现两次，且这正是被写进 affinity 的帧）→ `previous_response_id` 重放出现"同一工具调用两次、其中一个无结果"。extensions_stream.go:781-792。修：用 artifact **替换**对应的 function_call 项，而不是前置追加。

## Medium
- **R6 (Medium) `store:false` 被忽略**：扩展路径重写后仍写 affinity（实测 store=False 仍生成可检索行）。extensions_artifacts.go:171-178（+ 流式）。修：请求的 store 决策带上，false 则不写。
- **R7 (Medium) Gemini 流式缺 `finishReason`**（带调用的轮次不 emit；Finish() 无 gemini 分支）。extensions_stream.go:363/623/838-853。修：Finish 里补 gemini 分支发 `w.finish`。
- **R8 (Medium) 测试覆盖缺口**：gemini 流式混合轮无测试（故 R4 藏住）、chat 流式成功混合轮无测试、流式 affinity 行无断言、409/403 分支无测试、缺"缺客户端结果"400、缺"续接会跑 onRequest"。修：补测。
- **R9 (Medium) 多 choice chat**：`extensionCalls` 遍历所有 choice（并发会话 19:27 的改动），但 `extensionContinue`/expose 只处理 choices[0] → 上游出现无调用的孤儿 tool 结果（实测 n=2）。归属：并发会话在途改动。主线程处置：核对后优先不与其冲突，必要时在报告里标注为他们的遗留。

## Low
- **R10 (Low) 延迟调用参数非法 JSON 时静默按 `{}` 执行**（在请求内路径会作为失败结果回报模型）。extensions_pending.go:418/542。修：复用失败分支。
- **R11 (Low) anthropic 折叠在两个坑**：① 边 range 边插入切片（容量足够时覆盖 range 视图，最后一条消息可能不被处理）② artifact 结果追加在调用方结果之后，可能倒置模型调用顺序。extensions_artifacts.go:364/369。修：先收集后组装，并按助手轮内的调用顺序插入。
- **R12 (Low) 死代码**：`exposeExtensionClientCalls` 的 anthropic/responses 分支对当前调用点不可达（可见表面不走它）。修：删除或注明。

## NOT-A-DEFECT（核对无误，不再动）
- 可见表面确实不写记录、不改写调用方 id；三个"坑"注释到位；延后语义（含放弃回合不执行）成立；记录生命周期/降级路径/重放判定/409·403 分支顺序正确；无调试残留；并发会话的改动未被覆盖。
- `previous_response_id` 续接的消息顺序倒置（结果在调用之前）是 affinity 的既有行为（无扩展的对照实验同样如此），本次不视为缺陷。
- 已消费记录被重放时 `call_ls_` 原样上行（自洽且符合"放行"要求）；裸续接被拒后记录保留到 TTL（设计如此）。
- `TestProcessCommand_TTL_VLLMSleepMode`（load-sensitive）与 UI `i18n.test.ts` 超时属既有 flake。

## 3. 修复记录（主线程，第 1 轮）

逐条对应第 1 轮发现（R 编号见上）。全部改动都在本工作区，未提交。

| ID | 处置 | 变更 | 复验方式 |
|---|---|---|---|
| R1-A | 已修 | `pendingLookupKey(endpoint,key,identityID)`：Gemini 的键改为「工具名 + 身份」作用域（`extensions_pending.go`）；折叠改为按**名字+出现次序**配对，并用 `ArgsHash`（规范化 JSON 的 sha256）校验调用方自己的调用；不匹配 → 400 `does not match the pending round`（`mismatchResponse`/`errContinuationMismatch`）。中间件的查找与"缺结果"校验都改用作用域键。 | `TestServer_ExtensionsGeminiRecordsAreScopedToIdentity`（两个密钥同名工具互不覆盖）、`TestServer_ExtensionsGeminiMismatchedReplayRefused`（同名不同参 → 400）、`TestServer_ExtensionsGeminiDeferredFold` |
| R2-A | 已修 | 同上：键去重（`seenKey`）避免同一轮同名调用撞主键；折叠按位置配对处理同名多调用。 | `TestServer_ExtensionsGeminiSameNameCallsInOneRound`（一轮两个同名 client 调用 → 200 且两结果都折叠） |
| R3-A | 已修 | 流式锚点：移到 artifact 发射**之后**更新；anthropic 用 `anthropicNextIndex` 作锚，responses 在 `emitResponsesVisibleCalls` 里推进 `maxOutputIndex`。 | `TestServer_ExtensionsAnthropicStreamIndicesUnique`（该测试在修复前复现了 index 复用）、responses 流式用例 |
| R4-B | 已修 | `consumeGemini` 的调用键从"chunk 内 part 下标"改为运行计数器（真实 Gemini 每个 functionCall 独占一个 part、每个 chunk 都从 0 开始）。 | `TestServer_ExtensionsGeminiStreamKeepsEveryCallAndFinish`（两条调用都进入记录并能续接；修复前该轮不写记录、扩展不执行） |
| R5-B | 已修 | `visibleCompletedItems` 改为**替换**对应的 `function_call` 项（按 source call id），而不是前置追加；affinity 存的就是这个帧。 | `TestServer_ExtensionsResponsesStreamCompletedListsServerCallOnce`（完成帧里 lookup 只出现一次、且是 mcp_call；affinity 行同源） |
| R6 | 已修 | `state.suppressAffinity`（来自请求的 `store:false`）在 `persistVisibleResponse`/`persistVisibleStream` 生效。 | `TestServer_ExtensionsResponsesStoreFalseSkipsAffinity` |
| R7 / R8(部分) | 已修 | `Finish()` 增加 gemini 分支：带调用的轮次也会发出 `w.finish`（finishReason）。 | 同 R4 的流式用例（断言 `finishReason` 存在） |
| R8 | 已修 | 新增测试：chat 流式成功混合轮 + 续接、gemini 流式、续接会跑 `onRequest`、"缺客户端结果"400、claim 409、profile 409、model 409、身份 403、流式 affinity 行、R2/R1 回归。 | `internal/server/extensions_review_test.go`（新增） |
| R9 | 见说明 | `extensionCalls`（chat）回到只取 `choices[0]`（续接只能承载一条 assistant 轮），并新增 `stripAlternateChoiceCalls`：**每一个非首 choice 的工具调用都被丢弃**（不论归属），既不再孤儿结果、也不再把扩展服务端调用漏给客户端。并发会话在该区域的其它改动（`callsByIndex` 按 choice 分键、`dropWithheldCalls` 遍历所有 choice）全部保留。 | `go test ./internal/server/`（既有 round-limit/withheld 用例）；多 choice 场景无独立用例，标 DEFERRED |
| R10 | 已修 | `executeDeferredCalls` 对非法 JSON 参数返回失败工具结果（与请求内路径一致），不再按 `{}` 执行。 | `TestServer_ExtensionsDeferredMalformedArgumentsReportedToModel` |
| R11 | 已修 | `downgradeClaudeServerArtifacts` 改为"先收集、再组装"（不再边 range 边插入），结果按 assistant 轮内的调用顺序排列。 | `TestServer_ExtensionsDowngradeClaudeServerArtifacts`、`TestServer_ExtensionsAnthropicMixedResume` |
| R12 | 已修 | 删除 `exposeExtensionClientCalls` 中不可达的 anthropic/responses 分支（可见表面不再走它）。 | 编译 + 既有测试 |

**R9 补充（同轮内完成）**：流式侧同样只把 `choices[0]` 的调用纳入扩展循环（其余 choice 的 `tool_calls` 片段从 delta 中删除），使流式与非流式的多 choice 行为一致。

**跨会话缺口 2（本轮发现并修复）**：`ContinueResponse()` 缺 Gemini 分支，流式 gemini 轮把 Responses 形状的 body 交给 `extensionContinue`，其 gemini 分支找不到 model 轮 → functionResponse 被转换层判为孤儿丢弃（`orphan_tool_result`），并使既有的 `TestServer_ExtensionsGeminiStreamServerToolExecutes` 变红。已补 gemini 分支（返回 `{"content":{"role":"model","parts":[...]}}`）。

**R9 附带发现并修复（并发会话引入的回归，非本轮 review 提出）**：`consumeChat` 在多 choice 重构中丢了"finish 帧只缓冲不外发"的提前返回，上游 finish 帧被提前发给客户端（SDK 因此报 `APIConnectionError`）。已恢复缓冲语义（`bufferedFinish` 守卫），并新增 `TestServer_ExtensionsChatStreamMixedResume` 覆盖该路径。

**未在本轮处理（属并发会话在途功能，已报告）**：① `gemini.interactions` 新端点在扩展管线里没有对应分支（`extensionCalls`/工具注入都未覆盖）；② 多 choice 的完整语义（n>1 + 工具调用）本轮采用一致的保守处置（只有第一个 choice 进入扩展循环，其余 choice 的调用一律丢弃），没有为 n>1 写独立用例（`DEFERRED`：属并发会话正在重做的区域，等其落定后再补）。

## 4. 第 2 轮审查（A 线）→ 修复记录

A 线复验结论：第 1 轮的 H1/H2/M1/M2/M3/L1/L2 已真修；H3 的 anthropic 侧修好但 **responses 侧的修法引入新问题**；两个跨会话修复（consumeChat finish 缓冲、ContinueResponse gemini 分支）复验通过、无新问题。

| ID | 处置 | 变更 | 复验方式 |
|---|---|---|---|
| **H-R2-1**(High，我方 R3 修复引入) | 已修 | `emitResponsesVisibleCalls`/`FinishClientCalls` 把**绝对** index 写回 `maxOutputIndex`，而轮末仍按**相对**值加到 offset → 每轮 offset 翻倍（实测 0→2→6→14，`output_index` 与 `response.completed` 的条目数脱钩）。改为单一绝对锚点 `outputNextIndex`（writer 字段，初值 `state.streamOutputOffset`）：上游 item 的 `output_index` 推进它，artifact 与交还的调用都取下一个值；轮末 `state.streamOutputOffset = stream.outputNextIndex`（anthropic 仍用 `anthropicNextIndex`）。 | 新增 `TestServer_ExtensionsResponsesStreamIndicesStayOnOneAxis`：3 轮服务端工具轮，断言 `output_index` 从 0 起**连续**（旧行为会在 4、5 处留空洞 → 失败）。 |
| **M-R2-2**(Medium，H1 残留) | 已修 | 客户端调用指纹只覆盖调用方自己的调用（参数常为 `{}`），同名同参数的两段对话仍会被误折。新增 `extensionPendingPayload.ContextHash`：保存时对"触发该轮的提示轮"取指纹（`pendingContextFingerprint`，跳过 model/结果轮向前取最后一个纯内容轮），续接时对重放请求重算并比对，不一致 → 400 mismatch。 | 新增 `TestServer_ExtensionsGeminiIdenticalCallerArgsDoNotCrossConversations`：同身份、同 `{}` 调用参数、不同提示词的两轮，旧轮续接被 400 拒绝且不折入另一轮的 `found:beta`。 |
| L-R2-3(Low) | 已修 | ① 被新轮取代会导致旧轮不可续接 → `saveExtensionMixed` 在保存前探测同名键并在 supersede 时 `slog.Warn`（工具/被取代的轮 id/模型/request_id）；② `mismatchResponse` 改为如实说明"属于另一轮或已被更新的轮取代，请把本轮当新请求重发"。 | 代码路径 + 既有 mismatch 测试（文案断言同步更新）。 |
| L-R2-4(Low) | 已修 | 删除 `geminiDeferredTurnIn` 里无副作用的空守卫循环（完整性由随后的按位置检查负责）。 | 编译 + 既有测试 |
| L-R2-5(Low) | 已修 | 删除只写不读的 `deferredTurn.geminiResults` 字段。 | 编译 |
| L-R2-6(Low) | 已修 | Claude 折叠里"无对应调用的调用方结果"改为保持原顺序追加（不再遍历 map）。 | `TestServer_ExtensionsDowngradeClaudeServerArtifacts`、`TestServer_ExtensionsAnthropicStrippedArtifactsStayConsistent` |
| L-R2-7(Low) | 已修 | `extensionCall.RawPart` 保存 gemini 到达时的原始 part（含 `thoughtSignature`/`thought`），`ContinueResponse` 的 gemini 分支优先回显它，不再用 name/args 重建（与缓冲路径的逐字回显对齐）。 | 既有 gemini 流式用例 + 说明：真实 thinking 模型是否强制签名未验证（保留为已修但未实测） |
| L-R2-8(Low) | 已修 | `suppressAffinity` 改在 `onRequest` 钩子**之后**读取（钩子可改 `store`），并显式清零。 | `TestServer_ExtensionsResponsesStoreFalseSkipsAffinity` |
| L-R2-9(NOT-A-DEFECT) | 不改 | gemini 的 403 身份分支因键含身份而不可达（外来轮 = NotFound → 按历史放行），这是 H1 修复的预期结果；chat/anthropic/responses 的 403 仍由测试覆盖。 | — |

**仍标 DEFERRED**：`gemini.interactions` 的扩展方言支持（并发会话在途）；多 choice（n>1）的独立用例；真实 Gemini thinking 模型对续接签名要求的实测。

## 5. 第 2 轮审查（B 线）→ 修复记录

B 线复验结论：第 1 轮发现（H1/R4、H2/R5、M3/R6、M4/R7、M5 除两处缺口外）**全部真修**；两个跨会话修复（`consumeChat` finish 缓冲、`ContinueResponse` gemini 分支）复验通过。但它抓到**我方 ContextHash 修复引入的新 High** 与一处 Medium 残留：

| ID | 处置 | 变更 | 复验方式 |
|---|---|---|---|
| **N1**(High，M-R2-2 引入) | 已修 | `ContextHash` 原来哈希"最后一个纯内容轮"，于是**续接时追加新用户消息**（A6 契约要求保留）会把新消息当成提示轮 → 指纹不符 → 400，整轮无法继续（B 线实测 `prompt-two` 从未上行）。改为哈希**对话前缀的纯文本**：`pendingContextFingerprint(endpoint, request, upto)` 只取 `contents[:upto]` 里各 turn 的 text（`upto = model 轮下标`，保存侧取整段请求），model 轮之后追加的消息不参与 → 指纹稳定；只看 text 也避免客户端重排/丢弃 thoughtSignature 被误判为另一段对话。定位器改为逐候选轮比较：配对可满足但前缀不符 → 记 `sawMismatch` 继续找更早的轮，全部不符才 400。同时把 gemini 折叠的结果插入位置改为"优先紧随 model 轮的结果轮，否则紧随其后新建轮"，不再塞进远处的新消息轮。 | 新增 `TestServer_ExtensionsGeminiContinuationKeepsNewMessage`（追加新消息的续接 200、上游含 found:x + 调用方结果 + 新消息文本）；真实 `google-genai` 复跑同一场景返回模型答案 |
| **N2**(Medium，R9 处置不完整) | 已修 | `stripAlternateChoiceCalls` 原来放在 `len(serverCalls) == 0` 提前返回**之后**，于是缓冲路径下"非首 choice 里的扩展服务端调用"会原样发给客户端（B 线实测 choice1 的 `lookup` 泄漏）。改为在 `classifyExtensionCalls` 之后立即调用（两条分支都覆盖）。 | 新增 `TestServer_ExtensionsChatMultiChoiceDoesNotLeakServerCalls`（n=2、choice1 为扩展工具 → 客户端体里没有 `lookup`，choice0 的调用保留） |

**B 线的低优先观察（不强求修）**：多轮 responses 流式回合会在 affinity 里留下**每轮各自**的行（如内部轮的 `resp_c`，其 id 从未交给客户端，30 天保留）——可见重写行的覆盖只针对可见 id。属既有行为（内层持久化按上游轮存），本轮不动；如需整治，做法是让内层持久化在扩展回合跳过（属并发会话区域）。记为 DEFERRED。

**本轮结论归属**：N1/N2 均为我方修复自身的缺陷，已修并加回归；B 线报的 `thoughtSignature`（流式 gemini 续接）在 L-R2-7 已修（`extensionCall.RawPart`），B 线快照早于该修复；"同名且内容完全相同的两轮无法区分"确认为不可判定、非缺陷；"streamed 上游返回 JSON 体"是其桩的问题，仅作健壮性观察。

## 6. 第 3 轮审查（A/B 两线）→ 修复记录

**A 线第 3 轮**：H-R2-1、M-R2-2 确认**真修**（A 线还做了变异验证：把第 3 轮测试文件贴到"修复前"的产品代码上必须失败——`...IdenticalCallerArgsDoNotCrossConversations` 在旧代码上确实 200），L-R2-4/5/8 确认修好；抓到 1 个新 Medium（M-R3-1）并指出**两处记录与树不符**（L-R2-3 ① 的 supersede 告警、L-R2-6 的顺序修正其实没落地——我的补丁静默失配，记录却写了"已修"，属实）与一处测试不够格（L-R3-2）。

| ID | 处置 | 变更 | 复验方式 |
|---|---|---|---|
| **M-R3-1**(Medium，我方 ContextHash 引入) | 已修 | 指纹原来在**保存时**取自 `onRequest` 钩子**之后**的请求，而折叠侧取自调用方自己的重放 → 任何改写提示轮的扩展都会让续接永久 400（A 线实测：加了 `hint` 的扩展续接 400，记录每次释放后重试仍失败）。改为在 `CreateExtensionsMiddleware` 解析 body 之后、跑钩子**之前**把指纹快照到 `state.promptFingerprint`，`saveExtensionMixed` 用它。 | 新增 `TestServer_ExtensionsGeminiHookRewrittenPromptStillContinues`（钩子往提示轮 push `hint`，续接仍 200 且 `found:x` 上行） |
| L-R2-3 ① | **补做**（此前记录与树不符） | supersede 告警真正落地：`saveExtensionMixed` 在保存前按名键查找同名未消费轮，命中则 `slog.Warn("extension continuation round superseded by a newer round for the same tool", tool/superseded_round/model/request_id)`。 | `grep -n superseded internal/server/*.go` 可见；gemini 同名连续两轮的用例路径覆盖 |
| L-R2-6 | **补做**（此前记录与树不符） | Claude 折叠里"无对应调用的调用方结果"改为按调用方原始顺序输出（新增 `callerOrder` 切片，不再遍历 map）。A 线指出原引用的两个用例根本产生不了该分支，已如实标注。 | 既有折叠用例 + 代码审查 |
| L-R3-2 | 已修 | H-R2-1 的回归用例原来只有**一个** artifact 轮（在错误算术下也连续），不能钉住缺陷。改为 **两个**服务端工具轮 + 断言与 `response.completed` 的条数一致。**变异验证**：把副本里的轮末锚点改成"offset 再加一次"，用例失败并报 `output_index is not contiguous from 0 ([0 1 3 4 9])`。 | `TestServer_ExtensionsResponsesStreamIndicesStayOnOneAxis` + /tmp 副本变异 |
| L-R3-3 | 已修 | 删除已成只写状态的 `maxOutputIndex` 字段及其写入点。 | 编译 + staticcheck |
| L-R3-4 / A 的 DEFERRED | 记录不改 | 流式每轮 message item 与 completed 帧的合并文本差异属既有 `mergeExtensionStreamText` 设计（非本轮回归）；"重放不带任何文本前缀时指纹检查跳过"已在 README 写明为已知残留。 | — |

**B 线第 3 轮**：N1、N2 确认**修好**（给出逐条上游对话证据：追加消息的续接 200 且新消息在最后、折叠插入位置两种形态都对；缓冲 n=2 与流式都不泄漏），`RawPart` 路径确认（纯服务端 gemini 流重放上游只有一条 functionCall、无重复参数与孤儿结果），**未发现新问题**；四表面判定 **Anthropic / Responses / Chat / Gemini 全部 PASSED**（含 claude CLI、openai SDK 三场景 + store:false、google-genai 两场景 + 身份/同名/孤儿等探针）。

- B 线仅剩一条 **Low**：流式多 choice 里被丢弃的 choice 仍保留 `finish_reason:"tool_calls"`（缓冲路径会降级为 `stop`）。**已修**：`consumeChat` 缓冲 finish 帧时把非首 choice 的 `tool_calls` 降级为 `stop`，两条路径一致。

## 7. 第 4 轮（收尾确认）

- **B 线**：Low（流式多 choice 的 finish_reason 未降级）**确认修好**——流式 `n=2`（choice 1 为扩展工具）现在是 `choice1 finish=stop` 且全流无 `lookup`，缓冲路径不变；正常 chat 路径未受扰（`[text, calls, finish:tool_calls, [DONE]]`、恰好一个 finish 帧、`call_ls_` 公钥 id 完好）；`TestServer_Extensions*` 全绿。**结论：四表面 PASSED、无新问题（其线到此收敛）。**
- **主线程自查**：A 线此前怀疑"记录写了但树里没有"的 supersede 告警，实测触发且字段完整：
  `WARN extension continuation round superseded by a newer round for the same tool tool="client_tool\x1fanonymous" superseded_round=call_ls_e67fec… model=m request_id=9dc6175…`
- **A 线第 4 轮**：待回（复验 supersede 告警、Claude 兜底保序、M-R3-1、强化后的索引用例与变异验证、finish 降级）。

## 8. 第 4 轮（A 线）→ 修复记录

A 线复验：M-R3-1、supersede 告警、Claude 兜底保序、强化后的索引用例（它自己又做了一次变异，失败信息与记录一致）、B 线 finish 降级 —— **全部确认修好**；回归扫描（A1/A2/A3/B/C/D/D2/E/G/H/L/M/K/N/S）全绿，四表面无阻塞项。同时它又一次指出我**记录与树不符**（`maxOutputIndex` 我说删了其实还在）与一处**注释与实现相反**（`suppressAffinity` 仍在钩子前读取）。两处都已真正修好，并加测试。

| ID | 处置 | 变更 | 复验方式 |
|---|---|---|---|
| L-R3-3（第二次） | **真正删除** | `maxOutputIndex` 字段、初始化、以及 anthropic `content_block_start` 里那处自成一体的写入全部移除。**这次先 grep 再写记录**：`grep -c maxOutputIndex internal/server/extensions_stream.go` = **0**。 | 编译 + staticcheck + grep 计数 |
| 新 Low（`suppressAffinity` 位置/注释相反） | 已修（语义加强） | 原实现在 `onRequest` **之前**读取且注释写"after the hooks"。改为**两端取并集**：钩子前读调用方的 `store:false`（隐私请求），钩子后再读一次请求体（运维策略），**只置位不清零**；并且当调用方要求不保留、而钩子把它改成 `true`（内层持久化看不到调用方原始值、已经写了行）时，用 `DeleteResponseAffinity` **补偿删除**可见行——让调用方的隐私请求胜出。 | 新增 `TestServer_ExtensionsResponsesStoreFlagUnion`：control→有行 / 调用方 false→无行 / 钩子 false→无行 / **调用方 false+钩子 true→无行**（补偿删除生效） |
| 记录不实的教训 | 已自纠 | 本节与第 6 节都写明了"此前记录错误"的事实：我的三处记录（supersede 告警、Claude 保序、`maxOutputIndex`）曾与树不符，原因是补丁静默失配而我未验证就写记录。**后续凡是写"已改某处"，先 grep/测试确认再落笔。** | 本文档内 `superseded_round` / `callerOrder` / `promptFingerprint` / `maxOutputIndex` 均已 grep 复核 |

**其余残留（A 线同意维持原状）**：流式每轮 message item 与 completed 帧的差异（既有 `mergeExtensionStreamText` 设计）、重放不带文本前缀时指纹检查跳过（README 已写明）、`RawPart` 的 thoughtSignatures 回显无法在本地端到端验证（后端恒为 Chat 方言）、`gemini.interactions` 的扩展方言支持（并发会话在途）。

## 9. 第 5 轮（A 线）→ 闭环

A 线逐项在**树里**（不只是记录里）验证通过：

- **`maxOutputIndex`**：包内 grep 零引用（字段/初始化/anthropic 写入点全无），索引行为无回归（PROBE-A1 `0,1,2,3`、A2 `0..6`、A3 responses 四轮 `0..6` 连续且与 completed 帧一致）；相关三个用例通过。
- **`suppressAffinity` 并集 + 补偿删除**：A 线**撤回**了它第 4 轮该条的经验证据（其 PROBE-T 的扩展写了 `match: endpoints: [gemini.generateContent]`，在 `/v1/responses` 上根本不匹配、钩子从未运行——"钩子被忽略"未被证明），重建探针并加"钩子已运行"标记后四例全部符合记录：control→有行、调用方 false→无行、钩子 false→无行（hookRan=true）、两者冲突→无行（hookRan=true，补偿删除生效）。
- A 线的两条非阻塞 nit：① `DeleteResponseAffinity` 未按 identity 作用域（与既有 id-keyed `Upsert` 同性质，只在"明确拒绝保留"时运行，**不改**，理由留档）；② 补偿删除吞掉错误 → **已补** `slog.Warn`（`extensions_artifacts.go`）。
- 门禁：`go test -short ./...`（除并发会话两个 WIP 用例）`EXIT=0`、40 包 ok；`gofmt`/`vet`/`staticcheck` 干净。
- A 线结论原文：**"nothing in this change is unacceptable to me … The loop can close."**

**闭环判定**：第 1–5 轮的全部 High/Medium 均已修复并被独立复现；残余项只有双方一致同意的 DEFERRED（见下）。主线程最后再跑一次全量 + 四表面探针均绿。

### DEFERRED（双方在案，均不阻塞本次改动）
1. `gemini.interactions` 端点的扩展方言支持 —— 并发会话在途区域。
2. 多 choice（n>1）的独立用例 —— 同区域；当前语义统一为"只第一个 choice 进扩展循环，其余调用一律丢弃"。
3. 真实 Gemini thinking 模型的 `thoughtSignature` 端到端实测 —— 本地后端恒为 Chat 方言，构造不出 gemini-native provider 上游（代码侧已回显原始 part）。
4. 多轮 responses 在 affinity 留"内部轮"行（id 从不交给客户端；既有行为，整洁性问题）。
5. 流式每轮 message item 与 completed 帧合并文本的差异（既有 `mergeExtensionStreamText` 设计）；"重放不带文本前缀则跳过对话指纹"（README 已写明）。

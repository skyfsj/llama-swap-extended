# 协议运行时验收记录

日期：2026-10-19（Asia/Shanghai）。

## 验收范围

覆盖 OpenAI Chat、OpenAI Responses、Claude Messages、Gemini generateContent、Gemini Interactions 的请求、响应和 SSE 转换；网关 MCP Tools、Resources、Resource Templates、Prompts、审批、输入续接、传输、取消和通知；会话 checkpoint、原生状态隔离、文件保存和重上传、后台生成、兼容性规划。

不把模拟上游、官方 SDK 接入与真实厂商行为混为一项验证。测试通过只证明下述场景，不代表厂商所有功能或未来新增字段均可无损互换。

## 已修复的问题

| 范围 | 修复结果 |
| --- | --- |
| 工具身份与结果 | 保留显式调用 ID、命名空间、并行调用及空/富内容结果；避免按名称串配结果 |
| 流式转换 | 按索引累积并行调用；修复调用结束、拒绝、故障、JSON 回退及已执行网关工具的生命周期 |
| Responses 历史 | 持久化请求历史，隔离身份和模型；截断历史明确拒绝跨格式续接；遵守 `store:false` |
| 原生状态 | 按 provider、adapter、model、credential、版本保存；分支、切换和 compaction 使旧状态失效 |
| 审批策略 | 客户端只能收紧配置策略；校验端点、工具白名单、只读声明、tool choice 和并行限制 |
| 混合工具调用 | 在执行之前保存审批轮次；客户端结果补齐后恢复；已有执行结果不会重跑 |
| MCP 输入续接 | 保存待输入项、原始 requestState 和模型轮次；校验输入类型与表单 schema；原子认领续接 |
| MCP 会话 | 身份/配置/凭据隔离；复用有状态连接；审批期间保留会话；取消、失连、配置更新和关闭清理 |
| MCP 清单 | 限制数量、字节和字段长度；拒绝空条目、重复身份和非法 schema；注册 panic 转为错误 |
| MCP 跨版本 | 清理逐跳协议元数据；将旧版 roots、sampling、elicitation 回调转换为新版显式输入轮次 |
| MCP 通知 | 更新工具/资源/提示清单；转发资源订阅、更新、进度和旧版日志；共享资源订阅计数 |
| 会话竞态 | 修复租约续期 goroutine 捕获可变 context 的竞争 |
| 文件 | 保存原始字节与厂商引用；按身份和凭据校验、上传、检查及过期重传；隔离远端上传 origin |
| 后台 checkpoint | 持久记录任务和最终结果，版本冲突检查、取消、重启中断；查询不会重复提交会话 |
| 原生后台 checkpoint | `execution:"native"` 提交厂商任务、钉扎来源轮询、取消转发、暂态失败续盯、重启再挂接 |
| MCP Tasks 与 completion | 任务句柄持久路由，查询/补充输入/取消幂等，重启恢复；prompts/resources completion 跨版本转发 |

## 验证结果

| 验证 | 结果 |
| --- | --- |
| 五协议会话矩阵：5×5×流式/非流式 | 通过 |
| 五协议 MCP 执行矩阵：5×5×流式/非流式 | 通过 |
| MCP 输入续接：5 客户端×2 输出模式×2 审批策略，含两轮输入、schema 错误和重复恢复 | 20 个场景通过 |
| 后台 checkpoint：5×5 上游/客户端 | 25 个场景通过 |
| 后台取消、轮询幂等、重启中断、存储归属和持久恢复 | 通过 |
| 原生后台 checkpoint：提交→完成→下一轮继续同一厂商任务 | 通过 |
| 原生后台暂态 5xx 续盯、取消确认、非原生表面拒绝、重启再挂接 | 通过 |
| stdio、Streamable HTTP、旧 SSE、取消和会话生命周期 | 通过 |
| MCP 2025-11-25 / 2026-07-28 通知、资源订阅与清理 | 通过 |
| 旧 SSE 回调转接两版本客户端，含 Tools、Resources、Prompts | 通过 |
| 网关 live 路由、直接执行及审批后执行 | 通过 |
| 官方 SDK 五协议组合、JSON 回退和原生基线 | 80 个场景通过 |
| `make test-dev`：全量短测试与 staticcheck | 最后增量通过；server 83.058 秒 |
| 服务端相关 race 回归 | 通过，323.806 秒 |
| 最新 MCP live、bridge、stdio race 回归 | 通过 |
| MCP proxy 构建 | 最后增量通过，产物位于 `build/mcp-proxy` |

官方 SDK 使用本地实际安装版本：openai 2.30.0、anthropic 1.15.0、google-genai 2.31.0。MCP 使用官方 Go SDK v1.8.0。原始审查的 460 项检查属于修复前后阶段的历史证据，其中包含 9 项旧能力缺失探测，不将这些探测算作当前新增能力的验收。

## OpenCode Go 真实联调

使用用户提供的 Go 套餐凭据，仅经进程环境传入测试。凭据未写入仓库、配置、报告或测试源码。

官方 Go 入口：[OpenCode Go](https://opencode.ai/v2/docs/console/go/)。

| 真实上游 | 模型 | 入口 |
| --- | --- | --- |
| Chat | glm-5.3-flash | `/inference/go/openai/v1/chat/completions` |
| Responses | gpt-6-luna | `/inference/go/openai/v1/responses` |
| Messages | claude-haiku-5-5 | `/inference/go/anthropic/v1/messages` |
| Gemini | gemini-4-argon-contributor | `/inference/go/google/v1beta/models/{model}:generateContent` |

- 四类真实上游 × 五种客户端协议 × 流式/非流式：40 个场景通过，实际 assistant 文本符合断言。
- 真实模型自动选择并调用本地 MCP 服务，再接收工具结果继续生成：五种客户端 × 流式/非流式，10 个场景通过；每个场景恰好执行一次工具。
- 最终 40 场景联调使用 1024 输出 token 预算。首轮 256 预算曾出现一次空文本，未保存该次原始上游响应，原因未确认。对原预算的相关 Gemini/Interactions 客户端组合重复三轮均未复现；不将其写成已修复缺陷，也不据此推断为转换丢失。
- 误用独立付费入口时返回过 HTTP 402。该结果不代表 Go 套餐不可用；切换 Go 专用入口后联调通过。
- 以上是经 OpenCode Go 的真实模型联调，不等同于直接连接四家原厂服务。

## 能力边界与未验证项

| 项目 | 当前行为 |
| --- | --- |
| 原生 opaque reasoning / thought signature 跨厂商重放 | 无等价语义时诊断或拒绝，不互换签名/密文；同 scope 保留 |
| MCP 快照与 live | 默认 `/mcp` 保持快照；通知、订阅、旧回调通过 `/mcp?model={model}&live=true` |
| 旧回调暂停 | 内存中的原始操作最多保留两分钟、十轮输入；重启后不可自动恢复或重跑 |
| 原生厂商后台任务与 checkpoint | `execution:"native"` 以 `background:true` 提交 Responses/Interactions 任务，按保存的 provider/adapter/model/凭据钉扎轮询与取消；完成即提交 checkpoint，下一轮复用原生 opaque 状态。不宣称等价于原厂生命周期：网关重启或 30 分钟到期标 `interrupted`，厂商侧任务可能仍在运行 |
| 原生 Interactions 上游真实联调 | Go 官方入口未列出 Interactions 原生服务；已验证 Interactions 客户端转换，其原生上游使用模拟服务与官方 SDK 验证 |
| 原厂 Files API、引用失效与重上传 | 本地模拟厂商端点通过；未直接连接原厂 Files 服务验证 |
| 厂商缓存命中、计费和后台生命周期 | 未取得真实厂商命中/计费/异步任务证据 |
| 任意第三方 MCP 扩展 | 支持 `io.modelcontextprotocol/tasks` 任务句柄（查询、补充输入、取消、重启恢复）与 prompts/resources completion 转发；其余扩展不宣称支持 |
| 外部操作在进程崩溃时的最终结果 | 可能未知；保留中断/失败状态，不自动重试有副作用的操作 |

运行时接口、限制和调用格式见 [protocol-runtime.md](protocol-runtime.md)。

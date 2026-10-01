export const quickStart = `export const settings = {
  temperature: {
    type: "number", label: "温度", default: 0.7,
    min: 0, max: 2, section: "生成参数"
  }
};

export default {
  onRequest(ctx, request) {
    ctx.log.info("请求接口：" + ctx.endpoint);
    request.temperature = ctx.config.temperature;
    return request;
  }
};`;

export const extensionDocSections = [
  { id: "start", title: "快速上手", description: "创建、配置并验证第一个拓展", content: `
1. 在拓展管理器中新建拓展，填写唯一标识和名称。
2. 将下面的代码粘贴到入口文件 **index.js**。它会记录请求接口，并为请求设置温度。
3. 保存拓展，在配置面板中调整「温度」，选择适用模型并启用。
4. 在「请求调试」中选择模型并运行，检查匹配结果、请求变化和日志。
5. 通过实际推理请求验证完整流程；调试不会启动模型。

\`\`\`javascript
${quickStart}
\`\`\`

真实请求的接口由调用方决定，拓展通过 **ctx.endpoint** 读取。调试中的接口选择用于模拟不同协议；请求体字段应与所选协议一致。
` },
  { id: "lifecycle", title: "生命周期", description: "7 个钩子的执行时机与返回值", content: `
入口文件默认导出一个对象，按需声明以下钩子。所有钩子都支持同步函数或 async 函数。返回 null 或 undefined 时保留输入；修改对象后建议显式返回。

| 钩子 | 时机与输入 | 返回值与约束 |
| --- | --- | --- |
| onRequest(ctx, request) | 匹配成功后处理请求体 | 修改后的请求对象；可调整生成参数与消息 |
| onBeforeForward(ctx, request) | 请求转发给后端前 | 修改后的请求对象；不能更改工具定义 |
| onToolCall(ctx, call) | 执行服务端工具；也可拦截客户端工具 | { content: ... }；拦截客户端调用时需 { handled: true, content: ... } |
| onToolResult(ctx, result) | 服务端工具执行后，结果写入对话前 | 修改后的工具结果对象 |
| onResponse(ctx, response) | 非流式响应返回前 | 修改后的响应对象 |
| onStreamEvent(ctx, event) | 流式响应事件处理时 | 修改后的事件对象；结构随协议变化 |
| onError(ctx, error) | 钩子失败或请求处理错误时 | 错误对象含 hook、error；返回值不用于替换响应 |

请求钩子按 priority 从小到大执行，同优先级按拓展 ID 排序；响应钩子逆序执行。每次钩子调用使用独立 worker，不应依赖模块全局变量跨钩子或跨请求保存状态。

continueOnError 决定拓展出错时是否继续处理请求。onError 用于记录错误，不能代替错误恢复策略。
` },
  { id: "context", title: "上下文 ctx", description: "请求、路由、配置和运行环境", content: `
| 字段 | 含义 |
| --- | --- |
| requestId | 当前请求标识 |
| requestedModel | 调用方请求的模型 |
| resolvedModel | 路由解析后的实际模型 ID |
| profile | 当前 Profile |
| provider | 后端提供方 |
| endpoint | chat.completions、responses 或 anthropic.messages |
| stream | 是否为流式请求 |
| extension | 当前拓展 ID |
| config | 配置对象，包含已声明字段的默认值；未声明的键也会传入 |
| abortSignal.aborted | 当前实现初始值为 false；不是浏览器 AbortSignal，不支持事件监听 |
| log / console | 日志方法，见下节 |
| http / files | 受权限控制的网络与文件操作 |

不同接口的请求体不统一：Chat Completions 通常使用 messages，Responses 使用 input，Anthropic Messages 使用 messages 和独立的 system 字段。先检查 ctx.endpoint，再操作协议专属字段。
` },
  { id: "apis", title: "日志、网络与文件 API", description: "方法签名、返回值和权限", content: `
### 日志

ctx.log.debug(message)、info(message)、warn(message)、error(message) 写入拓展日志。ctx.console 还支持 log 方法。每次调用最多收集 100 条日志；避免记录密钥或完整敏感请求。

### 网络请求

\`\`\`javascript
const response = await ctx.http.fetch("https://example.com/api", {
  method: "POST",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify({ query: "hello" })
});
if (!response.ok) throw new Error("HTTP " + response.status);
const data = response.json();
\`\`\`

fetch(url, options?) 返回 Promise，响应提供 status、ok、text() 和 json()。text() 与 json() 直接返回解析结果。它不是浏览器的完整 Fetch API，不提供流式 body、Response.headers 或自定义 AbortSignal。

先在网络权限中添加目标主机。networkHosts 支持完整主机名和 *.example.com 子域通配符，不支持裸 *。私有、回环、保留地址以及服务自身主机禁止访问，每次重定向也会检查。

### 文件读写

\`\`\`javascript
const text = await ctx.files.read("/app/data/documents/input.txt");
await ctx.files.write("/app/data/output/result.txt", text);
\`\`\`

read(path) 返回 Promise<string>；write(path, content) 返回 Promise<boolean>。路径必须位于 readRoots / writeRoots 允许的目录中，单次文件内容上限 10 MiB。未配置权限时拒绝访问。

测试模式禁用所有网络与文件操作，即使已配置权限。脚本无法使用 Node fs、process、shell 或环境变量。
` },
  { id: "tools", title: "工具声明与执行", description: "客户端工具、服务端工具及调用拦截", content: `
\`\`\`javascript
export default {
  tools: [{
    type: "function",
    function: {
      name: "echo",
      description: "返回输入文本",
      parameters: {
        type: "object",
        properties: { text: { type: "string" } },
        required: ["text"]
      }
    },
    execution: "server"
  }],
  onToolCall(ctx, call) {
    if (call.name === "echo") {
      return { content: String(call.arguments.text ?? "") };
    }
  }
};
\`\`\`

function 包含 name、可选 description、parameters（JSON Schema）和 strict。execution 为 client 时将调用交给 API 客户端；为 server 时由 onToolCall 执行。call 包含 id、name 和已解析的 arguments。

服务端工具结果写回对话并继续生成，受全局 maxToolRounds 限制。不同接口的工具定义与结果由系统转换；Anthropic 使用 input_schema、tool_use 和 tool_result。

开启 interceptClientTools 后，可以在 onToolCall 中接收客户端工具调用，返回 { handled: true, content: ... } 接管；返回 undefined 则保留客户端处理。toolConflict 可选 skip、override、error；skip 保留客户端同名工具及其所有权。

工具没有单独的 execute API，执行入口是 onToolCall。
` },
  { id: "settings", title: "配置表单", description: "声明字段，避免手写配置 JSON", content: `
使用命名导出 **export const settings = { ... }** 声明字段。配置值保存在 manifest.config，脚本通过 ctx.config 读取。

| type | 表单用途 |
| --- | --- |
| string / password | 单行文本 / 密码 |
| number | 数值，可设置 min、max |
| boolean | 开关 |
| select | 单选，options 为字符串数组 |
| textarea | 多行文本 |
| list | 列表 |
| map | 键值对 |
| json | 确实需要任意嵌套数据时使用 |

通用属性：label（名称）、hint（提示）、section（分区）、default（默认值）、required（键必须存在）、secret（遮罩显示）。未显式配置的字段使用 default；required 不保证字符串非空，业务校验需在脚本中完成。

secret 仅影响界面遮罩，值仍随配置写入 manifest.yaml。不要将敏感默认值写进代码。
` },
  { id: "runtime", title: "匹配与运行限制", description: "适用范围、执行顺序与资源预算", content: `
| 配置 | 含义 |
| --- | --- |
| enabled | 是否参与请求处理 |
| priority | 请求处理优先级，数值越小越先执行 |
| match.models / excludeModels | 针对解析后的模型 ID；支持 *、?，排除规则优先 |
| match.profiles / providers / endpoints | 按 Profile、提供方、接口匹配；空列表不限制 |
| timeout | 单次钩子的墙钟时间限制，如 10s |
| maxCpuMillis | CPU 时间预算，单位毫秒 |
| maxMemoryMiB | 内存预算，单位 MiB |
| continueOnError | 发生拓展错误后是否继续请求 |
| toolConflict | 同名工具冲突策略 |
| interceptClientTools | 是否允许拦截客户端工具调用 |

CPU 和内存通过采样检查，短时间内可能超出设置值。拓展脚本应由受信任的管理员维护。
` },
  { id: "files", title: "文件与依赖", description: "入口、多文件模块和 npm 包", content: `
index.js 是代码入口，相对导入如 import { helper } from "./lib/helper.js" 可用于拆分模块。manifest.yaml 通过配置面板维护。

支持 .js、.mjs、.cjs、.json、.md、.txt、.yaml、.yml。每个拓展最多 200 个文件，单文件 256 KiB，总计 4 MiB。隐藏文件和 node_modules 不通过管理 API 写入。

使用 npm 依赖时需同时提供 package.json 与 package-lock.json；保存验证阶段安装依赖，服务器需具备 npm。仅支持纯 JavaScript 包，不支持安装脚本和原生模块。

保存提交完整文件集合，被移除的文件会从拓展目录删除。新版本无效时，最近一次有效版本保持运行。
` },
  { id: "testing", title: "调试与性能测试", description: "检查范围、模拟请求和统计含义", content: `
代码检查验证语法和相对导入路径，定位文件、行与列；它不是完整类型检查。npm 依赖在保存时解析。

请求调试使用**已保存版本**，先检查启用状态和匹配规则，再执行 onRequest 并计算工具注入结果。结果包含匹配状态、处理前后请求、注入工具、日志和耗时。返回的 hooks 是已声明钩子列表，不代表本次执行了所有钩子。

性能测试重复执行同一模拟请求，统计拓展处理耗时、成功率和重复执行结果。它不运行模型推理，不衡量模型吞吐或 token 延迟；网络与文件操作禁用。未匹配的请求不代表一次有效拓展执行。

onBeforeForward、onToolCall、onToolResult、onResponse、onStreamEvent 的端到端行为需要通过真实请求验证。

多轮调试对话把整个对话发到 /api/extensions/{id}/debug/chat，在真实推理管线中执行：仅应用当前扩展（无论启用状态和匹配规则），其他扩展不参与；onRequest、工具注入、服务端工具循环、流式桥接全部按生产逻辑运行，因此工具调用、网络与文件权限都是真实生效的。每轮结束后可展开「扩展执行」查看各钩子耗时、请求变化与日志，以及 kv 读写、模型转发等宿主任务调用。模型请求客户端工具时，调试对话会提示而不执行。
` },
  { id: "storage", title: "存储与会话", description: "ctx.kv、scope 隔离与上下文信息", content: `
ctx.kv 以 JSON 形式存取键值对：get(key)、set(key, value)、delete(key)、keys(prefix)，均可带 { scope } 与 set 的 { ttlSeconds }。权限由 manifest 的 permissions.storage 决定：缺省为临时存储（内存、滑动 TTL，重启丢失），persistent 启用跨重启的持久化，none 完全禁用。scope 为 global（默认，仅本扩展可见）、session（按调用方会话 X-Session-ID 隔离）、key（按 API key 隔离）；配额默认每个扩展 2000 个键、每值 256 MiB。

ctx.session 提供调用方快照：会话 id、key id、是否匿名；ctx.locale 是控制台语言（如 zh-CN）；ctx.models 列出当前调用方有权使用的模型。ctx.usage() 返回该调用方近 24 小时的请求数与 token 用量，可用于在 onRequest 中自行实现配额。
` },
  { id: "forwarding", title: "模型转发", description: "ctx.forward 调用其余模型", content: `
ctx.forward(model, request) 以调用方身份执行一次非流式模型调用，返回 { status, body }。调用方 key 的模型白名单照常生效，请求不会经过任何扩展钩子（不会递归），嵌套转发受 extensions.maxForwardDepth 限制（默认 2）。

注意：并发为 1 且被占用的模型可能自锁到钩子超时；转发会产生独立的调用记录。设置里的 label 与 hint 还可以是 { en, "zh-CN" } 形式的对象，设置中心按当前界面语言显示。
` },
  { id: "management", title: "管理 API", description: "管理、校验、测试和预设接口", content: `
管理接口使用 config-admin 权限；无密钥部署沿用服务的开放管理策略。

| 方法与路径 | 用途 |
| --- | --- |
| GET /api/extensions | 列表 |
| POST /api/extensions | 新建 |
| GET /api/extensions/{id} | 获取定义与 ETag |
| PUT /api/extensions/{id} | 保存完整定义，需要 If-Match |
| DELETE /api/extensions/{id} | 删除，需要 If-Match |
| POST /api/extensions/{id}/duplicate | 复制 |
| POST /api/extensions/reload | 重新扫描目录 |
| POST /api/extensions/check | 提交 { files } 或 { path, source }，返回 diagnostics；不保存 |
| POST /api/extensions/{id}/test | 提交 { context, request }，模拟已保存版本 |
| GET /api/extensions/presets | 列出预设 |
| GET /api/extensions/presets/{id} | 获取预设及文件 |
| POST /api/extensions/presets/{id}/install | 安装预设，可通过 id 指定新标识 |

定义的 files 是完整源文件映射。配置字段验证失败返回 422，diagnostics.path 指向问题字段。编辑时保留最新 ETag，避免覆盖其他管理员的修改。
` },
];

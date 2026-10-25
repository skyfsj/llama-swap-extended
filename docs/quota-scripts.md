# 自定义额度脚本

`providers.<id>.quota.script` 用一段 JavaScript 代替声明式映射（`endpoint` +
`windows` + `extras`），适合两类情况：

- 平台的余额接口不是"一个地址 + 固定字段"：需要先查设置再换算、需要拼接多个
  请求、或者要自己做条件判断。
- 你已经有一份 **CC Switch** 的自定义用量脚本，想直接搬过来。

脚本只在刷新额度时执行（手动刷新或 `refreshInterval`），不会进入推理请求路径。

## 两种写法

### 1. CC Switch 兼容：`{ request, extractor }`

```yaml
quota:
  script: |
    ({
      request: {
        url: "{{baseUrl}}/v1/usage",
        method: "GET",
        headers: { "Authorization": "Bearer {{apiKey}}" }
      },
      extractor: function (response) {
        const remaining = response?.remaining ?? response?.quota?.remaining ?? response?.balance;
        const unit = response?.unit ?? "USD";
        return { isValid: true, remaining, unit };
      }
    })
```

- `request.url` 必填；`method` 默认 `GET`（支持 GET/POST/PUT/PATCH/DELETE）；
  `headers` 和 `body` 可选。
- 占位符 `{{baseUrl}}` 与 `{{apiKey}}` 只在 `request` 的 `url`/`headers`/`body`
  里替换。`{{baseUrl}}` 是供应商配置的端点（如 `https://relay.example.com`）。
- HTTP 请求由服务端发出，因此凭据代理、超时和响应大小限制与声明式映射一致。
- `extractor` 收到解析后的 JSON，必须是同步函数。

### 2. 函数形态：`async (ctx) => snapshot`

```yaml
quota:
  script: |
    (async (ctx) => {
      const token = await ctx.fetch(ctx.baseUrl + "/api/usage/token/", {
        headers: { Authorization: "Bearer " + ctx.apiKey }
      });
      const status = await ctx.fetch(ctx.baseUrl + "/api/status");
      const unit = status.json?.data?.quota_per_unit || 500000;
      return {
        windows: [{ id: "quota", label: "额度", used: token.json.data.total_used, limit: token.json.data.total_granted }],
        extras: [{ id: "available", label: "剩余额度", value: token.json.data.total_available / unit, suffix: " USD" }]
      };
    })
```

`ctx` 提供：

| 成员 | 说明 |
|---|---|
| `ctx.baseUrl` | 供应商端点（第一个适配器的 endpoint） |
| `ctx.apiKey` | 当前凭据的值 |
| `ctx.providerID` | 供应商 id |
| `ctx.fetch(url, options?)` | 同步返回 `{status, ok, url, json, text}`；`options` 支持 `method`/`headers`/`body` |

`fetch` 返回的不是 Promise，`await` 一个普通值同样合法，所以两种写法可以混用。
函数形态可以连续调用多次 `fetch`。

> 脚本必须是**单个表达式**（对象字面量或函数）。多语句请包在
> `(() => { ... })()` 里。

## 返回值

### 原生形状（推荐）

```js
{
  windows: [
    { id: "rolling", label: "5 小时", percent: 40 },          // 已用百分比
    // 或 { id: "quota", used: 6, limit: 24 }                  // 绝对用量
    // 或 { id: "balance", remaining: 75 }                     // 剩余百分比
  ],
  extras: [
    { id: "available", label: "剩余额度", value: 12.5, suffix: " USD" }
  ]
}
```

- `windows[].percent` 是**已用**百分比；`remaining` 是剩余百分比；`used` +
  `limit` 会算出已用比例。三者给出其一即可。
- 可选的 `status` / `resetsAt`（RFC3339）会渲染在进度条下方。
- `extras[].value` 可以是数字、字符串或布尔值；`suffix` 追加在值后面。
- `quota.divideBy` 会作用到 `windows[].used/limit` 与数值型 extras 上。

### CC Switch 扁平字段

`windows`/`extras` 为空时，接受 CC Switch 的归一化结果：

| 字段 | 效果 |
|---|---|
| `total` + `used` | 生成一条金额窗口（`used/limit`） |
| `remaining` | 一行"剩余额度"，单位取 `unit` |
| `unit` | 追加到金额行（如 `USD`） |
| `planName` | 一行"套餐" |
| `extra` | 一行备注 |
| `isValid: false` | 卡片显示为错误，文案取 `invalidMessage` |

## 错误与超时

- 脚本抛错、返回 Promise 悬挂、返回不可序列化对象、extractor 结果形状不对，
  都会写成卡片上的错误文本；上次成功的快照会保留。
- 执行时间上限是 `quota.timeoutSeconds`（默认 15s），超时通过中断 VM 结束；
  单个 `fetch` 也受同一预算约束。
- 错误文本会抹掉凭据明文，但**不要**把 apiKey 拼进 URL 之外的字符串里。

## 安全

脚本与 `models.<id>.cmd` 是同一信任级别：写配置的人本来就能执行命令。
因此脚本在服务进程内运行（不是扩展的独立 worker），没有 Node 内置模块，
只有上面列出的 `ctx` 能力。不要在脚本里硬编码密钥——使用 `{{apiKey}}` 或
`ctx.apiKey`，否则它会以明文留在配置文件里。

## 哪些平台没有公开余额接口

截至 2026-10，智谱（BigModel）、MiniMax、小米 MiMo **没有**公开的、用 API Key
就能查询的余额接口（官方文档与主流聚合网关都没有实现）。这些平台请：

- 用它们的中转站（new-api / sub2api）暴露的 `/v1/usage`、`/api/usage/token/`；
- 或者让管理员提供一个可查询余额的接口，再用自定义脚本接入。

已内置且经过核对的余额预设：DeepSeek、Moonshot (Kimi)、SiliconFlow、
OpenRouter、OpenCode Go、New API（含自动读取额度单位的脚本版）、Sub2API。

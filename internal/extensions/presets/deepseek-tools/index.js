// DeepSeek 官方工具 API：联网搜索 + 网页抓取。
//
// 合同依据官方文档：
//   搜索 https://api-docs.deepseek.com/zh-cn/api/search
//   抓取 https://api-docs.deepseek.com/zh-cn/api/fetch
//   指南（鉴权/错误/计费）https://api-docs.deepseek.com/zh-cn/guides/search
//
// 端点：
//   POST https://api.deepseek.com/v1/tools/search   （联网搜索）
//   POST https://api.deepseek.com/v1/tools/fetch    （网页抓取 → Markdown）
//   鉴权：Authorization: Bearer <DeepSeek API Key>
//
// 账户余额不走这里：它是运维视角的信息，由供应商卡片的"额度"面板展示
// （config/quotapresets/deepseek.yaml，GET /user/balance），不给模型暴露工具。
//
// 计费与限流（指南原文要点）：
//   - 搜索与抓取按「成功请求数」计费，每千次 5 元；失败、无结果、超时不计费。
//     count 只影响返回条数，不影响单价。
//   - 搜索与抓取各自有账户级 QPS 上限 5（与模型并发限制无关）；抓取另有按目标域名的
//     QPS 限制，超限一律返回 429。
//   - 看到 408/429/5xx 应等待后重试；402 需充值后再试；其它 4xx 不要重试。
//
// 搜索语法（原样透传给检索服务，可组合）：
//   site:github.com / -site:example.com / -广告 / -"release notes" /
//   "精确短语" / lang:zh,en / after:2025 / before:2025-01-01
//
// 工具名一律带 deepseek 前缀，与其它搜索器同时启用时不会撞名。

import {
  clampCount,
  collapseText,
  createToolRegistry,
  defineExtension,
  fetchTool,
  searchTool
} from "@llama-swap/extension";

const SEARCH_ENDPOINT = "https://api.deepseek.com/v1/tools/search";
const FETCH_ENDPOINT = "https://api.deepseek.com/v1/tools/fetch";

// 计费说明直接写进工具描述，模型据此有意识地控制调用次数（每次 0.005 元）。
const COST_NOTE = "每次成功调用计费 0.005 元，账户级 QPS 上限 5。";

export const settings = {
  apiKey: {
    type: "string",
    label: "API Key",
    hint: "DeepSeek API Key（platform.deepseek.com/api_keys），以 Bearer 方式发送。搜索与抓取共用；未填写时调用会直接报错，而不是发一次注定失败的请求。",
    secret: true,
    required: true
  },
  searchCount: {
    type: "number",
    label: "搜索结果条数",
    hint: "搜索默认返回条数（count，1-50）。条数不影响计费，只影响上下文体积。",
    default: 10,
    min: 1,
    max: 50,
    section: "搜索"
  },
  needContent: {
    type: "boolean",
    label: "搜索时返回整页正文",
    hint: "开启后搜索结果会带上抓取到的正文（need_content=true），上下文会明显变长；默认只用摘要。",
    default: false,
    section: "搜索"
  },
  localization: {
    type: "string",
    label: "结果本地化 IP",
    hint: "可选：用于地域化的 IP 地址（localization），留空则由服务端决定。",
    section: "搜索"
  },
  maxFetchChars: {
    type: "number",
    label: "抓取正文上限（字符）",
    hint: "抓取返回的 Markdown 正文截断长度，避免一页塞爆上下文。",
    default: 32000,
    min: 2000,
    max: 200000,
    section: "抓取"
  },
  requestTimeoutMs: {
    type: "number",
    label: "HTTP 超时（毫秒）",
    hint: "单次请求超时；抓取整页较慢，可适当放宽。扩展自身的 timeout 仍然兜底。",
    default: 20000,
    min: 1000,
    max: 60000
  }
};

/** 搜索与抓取共用的状态码建议（DeepSeek 指南里的重试策略）。 */
const TOOL_STATUS_ADVICE = {
  400: "请求格式或参数不合法，修正参数后重试。",
  401: "API Key 无效或缺失，检查扩展设置。",
  402: "账户余额不足，充值后重试。",
  403: "抓取请求被安全策略拒绝，不要重试。",
  404: "抓取未能获得可用正文（URL 不可达、被站点策略拦截或内容不可提取），换一个地址。",
  408: "服务端读取请求超时，等待后重试。",
  413: "请求体超过大小限制。",
  429: "触发 QPS 限流（账户上限 5），等待后重试。",
  500: "服务端内部错误，稍后重试。",
  504: "下游超时，稍后重试。"
};

/** 注册表把两个工具声明摆在一处，重名会在扩展加载时立即失败。 */
const registry = createToolRegistry();

// ---------------------------------------------------------------------------
// 1) 联网搜索：deepseek_search
// ---------------------------------------------------------------------------

registry.add(
  searchTool({
    name: "deepseek_search",
    title: "DeepSeek",
    description:
      "用 DeepSeek 官方联网搜索检索实时网页，返回带权威度与相关度评分的编号结果（标题、URL、摘要、来源、发布日期）。" +
      "支持检索语法并可与查询词组合：site:github.com 限定站点、-site:example.com 排除站点、\"精确短语\"、" +
      "lang:zh,en 限定语言、after:2025 与 before:2026-06-01 限定时间。适合需要最新事实、新闻或出处的问题。" +
      "同一账户 QPS 上限 5，请勿在短时间内连续调用。" +
      COST_NOTE,
    // 合同规定查询必须为 1～128 个字符且不能全为空白。
    maxQueryLength: 128,
    credential: { setting: "apiKey", label: "API Key", bearer: true },
    parameters: {
      type: "object",
      properties: {
        query: {
          type: "string",
          description:
            "检索式，1-128 个字符。可用 site:、-site:、-词、\"短语\"、lang:、after:、before: 等运算符组合，例如：DeepSeek 模型 site:github.com lang:zh after:2025"
        },
        count: {
          type: "number",
          description: "本次返回条数（1-50）。条数不影响计费，按需取用。"
        },
        ori_question: {
          type: "string",
          description: "可选：用户最初的问题原文（1-128 字符），帮助检索服务理解意图。"
        }
      },
      required: ["query"]
    },
    request({ query, config, arguments: args }) {
      const body = {
        query,
        count: clampCount(args.count, clampCount(config.searchCount, 10, 1, 50), 1, 50)
      };
      // 该字段对长度敏感，超限会被服务端判 400，这里先自行约束。
      const original = collapseText(args.ori_question).slice(0, 128);
      if (original !== "") {
        body.ori_question = original;
      }
      const localization = collapseText(config.localization);
      if (localization !== "") {
        body.localization = localization;
      }
      if (config.needContent === true) {
        body.need_content = true;
      }
      return { url: SEARCH_ENDPOINT, method: "POST", body };
    },
    parse(payload) {
      const items = Array.isArray(payload.results) ? payload.results : [];
      return items.map((item) => ({
        title: item.title,
        url: item.url,
        source: item.site_name,
        // date 是 RFC 3339；只展示到日期，避免噪音。
        date: item.date ? String(item.date).slice(0, 10) : "",
        // 相关度满分 3，权威度 0-4（-1 表示未知）：两者一起给模型判断可信度。
        meta: scoreLine(item),
        // summary（约 600-800 字）比 snippet 更完整，作为正文主来源。
        summary: item.summary || item.snippet,
        content: item.content
      }));
    },
    statusAdvice: TOOL_STATUS_ADVICE
  })
);

/** 把 rerank_score 与 authority 渲染成一行元信息（缺失则省略）。 */
function scoreLine(item) {
  const parts = [];
  if (typeof item.rerank_score === "number") {
    const good = item.rerank_score >= 1.5 ? "，优质" : "";
    parts.push("相关度 " + item.rerank_score.toFixed(2) + good);
  }
  if (typeof item.authority === "number") {
    if (item.authority >= 0) {
      const label = item.authority >= 3 ? "，极权威" : "";
      parts.push("权威度 " + item.authority + label);
    } else {
      parts.push("权威度未知");
    }
  }
  return parts.length > 0 ? parts : undefined;
}

// ---------------------------------------------------------------------------
// 2) 网页抓取：deepseek_fetch
// ---------------------------------------------------------------------------

registry.add(
  fetchTool({
    name: "deepseek_fetch",
    title: "DeepSeek",
    description:
      "用 DeepSeek 官方抓取接口读取一个网页，服务端已渲染并抽取正文，返回页面标题与 Markdown 内容。" +
      "需要拿到干净正文（文档、文章、README）时用它；只是想看某页是否存在或需要离线抓取时，" +
      "可以改用 web_fetch 预设（自行下载解析、不需要密钥）。" +
      COST_NOTE,
    // 合同规定 URL 上限 8192 字符，且必须是带 host 的 http/https 地址。
    maxUrlLength: 8192,
    credential: { setting: "apiKey", label: "API Key", bearer: true },
    request({ url }) {
      return { url: FETCH_ENDPOINT, method: "POST", body: { url } };
    },
    parse(payload) {
      return { title: payload.title, content: payload.content };
    },
    maxChars: 32000,
    statusAdvice: TOOL_STATUS_ADVICE
  })
);

export default defineExtension({ tools: registry.tools() });

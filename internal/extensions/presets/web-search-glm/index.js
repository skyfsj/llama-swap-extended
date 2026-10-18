// 智谱 GLM 网络搜索 API。
//
// 合同依据官方文档 https://docs.bigmodel.cn/api-reference/工具-api/网络搜索：
//
//   POST https://open.bigmodel.cn/api/paas/v4/web_search
//   Authorization: Bearer <api-key>
//   Content-Type: application/json
//   请求体：{search_query, search_engine, search_intent, count?,
//           content_size?, search_recency_filter?, search_domain_filter?}
//     search_engine: search_std | search_pro | search_pro_sogou | search_pro_quark
//     search_recency_filter: oneDay | oneWeek | oneMonth | oneYear | noLimit
//     content_size: medium | high
//
//   响应：{id, created, request_id,
//           search_intent: [{query, intent, keywords}],
//           search_result: [{title, content, link, media, icon, refer,
//                            publish_date}]}
//   错误以 {error: {code, message}} 返回。
//
// 工具名带 glm 后缀：多个搜索器可同时启用；密钥只在设置里填写。

import { clampCount, collapseText, defineExtension, searchTool } from "@llama-swap/extension";

export const settings = {
  apiKey: {
    type: "string",
    label: "API Key",
    hint: "API key from the Zhipu bigmodel platform. Required for the tool to work; fill it in after installing the preset.",
    secret: true,
    required: true
  },
  maxResults: {
    type: "number",
    label: "Max results",
    hint: "How many results to request (count). Zhipu accepts 1-50.",
    default: 10,
    min: 1,
    max: 50
  },
  searchEngine: {
    type: "select",
    label: "Search engine",
    hint: "Underlying search backend. search_pro is the general default.",
    options: ["search_pro", "search_std", "search_pro_sogou", "search_pro_quark"],
    default: "search_pro"
  },
  contentSize: {
    type: "select",
    label: "Content size",
    hint: "medium returns a summary per result; high returns more context.",
    options: ["medium", "high"],
    default: "medium"
  },
  recencyFilter: {
    type: "select",
    label: "Recency filter",
    options: ["noLimit", "oneDay", "oneWeek", "oneMonth", "oneYear"],
    default: "noLimit"
  },
  intentRecognition: {
    type: "boolean",
    label: "Intent recognition",
    hint: "Let the service rewrite the query first (search_intent=true).",
    default: false
  },
  requestTimeoutMs: {
    type: "number",
    label: "HTTP timeout (ms)",
    hint: "Per-request timeout for the search call.",
    default: 20000,
    min: 1000,
    max: 30000
  }
};

export default defineExtension({
  tools: [
    searchTool({
      name: "web_search_glm",
      title: "Zhipu",
      description:
        "Search the web with the Zhipu GLM web search API and return numbered results with title, source, link and content.",
      // 该服务对查询长度较敏感，官方建议控制在 70 字符左右。
      maxQueryLength: 200,
      credential: { setting: "apiKey", label: "API Key", bearer: true },
      request({ query, config }) {
        const body = {
          search_query: query,
          search_engine: String(config.searchEngine || "search_pro"),
          search_intent: config.intentRecognition === true,
          count: clampCount(config.maxResults, 10, 1, 50),
          content_size: String(config.contentSize || "medium"),
          search_recency_filter: String(config.recencyFilter || "noLimit")
        };
        return { url: "https://open.bigmodel.cn/api/paas/v4/web_search", method: "POST", body };
      },
      // 意图改写的结果对模型理解检索方向有帮助，放在结果之前。
      preface(payload) {
        const intents = Array.isArray(payload.search_intent) ? payload.search_intent : [];
        if (intents.length === 0) {
          return undefined;
        }
        const rewritten = collapseText(intents[0] && intents[0].query);
        return rewritten === "" ? undefined : "Rewritten query: " + rewritten;
      },
      parse(payload) {
        // 智谱把业务错误放在 HTTP 200 的 error 字段里。
        if (payload.error) {
          const error = payload.error;
          const message = (error.code ? error.code + " " : "") + (error.message || JSON.stringify(error));
          throw new Error("Zhipu 搜索错误：" + message);
        }
        const items = Array.isArray(payload.search_result) ? payload.search_result : [];
        return items.map((item) => ({
          title: item.title,
          url: item.link,
          source: item.media,
          date: item.publish_date ? String(item.publish_date) : "",
          meta: item.refer !== undefined ? ["refer " + item.refer] : undefined,
          content: item.content
        }));
      }
    })
  ]
});

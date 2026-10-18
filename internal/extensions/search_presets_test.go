package extensions

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// scriptedCall is one ctx.http.fetch the preset made, as the host saw it.
type scriptedCall struct {
	URL     string
	Method  string
	Headers map[string]any
	Body    string
}

func (c scriptedCall) jsonBody(t *testing.T) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(c.Body), &payload); err != nil {
		t.Fatalf("request body is not JSON (%q): %v", c.Body, err)
	}
	return payload
}

// scriptedHost answers the executor's http.fetch from a script. The host call
// runs in this process, so a preset's request and its parsing can both be
// checked without a network - which is the only way to test a search preset,
// since the sandbox offers no way to point one at a test server.
type scriptedHost struct {
	respond func(call scriptedCall) (int, string)
	calls   []scriptedCall
}

func (h *scriptedHost) Exec(_ context.Context, fn string, args json.RawMessage) (any, error) {
	if fn != "http.fetch" {
		return nil, nil
	}
	var call struct {
		URL     string         `json:"url"`
		Options map[string]any `json:"options"`
	}
	if err := json.Unmarshal(args, &call); err != nil {
		return nil, err
	}
	method, _ := call.Options["method"].(string)
	body, _ := call.Options["body"].(string)
	headers, _ := call.Options["headers"].(map[string]any)
	record := scriptedCall{URL: call.URL, Method: method, Headers: headers, Body: body}
	h.calls = append(h.calls, record)
	status, payload := h.respond(record)
	return map[string]any{
		"status":  status,
		"ok":      status >= 200 && status < 300,
		"url":     call.URL,
		"headers": map[string]any{"content-type": "application/json"},
		"body":    payload,
	}, nil
}

// searchPresetWithConfig loads a preset with the credentials a test supplies.
// An installed preset carries them in manifest.config; ctx.config is built from
// that plus the declared defaults.
func searchPresetWithConfig(t *testing.T, id string, config map[string]any) *Compiled {
	t.Helper()
	compiled := presetBundle(t, id)
	if compiled == nil {
		t.Skipf("%s is not in the catalog", id)
	}
	compiled.Manifest.Config = config
	return compiled
}

// runSearchPreset runs the tool through the Node executor, whose host calls come
// back to this process. The goja worker is the production path; both runtimes
// answer ctx.http.fetch identically except for where the request is made, and
// the goja request/response plumbing is covered by the fetch tests.
func runSearchPreset(t *testing.T, compiled *Compiled, arguments map[string]any, host *scriptedHost) (string, error) {
	t.Helper()
	return runPresetTool(t, compiled, "", arguments, host)
}

// runPresetTool 执行预设里的一个工具。toolName 为空时取该预设声明的第一个工具：
// 搜索类预设都是"一个预设一个搜索工具"，而工具名由预设决定（带提供方后缀），
// 测试因此不必重复写死名字。
func runPresetTool(t *testing.T, compiled *Compiled, toolName string, arguments map[string]any, host *scriptedHost) (string, error) {
	t.Helper()
	nodePath, err := lookPathCached()
	if err != nil {
		t.Skip("node is not available")
	}
	script := executorScriptPath()
	if script == "" {
		t.Skip("the executor script is not available")
	}
	if toolName == "" {
		if len(compiled.Tools) == 0 {
			t.Fatalf("%s 没有声明任何工具", compiled.Manifest.ID)
		}
		var function struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(compiled.Tools[0].Function, &function); err != nil {
			t.Fatalf("%s: %v", compiled.Manifest.ID, err)
		}
		toolName = function.Name
	}
	payload, err := json.Marshal(map[string]any{"name": toolName, "arguments": arguments})
	if err != nil {
		t.Fatal(err)
	}
	request := workerRequest{
		Bundle: compiled.Bundle, Manifest: compiled.Manifest, Settings: compiled.Settings,
		Hook: "onToolCall", Context: Context{}, Input: payload,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	response, err := compiled.runNodeHook(WithHostCaller(ctx, host), request, 30*time.Second, nodePath, script)
	if err != nil {
		return "", err
	}
	var result struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(response.Output, &result); err != nil {
		t.Fatalf("output %s: %v", string(response.Output), err)
	}
	return result.Content, nil
}

// exaSearchFixture mirrors the sample response in Exa's search documentation.
const exaSearchFixture = `{
  "requestId": "01a2b3c4",
  "resolvedSearchType": "auto",
  "results": [
    {
      "id": "https://example.com/latency",
      "title": "How inference providers reduce transformer latency",
      "url": "https://example.com/latency",
      "publishedDate": "2025-04-14T00:00:00.000Z",
      "author": "A. Author",
      "score": 0.87,
      "highlights": ["Speculative decoding cuts the decode step.", "Batching keeps the GPU busy."]
    },
    {
      "title": "A second source",
      "url": "https://example.org/second",
      "highlights": ["Another excerpt."]
    }
  ],
  "costDollars": {"total": 0.007}
}`

func TestPresets_ExaSearchRequestAndFormatting(t *testing.T) {
	compiled := searchPresetWithConfig(t, "web-search-exa", map[string]any{"apiKey": "exa-test-key"})
	host := &scriptedHost{respond: func(scriptedCall) (int, string) { return 200, exaSearchFixture }}

	content, err := runSearchPreset(t, compiled, map[string]any{"query": "how do inference providers reduce transformer latency"}, host)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, want := range []string{
		"1. How inference providers reduce transformer latency",
		"https://example.com/latency",
		"2025-04-14",
		"Speculative decoding cuts the decode step.",
		"2. A second source",
		"https://example.org/second",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("result is missing %q:\n%s", want, content)
		}
	}

	if len(host.calls) != 1 {
		t.Fatalf("the preset made %d calls, want 1", len(host.calls))
	}
	call := host.calls[0]
	if call.URL != "https://api.exa.ai/search" || call.Method != "POST" {
		t.Fatalf("request went to %s %s", call.Method, call.URL)
	}
	if call.Headers["x-api-key"] != "exa-test-key" {
		t.Fatalf("x-api-key header = %v", call.Headers["x-api-key"])
	}
	payload := call.jsonBody(t)
	for _, field := range []string{"query", "type", "numResults"} {
		if _, present := payload[field]; !present {
			t.Fatalf("request body is missing %q: %s", field, call.Body)
		}
	}
	contents, _ := payload["contents"].(map[string]any)
	// This also pins runtime parity: the value comes from the preset's declared
	// default, so it is only there because the executor was handed the same
	// finished ctx.config the goja worker builds.
	if contents["highlights"] != true {
		t.Fatalf("contents = %v, want highlights enabled by default", payload["contents"])
	}
}

func TestPresets_ExaSearchTextModeAndDomains(t *testing.T) {
	compiled := searchPresetWithConfig(t, "web-search-exa", map[string]any{
		"apiKey":         "exa-test-key",
		"contentMode":    "text",
		"maxCharacters":  float64(1200),
		"includeDomains": "docs.python.org, github.com,",
		"numResults":     float64(3),
	})
	host := &scriptedHost{respond: func(scriptedCall) (int, string) { return 200, exaSearchFixture }}

	if _, err := runSearchPreset(t, compiled, map[string]any{"query": "asyncio"}, host); err != nil {
		t.Fatalf("search: %v", err)
	}
	payload := host.calls[0].jsonBody(t)
	contents, _ := payload["contents"].(map[string]any)
	text, _ := contents["text"].(map[string]any)
	if text["maxCharacters"] != float64(1200) {
		t.Fatalf("contents.text = %v", contents)
	}
	if _, present := contents["highlights"]; present {
		t.Fatalf("text mode also asked for highlights: %v", contents)
	}
	domains, _ := payload["includeDomains"].([]any)
	if len(domains) != 2 || domains[0] != "docs.python.org" || domains[1] != "github.com" {
		t.Fatalf("includeDomains = %v (the empty entry should be dropped)", payload["includeDomains"])
	}
	if payload["numResults"] != float64(3) {
		t.Fatalf("numResults = %v", payload["numResults"])
	}
}

func TestPresets_ExaSearchReportsHTTPErrors(t *testing.T) {
	compiled := searchPresetWithConfig(t, "web-search-exa", map[string]any{"apiKey": "bad-key"})
	host := &scriptedHost{respond: func(scriptedCall) (int, string) {
		return 401, `{"error":"Invalid API key","tag":"INVALID_API_KEY"}`
	}}

	_, err := runSearchPreset(t, compiled, map[string]any{"query": "anything"}, host)
	if err == nil {
		t.Fatal("a rejected key produced a result")
	}
	for _, want := range []string{"HTTP 401", "Invalid API key", "API Key"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to mention %q", err.Error(), want)
		}
	}
}

// parallelSearchFixture mirrors the sample response in Parallel's search
// documentation, including the warnings array.
const parallelSearchFixture = `{
  "search_id": "search_8a911eb27c7a4afaa20d0d9dc98d07c0",
  "results": [
    {
      "url": "https://www.reuters.com/business/example",
      "title": "Ex-Twitter CEO Agrawal's AI search startup raises $100M",
      "publish_date": "2025-11-12",
      "excerpts": ["Section Title: Information you can trust", "Parallel Web Systems secured a Series A round."]
    }
  ],
  "warnings": ["max_results reduced to 20"]
}`

func TestPresets_ParallelSearchRequestAndFormatting(t *testing.T) {
	compiled := searchPresetWithConfig(t, "web-search-parallel", map[string]any{"apiKey": "parallel-test-key"})
	host := &scriptedHost{respond: func(scriptedCall) (int, string) { return 200, parallelSearchFixture }}

	content, err := runSearchPreset(t, compiled, map[string]any{
		"query":     "Parallel Web Systems products",
		"objective": "decide whether to adopt their search API",
	}, host)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, want := range []string{
		"1. Ex-Twitter CEO Agrawal's AI search startup raises $100M",
		"https://www.reuters.com/business/example",
		"2025-11-12",
		"Parallel Web Systems secured a Series A round.",
		"Note from the API: max_results reduced to 20",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("result is missing %q:\n%s", want, content)
		}
	}

	call := host.calls[0]
	if call.URL != "https://api.parallel.ai/v1/search" || call.Method != "POST" {
		t.Fatalf("request went to %s %s", call.Method, call.URL)
	}
	if call.Headers["x-api-key"] != "parallel-test-key" {
		t.Fatalf("x-api-key header = %v", call.Headers["x-api-key"])
	}
	payload := call.jsonBody(t)
	queries, _ := payload["search_queries"].([]any)
	if len(queries) != 1 || queries[0] != "Parallel Web Systems products" {
		t.Fatalf("search_queries = %v", payload["search_queries"])
	}
	if payload["objective"] != "decide whether to adopt their search API" {
		t.Fatalf("objective = %v", payload["objective"])
	}
	if payload["mode"] != "fast" {
		t.Fatalf("mode = %v, want the fast default", payload["mode"])
	}
	advanced, _ := payload["advanced_settings"].(map[string]any)
	if advanced["max_results"] != float64(5) {
		t.Fatalf("advanced_settings.max_results = %v", advanced["max_results"])
	}
	// The excerpt budget lives inside excerpt_settings on the v1 path; at the
	// top level it would be ignored, which is the migration trap the docs call
	// out.
	excerpts, _ := advanced["excerpt_settings"].(map[string]any)
	if excerpts["max_chars_per_result"] != float64(2000) {
		t.Fatalf("advanced_settings.excerpt_settings = %v", advanced["excerpt_settings"])
	}
}

func TestPresets_ParallelSearchOmitsEmptyObjective(t *testing.T) {
	compiled := searchPresetWithConfig(t, "web-search-parallel", map[string]any{"apiKey": "parallel-test-key"})
	host := &scriptedHost{respond: func(scriptedCall) (int, string) { return 200, parallelSearchFixture }}

	if _, err := runSearchPreset(t, compiled, map[string]any{"query": "solid state batteries"}, host); err != nil {
		t.Fatalf("search: %v", err)
	}
	payload := host.calls[0].jsonBody(t)
	if _, present := payload["objective"]; present {
		t.Fatalf("an empty objective was sent: %v", payload["objective"])
	}
}

func TestPresets_ParallelSearchReportsHTTPErrors(t *testing.T) {
	compiled := searchPresetWithConfig(t, "web-search-parallel", map[string]any{"apiKey": "bad-key"})
	host := &scriptedHost{respond: func(scriptedCall) (int, string) {
		return 429, `{"error":"Rate limit exceeded"}`
	}}

	_, err := runSearchPreset(t, compiled, map[string]any{"query": "anything"}, host)
	if err == nil {
		t.Fatal("a rate limited request produced a result")
	}
	for _, want := range []string{"HTTP 429", "Rate limit exceeded", "限流"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to mention %q", err.Error(), want)
		}
	}
}

// TestPresets_SearchCredentialsAreRequired covers the paths that need no
// network: an unconfigured preset must say so instead of sending a request that
// can only fail.
func TestPresets_SearchCredentialsAreRequired(t *testing.T) {
	for _, entry := range []struct {
		id   string
		tool string
	}{
		{"web-search-exa", "web_search_exa"},
		{"web-search-parallel", "web_search_parallel"},
	} {
		compiled := presetBundle(t, entry.id)
		if compiled == nil {
			t.Skipf("%s is not in the catalog", entry.id)
		}
		payload := presetToolCall(t, compiled, map[string]any{"query": "llama-swap"})
		_, _, err := compiled.Invoke(context.Background(), "onToolCall", Context{}, payload)
		if err == nil {
			t.Fatalf("%s: 未配置密钥却没有报错", entry.id)
		}
		// 错误要点名工具与凭据，用户才知道该去哪个设置项里填。
		for _, want := range []string{entry.tool, "API Key"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: error = %v, want it to mention %q", entry.id, err, want)
			}
		}
	}
}

func TestPresets_SearchRejectsEmptyQuery(t *testing.T) {
	for _, id := range []string{"web-search-exa", "web-search-parallel"} {
		compiled := searchPresetWithConfig(t, id, map[string]any{"apiKey": "configured-but-unused"})
		payload := presetToolCall(t, compiled, map[string]any{"query": "   "})
		_, _, err := compiled.Invoke(context.Background(), "onToolCall", Context{}, payload)
		if err == nil || !strings.Contains(err.Error(), "must not be empty") {
			t.Fatalf("%s: error = %v", id, err)
		}
	}
}

// DeepSeek 搜索：请求体字段、检索语法透传、评分渲染与 need_content 开关。
// 合同见 https://api-docs.deepseek.com/zh-cn/api/search
const deepseekSearchFixture = `{
  "results": [
    {
      "rerank_score": 2.35,
      "title": "DeepSeek 开放平台",
      "url": "https://platform.deepseek.com",
      "snippet": "平台首页摘要",
      "summary": "这里是搜索服务返回的较长摘要，用于替代 snippet 作为正文。",
      "content": "",
      "site_name": "platform.deepseek.com",
      "site_icon": null,
      "date": "2026-10-16T12:47:39.282Z",
      "authority": 4
    },
    {
      "rerank_score": 0.4,
      "title": "一条低分结果",
      "url": "https://example.com/low",
      "snippet": "低分摘要",
      "summary": "",
      "content": "",
      "site_name": null,
      "site_icon": null,
      "date": null,
      "authority": -1
    }
  ]
}`

func TestPresets_DeepSeekSearchRequestAndFormatting(t *testing.T) {
	compiled := searchPresetWithConfig(t, "deepseek-tools", map[string]any{
		"apiKey":      "deepseek-test-key",
		"searchCount": float64(3),
		"needContent": true,
	})
	host := &scriptedHost{respond: func(scriptedCall) (int, string) { return 200, deepseekSearchFixture }}

	content, err := runPresetTool(t, compiled, "deepseek_search", map[string]any{
		"query":        "DeepSeek 模型 site:github.com lang:zh after:2025",
		"ori_question": "有哪些 DeepSeek 的开源实现？",
	}, host)
	if err != nil {
		t.Fatalf("搜索失败：%v", err)
	}
	for _, want := range []string{
		"1. DeepSeek 开放平台",
		"https://platform.deepseek.com",
		"2026-10-16",
		"相关度 2.35，优质",
		"权威度 4，极权威",
		"这里是搜索服务返回的较长摘要",
		"2. 一条低分结果",
		"权威度未知",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("结果缺少 %q：\n%s", want, content)
		}
	}

	if len(host.calls) != 1 {
		t.Fatalf("请求次数 = %d，期望 1", len(host.calls))
	}
	call := host.calls[0]
	if call.URL != "https://api.deepseek.com/v1/tools/search" || call.Method != "POST" {
		t.Fatalf("请求发往 %s %s", call.Method, call.URL)
	}
	if got := call.Headers["Authorization"]; got != "Bearer deepseek-test-key" {
		t.Fatalf("Authorization = %v", got)
	}
	payload := call.jsonBody(t)
	// 检索语法必须原样透传：site:/lang:/after: 由服务端解析。
	if payload["query"] != "DeepSeek 模型 site:github.com lang:zh after:2025" {
		t.Fatalf("query = %v", payload["query"])
	}
	if payload["count"] != float64(3) {
		t.Fatalf("count = %v（应取自设置项 searchCount）", payload["count"])
	}
	if payload["need_content"] != true {
		t.Fatalf("need_content = %v", payload["need_content"])
	}
	if payload["ori_question"] != "有哪些 DeepSeek 的开源实现？" {
		t.Fatalf("ori_question = %v", payload["ori_question"])
	}
}

// 单次调用可以覆盖条数；未开 need_content 时不应带上该字段。
func TestPresets_DeepSeekSearchPerCallCount(t *testing.T) {
	compiled := searchPresetWithConfig(t, "deepseek-tools", map[string]any{"apiKey": "k", "searchCount": float64(10)})
	host := &scriptedHost{respond: func(scriptedCall) (int, string) { return 200, deepseekSearchFixture }}
	if _, err := runPresetTool(t, compiled, "deepseek_search", map[string]any{"query": "x", "count": float64(50)}, host); err != nil {
		t.Fatalf("搜索失败：%v", err)
	}
	payload := host.calls[0].jsonBody(t)
	if payload["count"] != float64(50) {
		t.Fatalf("count = %v（单次参数应覆盖设置项）", payload["count"])
	}
	if _, present := payload["need_content"]; present {
		t.Fatalf("默认不应发送 need_content：%v", payload)
	}
}

// 合同规定 query 为 1-128 字符：超长要在本地拦住，别让它变成一次 400。
func TestPresets_DeepSeekSearchRejectsOverlongQuery(t *testing.T) {
	compiled := searchPresetWithConfig(t, "deepseek-tools", map[string]any{"apiKey": "k"})
	host := &scriptedHost{respond: func(scriptedCall) (int, string) { return 200, deepseekSearchFixture }}
	_, err := runPresetTool(t, compiled, "deepseek_search", map[string]any{"query": strings.Repeat("a", 129)}, host)
	if err == nil || !strings.Contains(err.Error(), "at most 128") {
		t.Fatalf("超长查询错误 = %v", err)
	}
	if len(host.calls) != 0 {
		t.Fatalf("超长查询仍然发出了请求：%+v", host.calls)
	}
}

// 错误信封按 DeepSeek 指南整形：状态码 + 上游 message + 处置建议。
func TestPresets_DeepSeekSearchReportsErrors(t *testing.T) {
	compiled := searchPresetWithConfig(t, "deepseek-tools", map[string]any{"apiKey": "bad"})
	host := &scriptedHost{respond: func(scriptedCall) (int, string) {
		return 402, `{"error":{"message":"Insufficient Balance","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`
	}}
	_, err := runSearchPreset(t, compiled, map[string]any{"query": "anything"}, host)
	if err == nil {
		t.Fatal("余额不足却返回了结果")
	}
	for _, want := range []string{"HTTP 402", "Insufficient Balance", "余额不足"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q，期望包含 %q", err.Error(), want)
		}
	}
}

// DeepSeek 抓取：只发 url，返回标题与 Markdown 正文。
const deepseekFetchFixture = `{"title":"Example article","content":"# Example article\n\nArticle content."}`

func TestPresets_DeepSeekFetchRequestAndFormatting(t *testing.T) {
	compiled := searchPresetWithConfig(t, "deepseek-tools", map[string]any{"apiKey": "deepseek-test-key"})
	host := &scriptedHost{respond: func(scriptedCall) (int, string) { return 200, deepseekFetchFixture }}

	content, err := runPresetTool(t, compiled, "deepseek_fetch", map[string]any{"url": "https://example.com/article"}, host)
	if err != nil {
		t.Fatalf("抓取失败：%v", err)
	}
	for _, want := range []string{"https://example.com/article", "Title: Example article", "markdown", "# Example article"} {
		if !strings.Contains(content, want) {
			t.Fatalf("结果缺少 %q：\n%s", want, content)
		}
	}

	call := host.calls[0]
	if call.URL != "https://api.deepseek.com/v1/tools/fetch" || call.Method != "POST" {
		t.Fatalf("请求发往 %s %s", call.Method, call.URL)
	}
	if got := call.Headers["Authorization"]; got != "Bearer deepseek-test-key" {
		t.Fatalf("Authorization = %v", got)
	}
	if payload := call.jsonBody(t); payload["url"] != "https://example.com/article" {
		t.Fatalf("请求体 = %v", payload)
	}
}

// 抓取只接受 http/https：本地先拦住，省掉一次注定失败的调用。
func TestPresets_DeepSeekFetchRejectsNonHTTPURL(t *testing.T) {
	compiled := searchPresetWithConfig(t, "deepseek-tools", map[string]any{"apiKey": "k"})
	host := &scriptedHost{respond: func(scriptedCall) (int, string) { return 200, deepseekFetchFixture }}
	_, err := runPresetTool(t, compiled, "deepseek_fetch", map[string]any{"url": "file:///etc/passwd"}, host)
	if err == nil || !strings.Contains(err.Error(), "http") {
		t.Fatalf("非 http 地址错误 = %v", err)
	}
	if len(host.calls) != 0 {
		t.Fatalf("非 http 地址仍然发出了请求：%+v", host.calls)
	}
}

// 两个工具共用同一个密钥设置项：任何一个发现密钥为空都要明确报错，
// 而不是发出一次注定 401 的请求。
func TestPresets_DeepSeekToolsRequireCredential(t *testing.T) {
	for _, tool := range []string{"deepseek_search", "deepseek_fetch"} {
		compiled := presetBundle(t, "deepseek-tools")
		if compiled == nil {
			t.Skip("deepseek-tools 不在目录里")
		}
		host := &scriptedHost{respond: func(scriptedCall) (int, string) { return 200, "{}" }}
		arguments := map[string]any{"query": "x", "url": "https://example.com"}
		_, err := runPresetTool(t, compiled, tool, arguments, host)
		if err == nil || !strings.Contains(err.Error(), "API Key") {
			t.Fatalf("%s：缺少密钥时错误 = %v", tool, err)
		}
		if len(host.calls) != 0 {
			t.Fatalf("%s：缺少密钥仍然发出了请求", tool)
		}
	}
}

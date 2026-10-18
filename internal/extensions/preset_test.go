package extensions

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestPresets_MissingCredentialIsReportedClearly(t *testing.T) {
	// An installed preset has an empty credential until the user fills it in.
	// The script must say so instead of sending a request that can only 401.
	compiled := presetBundle(t, "web-search-bing")
	if compiled == nil {
		t.Skip("web-search-bing is not in the catalog")
	}
	call := presetToolCall(t, compiled, map[string]any{"query": "llama-swap"})
	_, _, err := compiled.Invoke(context.Background(), "onToolCall", Context{}, call)
	if err == nil || !strings.Contains(err.Error(), "API Key") {
		t.Fatalf("missing credential error = %v", err)
	}
}

func TestPresets_OpenAIWebSearchInjection(t *testing.T) {
	compiled := presetBundle(t, "openai-web-search")
	if compiled == nil {
		t.Skip("openai-web-search is not in the catalog")
	}
	out, _, err := compiled.Invoke(context.Background(), "onRequest", Context{Endpoint: "chat.completions"}, json.RawMessage(`{"model":"gpt-4o","messages":[]}`))
	if err != nil {
		t.Fatalf("chat.completions injection: %v", err)
	}
	var chat map[string]any
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatal(err)
	}
	tools, _ := chat["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("chat tools = %+v", chat["tools"])
	}
	if entry, _ := tools[0].(map[string]any); entry["type"] != "web_search" {
		t.Fatalf("chat tool entry = %+v", tools[0])
	}

	out, _, err = compiled.Invoke(context.Background(), "onRequest", Context{Endpoint: "responses"}, json.RawMessage(`{"model":"gpt-4o","tools":[{"type":"web_search"}]}`))
	if err != nil {
		t.Fatalf("responses injection: %v", err)
	}
	var responses map[string]any
	if err := json.Unmarshal(out, &responses); err != nil {
		t.Fatal(err)
	}
	existing, _ := responses["tools"].([]any)
	if len(existing) != 1 {
		t.Fatalf("a client's own web_search tool was duplicated: %+v", responses["tools"])
	}
}

// presetBundle bundles one preset and inspects it, which is how a preset is
// exercised without installing it anywhere. The inspected settings are carried
// along so ctx.config holds the declared defaults, exactly as it does at
// runtime.
// presetToolCall 按预设声明的第一个工具构造一次工具调用。
//
// 工具名由预设自己决定（搜索类预设都带提供方后缀，这样多个搜索器才能共存），
// 所以测试从 preset.Tools 取名，而不是写死 "web_search"——写死会让改名后的
// 处理器永远匹配不上，测试却"通过"（错误为 nil），把回归漏掉。
func presetToolCall(t *testing.T, compiled *Compiled, arguments map[string]any) json.RawMessage {
	t.Helper()
	if len(compiled.Tools) == 0 {
		t.Fatalf("%s 没有声明任何工具", compiled.Manifest.ID)
	}
	var function struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(compiled.Tools[0].Function, &function); err != nil {
		t.Fatalf("%s: %v", compiled.Manifest.ID, err)
	}
	encoded, err := json.Marshal(map[string]any{"name": function.Name, "arguments": arguments})
	if err != nil {
		t.Fatal(err)
	}
	return json.RawMessage(encoded)
}

func presetBundle(t *testing.T, id string) *Compiled {
	t.Helper()
	for _, preset := range Presets() {
		if preset.ID != id {
			continue
		}
		bundle, err := bundleTree(preset.Files)
		if err != nil {
			t.Fatalf("bundle %s: %v", id, err)
		}
		compiled := &Compiled{Bundle: bundle, Manifest: preset.Manifest, Settings: preset.Settings}
		if err := compiled.Inspect(context.Background()); err != nil {
			t.Fatalf("inspect %s: %v", id, err)
		}
		return compiled
	}
	return nil
}

// webFetchFixture is a page with the parts a reader has to separate: navigation,
// a cookie banner, an article with an absolute and a relative link, a code
// block, a table, a comment thread and a footer.
const webFetchFixture = `<!doctype html>
<html lang="en">
<head>
  <title>Understanding Widgets | Example Site</title>
  <meta charset="utf-8">
  <base href="https://example.com/posts/">
  <style>.cookie { display: none }</style>
  <script>window.tracking = "nav footer cookie-banner must not appear";</script>
</head>
<body>
  <nav class="main-nav"><a href="/products">Products</a> <a href="/pricing">Pricing</a> <a href="/about">About us</a></nav>
  <header class="site-header"><h1>Example Site</h1></header>
  <div class="cookie-banner">We use cookies to improve your experience. Accept all cookies.</div>
  <main>
    <article class="post-content">
      <h1>Understanding widgets</h1>
      <p>The first widget was built in 1846 by an engineer who needed&nbsp;a better gear; see the <a href="/docs/spec">specification</a>.</p>
      <h2>How they work</h2>
      <ul><li>Spin the <strong>crank</strong></li><li>Watch the gears turn</li></ul>
      <pre><code>widget --spin --count 3
widget --status</code></pre>
      <table><tr><th>Model</th><th>Gears</th></tr><tr><td>W1</td><td>3</td></tr></table>
      <p>More at <a href="https://docs.example.org/widgets">the docs site</a> and <a href="javascript:void(0)">nowhere</a>.</p>
    </article>
    <aside class="related-posts"><h2>Related posts</h2><ul><li><a href="/blog/1">Ten gears</a></li></ul></aside>
  </main>
  <div class="comments" id="comments"><h2>Comments</h2><p>Great article, thanks for sharing this with everyone!</p></div>
  <footer class="site-footer"><p>Copyright 2026 Example Site. All rights reserved.</p></footer>
</body>
</html>`

// webFetch runs the preset's tool and returns the text it hands the model.
func webFetch(t *testing.T, compiled *Compiled, arguments map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"name": "web_fetch", "arguments": arguments})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := compiled.Invoke(context.Background(), "onToolCall", Context{}, payload)
	if err != nil {
		t.Fatalf("web_fetch: %v", err)
	}
	var result struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("output %s: %v", string(out), err)
	}
	return result.Content
}

func webFetchError(t *testing.T, compiled *Compiled, arguments map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"name": "web_fetch", "arguments": arguments})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = compiled.Invoke(context.Background(), "onToolCall", Context{}, payload)
	if err == nil {
		t.Fatal("the tool accepted input it should have rejected")
	}
	return err.Error()
}

func TestPresets_WebFetchRejectsUnsupportedTargets(t *testing.T) {
	compiled := presetBundle(t, "web-fetch")
	if compiled == nil {
		t.Skip("web-fetch is not in the catalog")
	}
	// These rejections happen before the network is touched, so the test never
	// depends on a fetch.
	for _, entry := range []struct{ name, url, want string }{
		{"empty", "", "url is required"},
		{"file scheme", "file:///etc/passwd", "only http and https"},
		{"ftp scheme", "ftp://example.com/page", "only http and https"},
		{"javascript scheme", "javascript:alert(1)", "only http and https"},
		{"credentials in the url", "https://user:secret@example.com/page", "remove the credentials"},
	} {
		got := webFetchError(t, compiled, map[string]any{"url": entry.url})
		if !strings.Contains(got, entry.want) {
			t.Fatalf("%s: error = %q, want it to mention %q", entry.name, got, entry.want)
		}
	}
}

func TestPresets_WebFetchKeepsContentAndDropsFurniture(t *testing.T) {
	compiled := presetBundle(t, "web-fetch")
	if compiled == nil {
		t.Skip("web-fetch is not in the catalog")
	}
	result := webFetch(t, compiled, map[string]any{
		"url":     "https://example.com/posts/widgets",
		"content": webFetchFixture,
	})

	for _, want := range []string{
		"Title: Understanding Widgets",
		"# Understanding widgets",
		"## How they work",
		"needed a better gear", // &nbsp; decoded
		"[specification](https://example.com/docs/spec)",
		"[the docs site](https://docs.example.org/widgets)",
		"nowhere", // a javascript: link keeps its text, loses the target
		"- Spin the **crank**",
		"widget --spin --count 3",
		"Model | Gears",
	} {
		if !strings.Contains(result, want) {
			t.Fatalf("result is missing %q:\n%s", want, result)
		}
	}
	for _, unwanted := range []string{
		"Accept all cookies", // cookie banner
		"Products",           // navigation
		"Related posts",      // aside
		"Ten gears",          // link list beside the article
		"Great article",      // comment thread
		"Copyright 2026",     // footer
		"window.tracking",    // script
		"display: none",      // style
		"javascript:void(0)", // a script-only href
	} {
		if strings.Contains(result, unwanted) {
			t.Fatalf("result kept %q:\n%s", unwanted, result)
		}
	}
}

func TestPresets_WebFetchTextFormatAndTruncation(t *testing.T) {
	compiled := presetBundle(t, "web-fetch")
	if compiled == nil {
		t.Skip("web-fetch is not in the catalog")
	}
	asText := webFetch(t, compiled, map[string]any{
		"url":     "https://example.com/posts/widgets",
		"content": webFetchFixture,
		"format":  "text",
	})
	if !strings.Contains(asText, "specification (https://example.com/docs/spec)") {
		t.Fatalf("text format did not spell out the link target:\n%s", asText)
	}
	if strings.Contains(asText, "**") || strings.Contains(asText, "](") {
		t.Fatalf("text format kept markup:\n%s", asText)
	}

	// The cap must be honoured and reported: a model that cannot tell it lost
	// text will answer from a partial page.
	long := "<html><head><title>Long page</title></head><body><article><h1>Long page</h1>" +
		strings.Repeat("<p>Lorem ipsum dolor sit amet, consectetur adipiscing elit sed do eiusmod tempor.</p>", 90) +
		"</article></body></html>"
	short := webFetch(t, compiled, map[string]any{
		"url":      "https://example.com/long",
		"content":  long,
		"maxChars": 1000,
	})
	if !strings.Contains(short, "characters omitted") {
		t.Fatalf("truncation was not reported:\n%s", short)
	}
	if len(short) > 1600 {
		t.Fatalf("maxChars was not applied: %d characters", len(short))
	}
}

func TestPresets_WebFetchRejectsOversizedPaste(t *testing.T) {
	compiled := presetBundle(t, "web-fetch")
	if compiled == nil {
		t.Skip("web-fetch is not in the catalog")
	}
	big := strings.Repeat("x", (1024*1024)+1)
	got := webFetchError(t, compiled, map[string]any{"url": "https://example.com/", "content": big})
	if !strings.Contains(got, "larger than") {
		t.Fatalf("error = %q", got)
	}
}

func TestPresets_CatalogLoads(t *testing.T) {
	presets := Presets()
	if len(presets) < 4 {
		t.Fatalf("catalog only has %d presets: %v", len(presets), presetProblems())
	}
	for _, preset := range presets {
		if preset.ID == "" || preset.Name == "" || preset.Description == "" {
			t.Fatalf("preset %s is incomplete: %+v", preset.ID, preset)
		}
		if preset.Category == "" || preset.Category == "other" {
			t.Fatalf("preset %s has no category", preset.ID)
		}
		if _, ok := preset.Files[EntryFile]; !ok {
			t.Fatalf("preset %s has no %s", preset.ID, EntryFile)
		}
	}
	for _, problem := range presetProblems() {
		// 尚未写 manifest.yaml 的目录是在建草稿（loadPresets 会跳过、商店不会展示），
		// 不是"被悄悄丢出目录的预设"。
		if strings.Contains(problem, "no manifest.yaml") {
			continue
		}
		t.Fatalf("presets were dropped from the catalog: %s", problem)
	}
}

func TestPresets_Install(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	preset, ok := PresetByID("web-search-bing")
	if !ok {
		t.Skip("bing preset is not in the catalog")
	}
	installed, err := m.InstallPreset(context.Background(), preset, "")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if installed.Manifest.ID != preset.ID || installed.Status != "ready" {
		t.Fatalf("installed = %+v", installed)
	}
	if _, err := os.Stat(filepath.Join(root, preset.ID, EntryFile)); err != nil {
		t.Fatalf("entrypoint missing: %v", err)
	}
	if _, err := m.InstallPreset(context.Background(), preset, ""); err == nil {
		t.Fatal("installing twice was allowed")
	}
	// Installing under another id is how a user runs two search backends.
	second, err := m.InstallPreset(context.Background(), preset, "bing-backup")
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if second.Manifest.ID != "bing-backup" || len(m.List()) != 2 {
		t.Fatalf("second install = %+v list=%d", second, len(m.List()))
	}
}

func TestPresets_DeclareNoCredentials(t *testing.T) {
	// A preset ships configuration knobs, never a key: a literal one would be
	// the same for every user who installs it.
	assignments := regexp.MustCompile(`(?m)^\s*(?:const|let|var)\s+[A-Za-z0-9_]*(?:key|token|secret|password)\w*\s*=\s*["'][^"']{8,}["']`)
	// 只匹配"长得像密钥"的默认值：常见密钥前缀，或一长串带数字的随机字符。
	// 早先的规则是"任何 8 字符以上的默认值"，它会把 "Moderate" 这类选择项的
	// 正常默认值也判成密钥——于是给下拉框补一个默认值就会让整个目录检查失败。
	// （Go 的 regexp 是 RE2，不支持前后瞻，所以用两条独立规则表达。）
	keyPrefixed := regexp.MustCompile(`default:\s*["'](?:sk|pk|rk|xai|gsk|glm|hf|key)[-_][A-Za-z0-9_-]{8,}["']`)
	randomLike := regexp.MustCompile(`default:\s*["'][A-Za-z0-9_-]{24,}["']`)
	for _, preset := range Presets() {
		for name, data := range preset.Files {
			if assignments.MatchString(data) {
				t.Fatalf("preset %s/%s assigns a credential literal", preset.ID, name)
			}
			if keyPrefixed.MatchString(data) || randomLike.MatchString(data) {
				t.Fatalf("preset %s/%s carries a credential-looking default", preset.ID, name)
			}
		}
		for _, field := range preset.Settings {
			if field.Secret && field.Default != nil {
				t.Fatalf("preset %s setting %s has a default secret", preset.ID, field.Key)
			}
		}
	}
}

func TestPresets_DeclareOnlyPublicHosts(t *testing.T) {
	for _, preset := range Presets() {
		for _, host := range preset.Hosts {
			if host == "" {
				t.Fatalf("preset %s declares an empty host", preset.ID)
			}
			// "*" (any public host) and a leading subdomain wildcard are the only
			// wildcard shapes allowed: the public-address check bounds both, while
			// a stray "*" inside a name is a typo that silently widens the
			// permission to something the author did not write.
			if host != "*" && strings.Contains(host, "*") && !strings.HasPrefix(host, "*.") {
				t.Fatalf("preset %s declares wildcard host %q", preset.ID, host)
			}
			if strings.ContainsAny(host, "?[]") {
				t.Fatalf("preset %s declares host %q", preset.ID, host)
			}
			if _, err := netip.ParseAddr(host); err == nil {
				t.Fatalf("preset %s declares the address %q instead of a hostname", preset.ID, host)
			}
			if err := validateHostPattern(host); err != nil {
				t.Fatalf("preset %s declares host %q: %v", preset.ID, host, err)
			}
		}
	}
}

func TestCheckPublicHost(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "localhost", "10.1.2.3", "192.168.1.10", "172.16.0.5",
		"169.254.169.254", "0.0.0.0", "::1", "fc00::1", "fe80::1", "100.64.0.1",
		"240.0.0.1", "255.255.255.255", "metadata.google.internal",
	}
	for _, host := range blocked {
		if err := checkPublicHost(host); err == nil {
			t.Fatalf("host %s was allowed", host)
		}
	}
	// A public address must pass. The resolver is not consulted for literals,
	// so this assertion does not depend on the network.
	if err := checkPublicHost("8.8.8.8"); err != nil {
		t.Fatalf("public literal rejected: %v", err)
	}
	if err := checkPublicHost("2606:4700:4700::1111"); err != nil {
		t.Fatalf("public IPv6 literal rejected: %v", err)
	}
}

func TestPresets_InstallRefusesInvalidTarget(t *testing.T) {
	presets := Presets()
	if len(presets) == 0 {
		t.Skip("no presets")
	}
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.InstallPreset(context.Background(), presets[0], "bad id"); err == nil {
		t.Fatal("invalid target id was accepted")
	}
}

func TestPresets_SettingsAreRenderable(t *testing.T) {
	raw, err := json.Marshal(Presets())
	if err != nil {
		t.Fatal(err)
	}
	// The UI renders the store from this payload; settings must survive the
	// round trip with the fields the form needs.
	var decoded []struct {
		ID       string `json:"id"`
		Settings []struct {
			Key       string   `json:"key"`
			Component string   `json:"component"`
			Label     string   `json:"label"`
			Options   []string `json:"options"`
		} `json:"settings"`
		Hosts []string `json:"hosts"`
		Tools []string `json:"tools"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, preset := range decoded {
		for _, field := range preset.Settings {
			if field.Key == "" || field.Component == "" || field.Label == "" {
				t.Fatalf("preset %s has an unusable setting %+v", preset.ID, field)
			}
		}
	}
}

// 预设工具体系的两条硬约束：
//  1. 每个预设都要能打包并加载——目录里存在一个坏预设，存储页就会少一张卡片；
//  2. 工具名必须在整个目录里唯一。同名会让后安装的预设被工具冲突策略静默跳过，
//     用户看到的是"装了但模型调不动"。搜索类预设过去全部叫 web_search，
//     因此同时启用多个搜索器是不可能的。
func TestPresets_CatalogLoadsAndToolNamesAreUnique(t *testing.T) {
	// 只对"声明了自己的预设"较真：缺少 manifest.yaml 的目录是在建的草稿，
	// 商店本来就不会把它摆出来（loadPresets 会跳过），因此不算目录损坏。
	for _, problem := range presetProblems() {
		if strings.Contains(problem, "no manifest.yaml") {
			continue
		}
		t.Fatalf("预设目录里有无法加载的条目：%s", problem)
	}
	catalog := Presets()
	if len(catalog) == 0 {
		t.Fatal("预设目录是空的")
	}
	owner := map[string]string{}
	for _, preset := range catalog {
		if preset.ID == "" || preset.Name == "" {
			t.Fatalf("预设缺少 id 或 name：%+v", preset)
		}
		for _, tool := range preset.Tools {
			if previous, taken := owner[tool]; taken {
				t.Errorf("工具名 %q 同时属于 %s 和 %s：两个预设无法同时启用", tool, previous, preset.ID)
			}
			owner[tool] = preset.ID
		}
	}
	// 每个搜索提供方都要有一个带提供方后缀的独立工具名，这是多搜索器共存的前提。
	for _, id := range []string{
		"web-search-bing", "web-search-brave", "web-search-doubao", "web-search-exa",
		"web-search-glm", "web-search-google", "web-search-parallel", "web-search-tavily",
		"deepseek-tools",
	} {
		preset, ok := PresetByID(id)
		if !ok {
			t.Errorf("预设 %s 缺失", id)
			continue
		}
		for _, tool := range preset.Tools {
			if tool == "web_search" {
				t.Errorf("预设 %s 仍在使用无后缀的 web_search：会与其它搜索器撞名", id)
			}
			if !strings.HasPrefix(tool, "web_search_") && !strings.HasPrefix(tool, "deepseek_") {
				t.Errorf("预设 %s 的工具名 %q 缺少提供方后缀", id, tool)
			}
		}
	}
}

// DeepSeek 预设按官方文档注册搜索与抓取两个工具，且主机在 manifest 里放行。
// 余额不在这里：它是运维信息，由供应商额度卡片展示（见 config/quotapresets/deepseek.yaml）。
func TestPresets_DeepSeekToolsDeclared(t *testing.T) {
	preset, ok := PresetByID("deepseek-tools")
	if !ok {
		t.Fatal("deepseek-tools 预设缺失")
	}
	if len(preset.Tools) != 2 {
		t.Fatalf("deepseek-tools 工具数 = %v", preset.Tools)
	}
	for _, want := range []string{"deepseek_search", "deepseek_fetch"} {
		if !containsString(preset.Tools, want) {
			t.Errorf("缺少工具 %s（现有 %v）", want, preset.Tools)
		}
	}
	if !containsString(preset.Hosts, "api.deepseek.com") {
		t.Fatalf("manifest 放行的主机 = %v，缺少 api.deepseek.com", preset.Hosts)
	}
	// 三个工具共用一个密钥设置项，名称为 apiKey。
	found := false
	for _, field := range preset.Settings {
		if field.Key == "apiKey" {
			found = true
			if !field.Secret {
				t.Error("apiKey 应当声明为 secret")
			}
		}
	}
	if !found {
		t.Error("缺少 apiKey 设置项")
	}
}

// 多个搜索器同时启用时，模型必须看到每个搜索器各自的工具名。这是"改了名就能
// 共存"的直接验证：把那 9 个带搜索工具的预设全部装进同一个管理器，
// 检查注入后的工具列表既不缺也不重名。
func TestPresets_MultipleSearchBackendsCoexist(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{
		"web-search-bing", "web-search-brave", "web-search-doubao", "web-search-exa",
		"web-search-glm", "web-search-google", "web-search-parallel", "web-search-tavily",
		"deepseek-tools",
	}
	installed := map[string]string{} // 工具名 → 预设 id
	for _, id := range ids {
		preset, ok := PresetByID(id)
		if !ok {
			t.Fatalf("预设 %s 不在目录里", id)
		}
		// 安装到默认 id：它们互不覆盖，因为每个都有自己的目录名。
		if _, err := m.InstallPreset(context.Background(), preset, preset.ID); err != nil {
			t.Fatalf("安装 %s: %v", id, err)
		}
		for _, tool := range preset.Tools {
			if previous, taken := installed[tool]; taken {
				t.Fatalf("安装后工具名 %q 同时属于 %s 与 %s", tool, previous, id)
			}
			installed[tool] = id
		}
	}
	// 管理器里每个预设只应贡献它自己声明的工具，且全部处于启用状态。
	items := m.List()
	if len(items) != len(ids) {
		t.Fatalf("已安装 %d 个，期望 %d 个", len(items), len(ids))
	}
	seen := map[string]bool{}
	for _, item := range items {
		compiled, ok := m.Compiled(item.Manifest.ID)
		if !ok {
			t.Fatalf("%s 未能加载", item.Manifest.ID)
		}
		for _, tool := range compiled.Tools {
			var function struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(tool.Function, &function); err != nil {
				t.Fatal(err)
			}
			if seen[function.Name] {
				t.Fatalf("工具 %s 在多个预设里重复注册", function.Name)
			}
			seen[function.Name] = true
		}
	}
	// 至少要能看到每个搜索引擎一个工具，外加 DeepSeek 的三个。
	for tool := range installed {
		if !seen[tool] {
			t.Errorf("工具 %s 没有出现在加载后的管理器里", tool)
		}
	}
}

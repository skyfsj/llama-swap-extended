package spec

import (
	"encoding/json"
	"sort"
)

var sectionLabels = map[string]string{
	"advanced": "高级", "general": "常规", "integrations": "集成", "logging": "日志",
	"modelFiles": "模型文件", "models": "模型", "observability": "可观测性", "peers": "节点",
	"performance": "性能", "routing": "路由", "runtimes": "运行时", "security": "安全",
	"storage": "存储", "ui": "界面", "upstream": "上游",
}

// Root returns the immutable configuration field catalogue. The catalogue is
// intentionally small at the root and delegates complex entity editing to the
// settings-center editors. Unknown keys remain valid in the generated schema
// so older YAML can be loaded and warned about by the engine.
func Root() *Field {
	root := &Field{
		Kind: KObject,
		Children: []*Field{
			bounded(field("healthCheckTimeout", KInt, "健康检查超时（秒）", "general", CompNumber, 120), 0, 86400),
			bounded(field("globalTTL", KInt, "全局模型空闲时间（秒）", "general", CompNumber, 0), 0, 2147483647),
			bounded(field("unloadTimeout", KInt, "模型卸载超时（秒）", "general", CompNumber, 10), 0, 86400),
			bounded(field("startPort", KInt, "模型端口起始值", "general", CompNumber, 5800), 1, 65535),
			field("sendLoadingState", KBool, "发送模型加载状态", "general", CompSwitch, false),
			field("includeAliasesInList", KBool, "在模型列表中包含别名", "general", CompSwitch, false),
			field("logRequests", KBool, "记录请求日志", "logging", CompSwitch, false),
			field("logLevel", KString, "日志级别", "logging", CompSelect, "info", "debug", "info", "warn", "error"),
			field("logTimeFormat", KString, "日志时间格式", "logging", CompSelect, "", "", "rfc3339", "rfc3339nano", "kitchen", "stamp", "stampmilli", "stampmicro", "stampnano"),
			field("logToStdout", KString, "标准输出日志来源", "logging", CompSelect, "proxy", "proxy", "upstream", "both", "none"),
			objectField("logStorage", "故障日志存储", "logging", false,
				field("path", KString, "存储目录（留空使用默认目录）", "logging", CompText, ""),
				bounded(field("maxFiles", KInt, "每类最多保留份数", "logging", CompNumber, 5), 1, 100)),
			field("metricsMaxInMemory", KInt, "内存中保留的指标数量", "observability", CompNumber, 1000),
			objectField("performance", "性能监控", "performance", false,
				field("disabled", KBool, "停用性能监控", "performance", CompSwitch, false),
				field("every", KDuration, "采样间隔", "performance", CompDuration, "5s")),
			objectField("store", "持久化存储", "storage", false,
				field("path", KString, "存储路径", "storage", CompText, "")),
			objectField("ui", "控制台界面", "ui", false,
				objectField("activity", "活动记录", "ui", false,
					field("session_id", KStringList, "会话标识", "ui", CompList, nil))),
			objectField("runtimeManager", "运行时管理器", "runtimes", false,
				field("root", KString, "运行时目录", "runtimes", CompText, ""),
				field("buildWhileBusy", KBool, "忙碌时允许构建", "runtimes", CompSwitch, false),
				field("sourceAllowlist", KStringList, "允许的源码目录", "runtimes", CompList, nil),
				field("containerRegistries", KStringList, "容器仓库", "runtimes", CompList, nil),
				field("operationTimeout", KDuration, "操作超时", "runtimes", CompDuration, "30m")),
			entityField("runtimes", "运行时定义", "runtimes", "runtime", true),
			objectField("resourceBudget", "资源预算", "runtimes", false,
				field("vramMiB", KInt, "显存上限（MiB）", "runtimes", CompNumber, 0),
				field("ramMiB", KInt, "内存上限（MiB）", "runtimes", CompNumber, 0),
				field("autoEvict", KBool, "自动回收", "runtimes", CompSwitch, false),
				field("queueLoads", KBool, "资源不足时排队", "runtimes", CompSwitch, false)),
			modelEntityField(),
			entityField("peers", "远端节点", "peers", "peer", true),
			entityField("profiles", "运行时配置档案", "routing", "profile", true),
			entityField("selectors", "模型选择器", "routing", "selector", true),
			objectField("routing", "路由与调度", "routing", false,
				objectField("scheduler", "调度器", "routing", false,
					field("use", KString, "调度器", "routing", CompSelect, "fifo", "fifo"),
					objectField("settings", "调度设置", "routing", false,
						objectField("fifo", "FIFO 设置", "routing", false,
							field("priority", KIntMap, "模型优先级", "routing", CompMap, nil)))),
				objectField("router", "路由器", "routing", false,
					field("use", KString, "路由器", "routing", CompSelect, "group", "group", "matrix", "gpus"))),
			objectField("modelFiles", "模型文件与下载", "modelFiles", false,
				bounded(field("maxFiles", KInt, "最多文件数", "modelFiles", CompNumber, 2000), 0, 100000),
				bounded(field("maxDepth", KInt, "扫描深度", "modelFiles", CompNumber, 8), 0, 64),
				objectField("downloads", "下载设置", "modelFiles", false,
					field("enabled", KBool, "启用下载", "modelFiles", CompSwitch, true),
					bounded(field("workers", KInt, "并发下载数", "modelFiles", CompNumber, 1), 0, 32),
					bounded(field("fileWorkers", KInt, "并发文件数", "modelFiles", CompNumber, 4), 0, 16),
					bounded(field("chunkWorkers", KInt, "单文件分块并发数", "modelFiles", CompNumber, 4), 0, 16),
					bounded(field("chunkSizeMiB", KInt, "分块大小（MiB）", "modelFiles", CompNumber, 16), 0, 1024),
					bounded(field("chunkThresholdMiB", KInt, "启用分块的文件大小（MiB）", "modelFiles", CompNumber, 64), 0, 4096),
					bounded(field("maxRetries", KInt, "最大重试次数", "modelFiles", CompNumber, 5), 0, 20),
					bounded(field("maxTaskRetries", KInt, "自动任务重试次数", "modelFiles", CompNumber, 3), 0, 10),
					field("retryBackoff", KDuration, "重试间隔", "modelFiles", CompDuration, "2s"),
					field("hfBaseURL", KString, "Hugging Face 地址", "modelFiles", CompText, ""),
					field("modelScopeBaseURL", KString, "ModelScope 地址", "modelFiles", CompText, ""),
					field("hfTokenEnv", KString, "Hugging Face Token 环境变量", "modelFiles", CompText, "HF_TOKEN"),
					field("modelScopeTokenEnv", KString, "ModelScope Token 环境变量", "modelFiles", CompText, "MODELSCOPE_API_TOKEN"),
					sensitiveField("hfToken", "Hugging Face Token", "modelFiles"),
					sensitiveField("modelScopeToken", "ModelScope Token", "modelFiles"))),
			objectField("upstream", "上游转发", "upstream", false,
				field("ignorePaths", KStringList, "忽略路径规则", "upstream", CompList, nil)),
			objectField("anthropic", "Anthropic 兼容层", "integrations", false,
				objectField("cacheFix", "缓存修复", "integrations", false,
					field("mode", KString, "模式", "integrations", CompSelect, "auto", "off", "auto", "force"),
					field("telemetry", KBool, "发送遥测", "integrations", CompSwitch, false),
					objectField("transforms", "请求转换", "integrations", false,
						field("fingerprintStrip", KBool, "移除指纹", "integrations", CompSwitch, true),
						field("sortStabilization", KBool, "稳定排序", "integrations", CompSwitch, true),
						field("freshSessionSort", KBool, "新会话排序", "integrations", CompSwitch, true),
						field("identityNormalization", KBool, "规范化身份", "integrations", CompSwitch, true),
						field("cacheControlNormalize", KBool, "规范化缓存控制", "integrations", CompSwitch, true),
						field("ttlManagement", KBool, "管理缓存 TTL", "integrations", CompSwitch, true),
						field("thinkingSanitize", KString, "思考内容处理", "integrations", CompSelect, "safe", "off", "safe", "experimental"),
						field("ccVersionNormalize", KString, "版本规范化", "integrations", CompSelect, "audit", "off", "audit", "on"),
						field("highRisk", KString, "高风险转换", "integrations", CompSelect, "audit", "off", "audit", "on", "experimental")))),
			objectField("pricing", "价格目录", "observability", false,
				objectField("modelsDev", "models.dev", "observability", false,
					field("enabled", KBool, "启用价格同步", "observability", CompSwitch, true),
					field("refreshEvery", KDuration, "刷新间隔", "observability", CompDuration, "24h"),
					field("url", KString, "数据地址", "observability", CompText, "https://models.dev/api.json"))),
			objectField("audit", "审计记录", "observability", false,
				field("enabled", KBool, "启用审计", "observability", CompSwitch, true),
				field("retention", KDuration, "保留时间", "observability", CompDuration, "720h"),
				nonNegative(field("maxBytes", KInt, "最大大小（字节）", "observability", CompNumber, 5368709120)),
				field("storeMedia", KBool, "保存媒体", "observability", CompSwitch, true),
				field("redactHeaders", KBool, "隐藏请求头", "observability", CompSwitch, true)),
			objectField("hooks", "生命周期钩子", "advanced", false,
				objectField("on_startup", "启动时", "advanced", false,
					providerBacked(field("preload", KStringList, "预加载模型", "advanced", CompList, nil), "models"))),
			anyField("macros", "宏定义", "advanced", CompMap),
			objectField("extensions", "扩展", "advanced", false,
				objectField("logging", "日志存储", "logging", false,
					field("logMaxDiskMiB", KInt, "日志总容量（MiB）", "logging", CompNumber, 100),
					field("logRetainDays", KInt, "保留天数", "logging", CompNumber, 7))),
			legacyField("groups", "旧版模型组（保存时迁移）"),
			legacyField("matrix", "旧版矩阵（保存时迁移）"),
			sensitiveAnyField("apiKeys", "启动 API 密钥", "security", CompPassword),
		},
	}
	// Provider IDs are stable server-side contracts. The UI uses them to
	// fetch current options instead of hard-coding every deployment's values.
	for _, child := range root.Children {
		switch child.Name {
		case "logLevel":
			child.UI.Provider = "log-level"
		case "logTimeFormat":
			child.UI.Provider = "log-time-format"
		case "logToStdout":
			child.UI.Provider = "log-stdout"
		case "models":
			child.UI.Provider = "models"
		}
	}
	// Providers are attached to fields in the same catalogue that describes
	// their shape. The browser never needs to know how an option was sourced.
	root.walk("", func(_ string, _ string, f *Field) {
		if f.Name == "runtime" && f.UI.Section == "models" {
			f.UI.Provider = "runtimes"
		}
		if f.Name == "useModelName" {
			f.UI.Provider = "models"
		}
	})
	return root
}

// KnownTopLevel returns the keys understood by the structured settings form.
// The loader still accepts additional keys; this set is only used to produce
// non-blocking unknown-field warnings.
func KnownTopLevel() map[string]struct{} {
	known := make(map[string]struct{})
	for _, field := range Root().Children {
		known[field.Name] = struct{}{}
	}
	return known
}

// bounded mirrors the numeric range the config loader enforces so the
// settings form can reject out-of-range values before the server does.
func bounded(f *Field, min, max int64) *Field {
	f.Min = &min
	f.Max = &max
	return f
}

func field(name string, kind Kind, desc, section, component string, def any, enum ...string) *Field {
	f := &Field{Name: name, Kind: kind, Desc: desc, Default: def, UI: UI{Section: section, Label: desc, Component: component}}
	if len(enum) > 0 {
		f.Enum = append([]string(nil), enum...)
	}
	return f
}

// nonNegative mirrors loader rules that only forbid negative values.
func nonNegative(f *Field) *Field {
	min := int64(0)
	f.Min = &min
	return f
}

// providerBacked attaches a server-registered dynamic option provider, so the
// settings center fetches the current values from
// /api/settings/options/{provider} instead of the schema hard-coding one
// deployment's models or runtimes.
//
// On a list field the provider supplies the choices for a multi-select; without
// it the field renders as free-text rows, and a mistyped entry is dropped
// during config loading with no error.
func providerBacked(f *Field, provider string) *Field {
	f.UI.Provider = provider
	return f
}

func objectField(name, desc, section string, sensitive bool, children ...*Field) *Field {
	return &Field{Name: name, Kind: KObject, Desc: desc, Sensitive: sensitive, HiddenUI: len(children) > 0, Children: children, UI: UI{Section: section, Label: desc, Component: CompEditor, Editor: name}}
}

func anyField(name, desc, section, component string) *Field {
	return &Field{Name: name, Kind: KAny, Desc: desc, UI: UI{Section: section, Label: desc, Component: component}}
}

func sensitiveField(name, desc, section string) *Field {
	return &Field{Name: name, Kind: KString, Desc: desc, Sensitive: true, UI: UI{Section: section, Label: desc, Component: CompPassword}}
}

func sensitiveAnyField(name, desc, section, component string) *Field {
	f := anyField(name, desc, section, component)
	f.Sensitive = true
	return f
}

func entityField(name, desc, section, editor string, advanced bool) *Field {
	return &Field{Name: name, Kind: KEntityMap, Desc: desc, Advanced: advanced, Template: &Field{Kind: KAnyMap}, UI: UI{Section: section, Label: desc, Component: CompEditor, Editor: editor}}
}

func modelEntityField() *Field {
	hidden := func(name string, kind Kind, desc string, component string) *Field {
		return &Field{Name: name, Kind: kind, Desc: desc, HiddenUI: true, UI: UI{Section: "models", Label: desc, Component: component}}
	}
	backend := hidden("backend", KObject, "后端参数", CompEditor)
	backend.Children = []*Field{
		hidden("type", KString, "后端类型", CompSelect),
		hidden("runtime", KString, "运行时", CompSelect),
		hidden("protocol", KString, "协议", CompSelect),
		hidden("apis", KStringList, "接口能力", CompList),
		hidden("discover", KBool, "自动发现接口", CompSwitch),
		hidden("args", KStringList, "启动参数", CompList),
	}
	template := &Field{Kind: KObject, Children: []*Field{
		// Keep the historical list kind in the template for compatibility with
		// callers of FindField; the model editor renders the actual string cmd
		// field from the configuration type.
		hidden("cmd", KStringList, "启动命令", CompText),
		hidden("proxy", KString, "代理地址", CompText),
		hidden("aliases", KStringList, "模型别名", CompList),
		hidden("ttl", KInt, "空闲时间", CompNumber),
		hidden("unloadAfter", KInt, "自动卸载时间", CompNumber),
		backend,
		hidden("capabilities", KObject, "能力声明", CompEditor),
	}}
	return &Field{Name: "models", Kind: KEntityMap, Desc: "模型配置", Advanced: true, Template: template, UI: UI{Section: "models", Label: "模型配置", Component: CompEditor, Editor: "model"}}
}

func legacyField(name, desc string) *Field {
	return &Field{Name: name, Kind: KAny, Desc: desc, Legacy: true, HiddenUI: true, UI: UI{Section: "advanced", Label: desc, Component: CompJSON}}
}

// SchemaDocument materializes the catalogue as the public JSON Schema. It is
// deterministic so a generated checked-in artifact can be compared in tests.
func SchemaDocument() map[string]any {
	root := Root()
	properties := make(map[string]any, len(root.Children))
	for _, child := range root.Children {
		properties[child.Name] = schemaFor(child)
	}
	return map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"$id":                  "https://github.com/mostlygeek/llama-swap/config-schema.json",
		"title":                "llama-swap configuration",
		"type":                 "object",
		"additionalProperties": true,
		"properties":           properties,
	}
}

func schemaFor(f *Field) map[string]any {
	if f == nil {
		return map[string]any{}
	}
	out := map[string]any{}
	switch f.Kind {
	case KBool:
		out["type"] = "boolean"
	case KInt:
		out["type"] = "integer"
	case KDuration:
		out["type"] = []string{"string", "integer"}
	case KStringList:
		out["type"] = "array"
		out["items"] = map[string]any{"type": "string"}
	case KStringMap, KIntMap, KStringListMap, KAnyMap, KEntityMap:
		out["type"] = "object"
		out["additionalProperties"] = schemaFor(f.Template)
	case KObjectList, KScalarOrObject:
		out["type"] = "array"
		if f.Item != nil {
			out["items"] = schemaFor(f.Item)
		} else if f.Template != nil {
			out["items"] = schemaFor(f.Template)
		}
	case KObject:
		out["type"] = "object"
		children := map[string]any{}
		for _, child := range f.Children {
			children[child.Name] = schemaFor(child)
		}
		out["properties"] = children
		out["additionalProperties"] = true
	default:
		out["type"] = "string"
	}
	if f.Default != nil {
		out["default"] = f.Default
	}
	if len(f.Enum) > 0 {
		out["enum"] = append([]string(nil), f.Enum...)
	}
	if f.Desc != "" {
		out["description"] = f.Desc
	}
	ui := map[string]any{"section": f.UI.Section, "label": f.UI.Label, "component": f.UI.Component, "order": f.UI.Order}
	if f.UI.Editor != "" {
		ui["editor"] = f.UI.Editor
	}
	if f.UI.Provider != "" {
		ui["provider"] = f.UI.Provider
	}
	if f.Sensitive {
		ui["sensitive"] = true
	}
	if f.Legacy {
		ui["legacy"] = true
	}
	if f.Restart {
		ui["restart"] = true
	}
	out["x-llama-swap-ui"] = ui
	return out
}

// Metadata returns the section/field tree consumed by the settings center.
func Metadata() map[string]any {
	sections := map[string][]map[string]any{}
	Root().walk("", func(path, pointer string, f *Field) {
		if path == "" || f.UI.Section == "" {
			return
		}
		sections[f.UI.Section] = append(sections[f.UI.Section], map[string]any{
			"path": pointer, "label": f.UI.Label, "hint": f.UI.Hint,
			"component": f.UI.Component, "editor": f.UI.Editor,
			"sensitive": f.Sensitive, "legacy": f.Legacy,
			"advanced": f.Advanced, "hidden": f.HiddenUI,
			"default": f.Default, "enum": f.Enum, "provider": f.UI.Provider,
			"required": f.Required, "min": f.Min, "max": f.Max,
		})
	})
	for section := range sections {
		sort.SliceStable(sections[section], func(i, j int) bool {
			return sections[section][i]["path"].(string) < sections[section][j]["path"].(string)
		})
	}
	labels := make(map[string]string, len(sectionLabels))
	for key, label := range sectionLabels {
		labels[key] = label
	}
	return map[string]any{"version": 1, "sections": sections, "sectionLabels": labels, "schema": SchemaDocument()}
}

// SchemaJSON is a stable encoding used by tests and the HTTP handler.
func SchemaJSON() ([]byte, error) {
	return json.MarshalIndent(SchemaDocument(), "", "  ")
}

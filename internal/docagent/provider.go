// Package docagent's provider layer exposes the documentation index through
// the tool shapes the MCP endpoint serves.
//
// Tools are namespaced by what they answer about: docs__* is this fork's own
// documentation, config__* is the configuration this server is running right
// now. Keeping the two apart means a model that wants advice about the live
// deployment never has to reason about whether it is reading a sample file.
package docagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Tool is one entry of tools/list.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Result is what a tool call returns. IsError marks a call that ran but could
// not answer, so the model reads the explanation and retries; only protocol
// failures become JSON-RPC errors.
type Result struct {
	Content string
	IsError bool
}

// Environment is the live state the provider answers about. It is an interface
// so the documentation tools can be exercised without a running Server.
type Environment interface {
	// RedactedConfigYAML returns the configuration this process is running,
	// with secrets replaced by placeholders.
	RedactedConfigYAML(ctx context.Context) (string, error)

	// ServerFacts describes the running instance for sys__overview.
	ServerFacts(ctx context.Context) map[string]any
}

// Provider answers tool calls over an index and an Environment.
type Provider struct {
	docs *Docs
	env  Environment
}

// NewProvider returns a provider over docs. A nil environment disables the
// live-state tools, which is what the documentation-only tests want.
func NewProvider(docs *Docs, env Environment) *Provider {
	return &Provider{docs: docs, env: env}
}

// maxListingDocs bounds docs__list so a small model is not handed the whole
// index at once; the listing says how many were omitted.
const maxListingDocs = 120

// maxDocLines bounds docs__get when the caller does not window the request.
const maxDocLines = 400

// maxResultBytes caps a tool result. Local models have small context windows,
// and a truncated answer with an explicit continuation beats a refusal.
const maxResultBytes = 32 << 10

// Tools returns the tool list.
func (p *Provider) Tools() []Tool {
	tools := []Tool{
		{
			Name: "docs__list",
			Description: "List llama-swap's documentation: every page and section with its id, " +
				"a one-line summary and tags. Call this first when you do not know what to search for.",
			InputSchema: objectSchema(nil),
		},
		{
			Name: "docs__search",
			Description: "Search llama-swap's documentation by keyword and get the best matching " +
				"sections with a short excerpt. Prefer this over reading a whole page.",
			InputSchema: objectSchema(map[string]any{
				"query": map[string]any{"type": "string", "description": "Words to search for, e.g. \"capability discovery\" or \"keep-alive\"."},
				"limit": map[string]any{"type": "integer", "description": "Maximum results (default 10)."},
			}, "query"),
		},
		{
			Name: "docs__get",
			Description: "Read one documentation section by id, optionally a window of it. " +
				"The result reports the source file and line so you can cite it.",
			InputSchema: objectSchema(map[string]any{
				"id":     map[string]any{"type": "string", "description": "A doc id from docs__list or docs__search, e.g. \"guides/configuration#capability-discovery\"."},
				"offset": map[string]any{"type": "integer", "description": "0-based body line to start at."},
				"limit":  map[string]any{"type": "integer", "description": "Maximum body lines to return."},
			}, "id"),
		},
	}
	if p.env != nil {
		tools = append(tools, Tool{
			Name: "config__get",
			Description: "Read the configuration this llama-swap is running right now, with credentials " +
				"redacted, selected by a jq query. Use it for advice about the active setup rather than " +
				"about the sample configuration.",
			InputSchema: objectSchema(map[string]any{
				"query": map[string]any{"type": "string", "description": "jq expression, e.g. \".models | keys\", \".models.qwen.context\", \".providers.deepseek\". Omit for the whole document."},
			}),
		}, Tool{
			Name:        "sys__overview",
			Description: "Facts about this instance: build version, how many models and providers are configured, which are running, and the host hardware summary.",
			InputSchema: objectSchema(nil),
		})
	}
	return tools
}

// HasTools reports whether any tool is registered. A provider with no
// documentation and no environment advertises none.
func (p *Provider) HasTools() bool { return len(p.Tools()) > 0 }

// Call runs one tool.
func (p *Provider) Call(ctx context.Context, name string, args map[string]json.RawMessage) Result {
	switch name {
	case "docs__list":
		return p.list()
	case "docs__search":
		return p.search(args)
	case "docs__get":
		return p.get(args)
	case "config__get":
		return p.config(ctx, args)
	case "sys__overview":
		return p.sys(ctx)
	default:
		return Result{Content: fmt.Sprintf("unknown tool %q", name), IsError: true}
	}
}

func (p *Provider) list() Result {
	if p.docs == nil {
		return Result{Content: "documentation is not available in this build", IsError: true}
	}
	listed := p.docs.List()
	if len(listed) == 0 {
		return Result{Content: "no documentation is indexed", IsError: true}
	}
	var b strings.Builder
	shown := min(len(listed), maxListingDocs)
	fmt.Fprintf(&b, "%d sections indexed; showing %d.\n", len(listed), shown)
	for _, meta := range listed[:shown] {
		fmt.Fprintf(&b, "\n%s\n  %s\n", meta.ID, meta.Title)
		if meta.Summary != "" {
			fmt.Fprintf(&b, "  %s\n", meta.Summary)
		}
		if len(meta.Tags) > 0 {
			fmt.Fprintf(&b, "  tags: %s\n", strings.Join(meta.Tags, ", "))
		}
	}
	if shown < len(listed) {
		fmt.Fprintf(&b, "\n%d further sections are not shown; use docs__search to find them.\n", len(listed)-shown)
	}
	return clip(Result{Content: b.String()})
}

func (p *Provider) search(args map[string]json.RawMessage) Result {
	query, err := stringArg(args, "query")
	if err != nil || strings.TrimSpace(query) == "" {
		return Result{Content: "docs__search requires a non-empty \"query\"", IsError: true}
	}
	limit, _ := intArg(args, "limit")
	hits := p.docs.Search(query, limit)
	if len(hits) == 0 {
		return Result{Content: fmt.Sprintf("no section of the documentation matches %q; call docs__list to see what is indexed.", query), IsError: true}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d matching sections:\n", len(hits))
	for _, hit := range hits {
		fmt.Fprintf(&b, "\n%s\n  %s\n", hit.ID, hit.Title)
		if hit.Snippet != "" {
			fmt.Fprintf(&b, "  %s\n", hit.Snippet)
		}
	}
	b.WriteString("\nRead one with docs__get.")
	return clip(Result{Content: b.String()})
}

func (p *Provider) get(args map[string]json.RawMessage) Result {
	id, err := stringArg(args, "id")
	if err != nil || strings.TrimSpace(id) == "" {
		return Result{Content: "docs__get requires an \"id\" from docs__list or docs__search", IsError: true}
	}
	offset, _ := intArg(args, "offset")
	limit, _ := intArg(args, "limit")
	if limit <= 0 {
		limit = maxDocLines
	}
	text, found := p.docs.Get(id, DocOptions{Offset: offset, Limit: limit})
	if !found {
		return Result{Content: fmt.Sprintf("no document with id %q; call docs__list for the index.", id), IsError: true}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", text.Title)
	fmt.Fprintf(&b, "source: %s line %d\n", text.Source, text.SourceLine)
	if text.Truncated {
		fmt.Fprintf(&b, "lines %d-%d of the section\n", text.StartLine, text.EndLine)
	}
	b.WriteString("\n")
	b.WriteString(text.Text)
	if text.Truncated {
		fmt.Fprintf(&b, "\n\n[truncated: continue with docs__get {\"id\": %q, \"offset\": %d}]", id, text.EndLine)
	}
	return clip(Result{Content: b.String()})
}

func (p *Provider) config(ctx context.Context, args map[string]json.RawMessage) Result {
	raw, err := p.env.RedactedConfigYAML(ctx)
	if err != nil {
		return Result{Content: fmt.Sprintf("could not read the running configuration: %v", err), IsError: true}
	}
	query, _ := stringArg(args, "query")
	selected, err := selectConfig(raw, query)
	if err != nil {
		return Result{Content: err.Error(), IsError: true}
	}
	return clip(Result{Content: selected})
}

func (p *Provider) sys(ctx context.Context) Result {
	facts := p.env.ServerFacts(ctx)
	encoded, err := json.MarshalIndent(facts, "", "  ")
	if err != nil {
		return Result{Content: fmt.Sprintf("could not encode server facts: %v", err), IsError: true}
	}
	return clip(Result{Content: string(encoded)})
}

// clip bounds a result and says so, so a model knows the answer continues.
func clip(result Result) Result {
	if len(result.Content) <= maxResultBytes {
		return result
	}
	cut := maxResultBytes
	for cut > 0 && !utf8Start(result.Content[cut]) {
		cut--
	}
	result.Content = result.Content[:cut] + "\n\n[truncated; narrow the request to see the rest]"
	return result
}

// utf8Start reports whether b begins a UTF-8 sequence, so a byte cut never
// splits a multi-byte character.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// objectSchema builds an object input schema with the given properties and
// required keys. A nil map means a tool that takes no arguments.
func objectSchema(properties map[string]any, required ...string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringArg(args map[string]json.RawMessage, key string) (string, error) {
	raw, ok := args[key]
	if !ok {
		return "", fmt.Errorf("missing %q", key)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%q must be a string", key)
	}
	return value, nil
}

func intArg(args map[string]json.RawMessage, key string) (int, error) {
	raw, ok := args[key]
	if !ok {
		return 0, fmt.Errorf("missing %q", key)
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, fmt.Errorf("%q must be an integer", key)
	}
	return value, nil
}

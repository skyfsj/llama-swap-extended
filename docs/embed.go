// Package docs embeds llama-swap's own documentation so it can be indexed and
// served to an LLM over the MCP endpoint (/api/mcp).
//
// The embedded set is deliberately the operator-facing documentation:
// docs/**/*.md plus the example directories they link to. Screenshots and the
// Grafana dashboard are excluded — they are megabytes of binary that no text
// endpoint can use.
//
// config.example.yaml is deliberately NOT embedded: it lives at the repository
// root, where a go:embed directive cannot reach it, and the MCP config tool
// answers from the running, redacted configuration instead, which describes the
// deployment the agent is actually looking at.
package docs

import "embed"

// Files is the embedded documentation tree, rooted at the docs/ directory.
//
// The directory is not embedded wholesale: extensions/design/ holds UI
// screenshots that no text endpoint can use and would only grow the binary.
//
//go:embed *.md extensions/*.md extensions/*.d.ts extensions/examples examples
var Files embed.FS

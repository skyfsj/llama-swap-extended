package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

// Server-tool artifacts.
//
// An extension's server tool is executed by the daemon, but unlike a caller-owned
// tool the caller never declared it. The vendor surfaces solve that by making the
// work visible in the assistant turn: Anthropic returns `server_tool_use` blocks
// paired with a result block, OpenAI Responses returns server-side call items.
// The caller then replays the whole turn, so nothing about the round has to live
// in a daemon-side record.
//
// This file renders those artifacts and folds the caller's replay back into the
// ordinary tool call/result pair the upstream dialect understands. Chat and
// Gemini have no vocabulary for a server-executed call (a caller that received
// one would try to run it), so those surfaces keep the hidden-and-deferred path.
//
// Verified against Claude Code 2.1.278 (A0 spike): a `server_tool_use` block
// carrying `input_json_delta` frames plus an `mcp_tool_result` block is accepted,
// carried verbatim in the next request's assistant turn, and neither executed nor
// answered by the client. `mcp_tool_use` fails that client's stream accumulator
// ("Content block is not a input_json block": it accepts input deltas only for
// tool_use and server_tool_use), which is why the call block is server_tool_use
// while the result block is the generic mcp_tool_result.
const (
	artifactClaudeCall     = "server_tool_use"
	artifactClaudeResult   = "mcp_tool_result"
	artifactClaudeIDPrefix = "srvtoolu_"
	// Responses reports a server-executed call as an mcp_call item: the shape
	// OpenAI's own MCP tools use, and the item family that tells a caller "the
	// server ran this" rather than "you run this".
	artifactResponsesCall = "mcp_call"
)

// artifactServerLabel names the daemon as the executor of an mcp_call item.
const artifactServerLabel = "llama-swap"

// visibleArtifact is one artifact item a streamed Responses round reported, kept
// with the call it stands for so the final completed frame can replace that call
// instead of listing it twice.
type visibleArtifact struct {
	SourceCallID string
	Item         map[string]any
}

// serverToolArtifactsVisible reports whether a surface renders server-side calls
// to the caller instead of hiding them. Chat Completions cannot express one (a
// tool_calls entry is by definition the caller's to run), and Gemini's custom
// function parts carry the same problem.
func serverToolArtifactsVisible(endpoint string) bool {
	return endpoint == "anthropic.messages" || endpoint == "responses"
}

// visibleTurnBlocks renders one round's assistant turn for the caller: text and
// caller-owned calls as they are, server calls as a call artifact paired with
// its result, in the model's own call order.
func visibleTurnBlocks(response map[string]any, endpoint string, outputs map[string]map[string]any, serverCalls []extensionCall) []any {
	if endpoint == "responses" {
		return responsesVisibleTurn(response, outputs, serverCalls)
	}
	return claudeVisibleTurn(response, outputs, serverCalls)
}

// setVisibleTurn writes the accumulated rounds plus this round into the answer
// the caller receives.
func setVisibleTurn(response map[string]any, endpoint string, accumulated, round []any) {
	merged := append(append(make([]any, 0, len(accumulated)+len(round)), accumulated...), round...)
	if endpoint == "responses" {
		response["output"] = merged
		return
	}
	response["content"] = merged
	response["stop_reason"] = "tool_use"
}

// claudeServerCall renders one executed server call as a server_tool_use block.
func claudeServerCall(call extensionCall) map[string]any {
	var input any
	if err := json.Unmarshal([]byte(call.Arguments), &input); err != nil {
		input = map[string]any{}
	}
	id := call.ID
	if !strings.HasPrefix(id, artifactClaudeIDPrefix) {
		id = artifactClaudeIDPrefix + strings.TrimPrefix(id, "toolu_")
	}
	return map[string]any{"type": artifactClaudeCall, "id": id, "name": call.Name, "input": input}
}

// claudeServerResult renders the tool result of an executed server call as an
// mcp_tool_result block: content is a list of content blocks, which is the shape
// both the daemon's own decode and Claude Code carry.
func claudeServerResult(call extensionCall, output map[string]any) map[string]any {
	content := output["content"]
	var blocks []any
	switch value := content.(type) {
	case nil:
	case string:
		blocks = append(blocks, map[string]any{"type": "text", "text": value})
	case []any:
		blocks = append(blocks, value...)
	default:
		blocks = append(blocks, map[string]any{"type": "text", "text": toolResultText(value)})
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}
	result := map[string]any{"type": artifactClaudeResult, "tool_use_id": claudeArtifactID(call), "content": blocks}
	if failed, _ := output["is_error"].(bool); failed {
		result["is_error"] = true
	}
	return result
}

func claudeArtifactID(call extensionCall) string {
	if strings.HasPrefix(call.ID, artifactClaudeIDPrefix) {
		return call.ID
	}
	return artifactClaudeIDPrefix + strings.TrimPrefix(call.ID, "toolu_")
}

// claudeVisibleTurn renders one round's assistant content for the caller: text
// and caller-owned calls as they are, server calls as a server_tool_use block
// followed by its result block, in the model's own call order.
func claudeVisibleTurn(response map[string]any, serverResults map[string]map[string]any, serverCalls []extensionCall) []any {
	blocks, _ := response["content"].([]any)
	byID := map[string]extensionCall{}
	for _, call := range serverCalls {
		byID[call.ID] = call
	}
	visible := make([]any, 0, len(blocks)+len(serverCalls)*2)
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok || block["type"] != "tool_use" {
			visible = append(visible, raw)
			continue
		}
		id, _ := block["id"].(string)
		call, isServer := byID[id]
		if !isServer {
			visible = append(visible, raw)
			continue
		}
		visible = append(visible, claudeServerCall(call))
		if output, ok := serverResults[id]; ok {
			visible = append(visible, claudeServerResult(call, output))
		}
	}
	return visible
}

// persistVisibleStream stores the completed Responses turn of a streamed call:
// the writer's completed frame is the only place the whole visible turn (its
// artifact items included) exists at once.
func (s *Server) persistVisibleStream(ctx context.Context, state *extensionRequestState, stream *extensionStreamWriter) {
	if s == nil || s.store == nil || stream == nil || state == nil || state.meta.Endpoint != "responses" || stream.completed == nil {
		return
	}
	response, ok := stream.completed["response"].(map[string]any)
	if !ok {
		return
	}
	if encoded, err := json.Marshal(response); err == nil {
		s.persistVisibleResponse(ctx, state, encoded)
	}
}

// persistVisibleResponse re-persists a Responses answer after the extensions
// rewrote it. The protocol middleware (and the native affinity middleware) store
// the response of the round they handled, which for an extension round is the
// pre-extension body: a previous_response_id continuation then replays a
// conversation without the artifacts and without the server tool's result. The
// visible body is what a caller actually holds, so that is what is stored.
func (s *Server) persistVisibleResponse(ctx context.Context, state *extensionRequestState, body []byte) {
	if s == nil || s.store == nil || state == nil || state.meta.Endpoint != "responses" || len(body) == 0 {
		return
	}
	if state.suppressAffinity {
		// The caller asked for no retention (or an operator hook did). A request
		// hook may have rewritten store:true for the rest of the pipeline, which
		// cannot see the caller's own flag and has already written its row; the
		// visible row is removed so the caller's request wins.
		var envelope struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(body, &envelope) == nil && envelope.ID != "" {
			writeCtx, cancel := affinityWriteContext()
			defer cancel()
			if err := s.store.DeleteResponseAffinity(writeCtx, envelope.ID); err != nil {
				// The row was written by the inner persistence because a hook rewrote
				// store:true; if the removal fails the caller's no-retention request is
				// not honored, which an operator has to be able to see.
				slog.Warn("could not remove the affinity row of a response the caller asked not to retain",
					"response_id", envelope.ID, "model", state.meta.ResolvedModel, "request_id", state.meta.RequestID, "error", err)
			}
		}
		return
	}
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	persistNativeResponseAffinity(ctx, s.store, state.meta.ResolvedModel, state.meta.Provider, body, headers, http.StatusOK)
}

// applyVisibleTurn prepends the server-tool work of earlier rounds to the answer
// the caller receives, so the caller's transcript carries the whole turn and
// replays it on the next request.
func applyVisibleTurn(response map[string]any, state *extensionRequestState, endpoint string) {
	if state == nil || !serverToolArtifactsVisible(endpoint) || len(state.visibleTurn) == 0 {
		return
	}
	key := "content"
	if endpoint == "responses" {
		key = "output"
	}
	current, _ := response[key].([]any)
	response[key] = append(append(make([]any, 0, len(state.visibleTurn)+len(current)), state.visibleTurn...), current...)
}

// responsesVisibleTurn renders a round as Responses output items: caller-owned
// calls stay function_call items, server calls become mcp_call items carrying
// their output (the item shape OpenAI uses for server-executed tools).
func responsesVisibleTurn(response map[string]any, outputs map[string]map[string]any, serverCalls []extensionCall) []any {
	items, _ := response["output"].([]any)
	byID := map[string]extensionCall{}
	for _, call := range serverCalls {
		byID[call.ID] = call
	}
	visible := make([]any, 0, len(items)+len(serverCalls))
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok || item["type"] != "function_call" {
			visible = append(visible, raw)
			continue
		}
		id, _ := item["call_id"].(string)
		if id == "" {
			id, _ = item["id"].(string)
		}
		call, isServer := byID[id]
		if !isServer {
			visible = append(visible, raw)
			continue
		}
		visible = append(visible, responsesCallItem(call, outputs[call.ID]))
	}
	return visible
}

// responsesCallItem renders one executed server call as an mcp_call item.
func responsesCallItem(call extensionCall, output map[string]any) map[string]any {
	id := call.ID
	if !strings.HasPrefix(id, "mcp_") {
		id = "mcp_" + strings.TrimPrefix(id, "call_")
	}
	item := map[string]any{
		"id": id, "type": artifactResponsesCall, "server_label": artifactServerLabel,
		"name": call.Name, "arguments": call.Arguments, "status": "completed",
	}
	if output == nil {
		item["output"] = ""
		return item
	}
	if text, ok := output["content"].(string); ok {
		item["output"] = text
	} else {
		item["output"] = toolResultText(output["content"])
	}
	if failed, _ := output["is_error"].(bool); failed {
		item["error"] = item["output"]
	}
	return item
}

// downgradeServerArtifacts folds the artifacts a caller replayed back into the
// ordinary call/result pair the rest of the pipeline understands, per surface.
func downgradeServerArtifacts(request map[string]any, endpoint string) bool {
	if endpoint == "responses" {
		return responsesFoldServerArtifacts(request)
	}
	if endpoint == "anthropic.messages" {
		return downgradeClaudeServerArtifacts(request)
	}
	return false
}

// responsesFoldServerArtifacts rewrites a replayed mcp_call item into the
// function_call + function_call_output pair every dialect below understands. The
// Responses input list is flat, so the two items simply replace the artifact in
// place; the caller's own outputs stay where they are.
func responsesFoldServerArtifacts(request map[string]any) bool {
	items, _ := request["input"].([]any)
	if len(items) == 0 {
		return false
	}
	folded := make([]any, 0, len(items)+2)
	changed := false
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok || item["type"] != artifactResponsesCall {
			folded = append(folded, raw)
			continue
		}
		id, _ := item["id"].(string)
		name, _ := item["name"].(string)
		arguments, _ := item["arguments"].(string)
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		folded = append(folded, map[string]any{
			"type": "function_call", "id": id, "call_id": id, "name": name,
			"arguments": arguments, "status": "completed",
		})
		output := item["output"]
		if output == nil {
			output = item["error"]
		}
		if output == nil {
			output = ""
		}
		folded = append(folded, map[string]any{"type": "function_call_output", "call_id": id, "output": toolResultText(output)})
		changed = true
	}
	if !changed {
		return false
	}
	request["input"] = folded
	return true
}

// downgradeClaudeServerArtifacts folds the artifacts a caller replayed back into
// the ordinary tool call/result pair the rest of the pipeline understands.
//
// The round-trip has to survive a dialect change: the caller speaks Anthropic
// (server_tool_use + mcp_tool_result inside the assistant turn) while the
// upstream is usually a Chat-dialect engine, whose encoder keeps tool results
// only in user/tool turns. Leaving the result block in the assistant turn would
// silently drop it (encodeChatAssistantMessage has no case for a tool result), so
// the result moves next to the caller's own results, in the order the turn lists
// its calls.
func downgradeClaudeServerArtifacts(request map[string]any) bool {
	messages, _ := request["messages"].([]any)
	if len(messages) == 0 {
		return false
	}
	changed := false
	// Built fresh: inserting into the slice being walked shifted the range's view
	// and could skip the conversation's last message.
	folded := make([]any, 0, len(messages)+2)
	for index := 0; index < len(messages); index++ {
		raw := messages[index]
		message, ok := raw.(map[string]any)
		if !ok {
			folded = append(folded, raw)
			continue
		}
		blocks, ok := message["content"].([]any)
		if !ok {
			folded = append(folded, raw)
			continue
		}
		role, _ := message["role"].(string)
		kept := make([]any, 0, len(blocks))
		type movedResult struct {
			id    string
			block map[string]any
		}
		var moved []movedResult
		order := make([]string, 0, len(blocks)) // the turn's tool_use ids, in order
		callNames := map[string]string{}
		for _, rawBlock := range blocks {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				kept = append(kept, rawBlock)
				continue
			}
			switch block["type"] {
			case artifactClaudeCall:
				call := map[string]any{"type": "tool_use", "id": block["id"], "name": block["name"], "input": block["input"]}
				kept = append(kept, call)
				if id, _ := block["id"].(string); id != "" {
					order = append(order, id)
					if name, _ := block["name"].(string); name != "" {
						callNames[id] = name
					}
				}
				changed = true
			case "tool_use":
				if id, _ := block["id"].(string); id != "" {
					order = append(order, id)
				}
				kept = append(kept, rawBlock)
			case artifactClaudeResult:
				id, _ := block["tool_use_id"].(string)
				moved = append(moved, movedResult{id: id, block: map[string]any{
					"type": "tool_result", "tool_use_id": id,
					"content": block["content"], "is_error": block["is_error"],
				}})
				changed = true
			default:
				kept = append(kept, rawBlock)
			}
		}
		if len(moved) == 0 {
			folded = append(folded, raw)
			continue
		}
		message["content"] = kept
		ours := map[string]map[string]any{}
		for _, entry := range moved {
			ours[entry.id] = entry.block
		}
		// The caller's own results live in a following user turn; the folded turn
		// carries the caller's call ids, so its results can be ordered too.
		nextIndex := index + 1
		var next map[string]any
		if role == "assistant" && nextIndex < len(messages) {
			if candidate, ok := messages[nextIndex].(map[string]any); ok && candidate["role"] == "user" && hasClaudeToolResults(candidate) {
				next = candidate
			}
		}
		callerResults := map[string]map[string]any{}
		// The caller's own order is what an unmatched result keeps, so a result whose
		// call the assistant turn no longer lists (a stripped call) is not reordered
		// by map iteration.
		var leftovers []any
		callerOrder := make([]string, 0, 4)
		if next != nil {
			existing, _ := next["content"].([]any)
			keptNext := make([]any, 0, len(existing))
			for _, rawBlock := range existing {
				block, _ := rawBlock.(map[string]any)
				if block == nil || block["type"] != "tool_result" {
					keptNext = append(keptNext, rawBlock)
					continue
				}
				id, _ := block["tool_use_id"].(string)
				if _, ok := callerResults[id]; ok {
					leftovers = append(leftovers, rawBlock)
					continue
				}
				callerResults[id] = block
				callerOrder = append(callerOrder, id)
			}
			ordered := make([]any, 0, len(order)+len(leftovers))
			for _, id := range order {
				if block, ok := ours[id]; ok {
					ordered = append(ordered, block)
					continue
				}
				if block, ok := callerResults[id]; ok {
					ordered = append(ordered, block)
					delete(callerResults, id)
				}
			}
			for _, id := range callerOrder {
				if block, ok := callerResults[id]; ok {
					ordered = append(ordered, block)
					delete(callerResults, id)
				}
			}
			ordered = append(ordered, leftovers...)
			next["content"] = append(keptNext, ordered...)
			folded = append(folded, raw, next)
			index = nextIndex
			continue
		}
		// The caller sent no result turn (a single-turn replay): ours go right after
		// the assistant turn, in call order.
		ordered := make([]any, 0, len(order)+len(moved))
		for _, id := range order {
			if block, ok := ours[id]; ok {
				ordered = append(ordered, block)
				delete(ours, id)
			}
		}
		for _, entry := range moved {
			if block, ok := ours[entry.id]; ok {
				ordered = append(ordered, block)
			}
		}
		folded = append(folded, raw, map[string]any{"role": "user", "content": ordered})
	}
	request["messages"] = folded
	return changed
}

func hasClaudeToolResults(message map[string]any) bool {
	blocks, _ := message["content"].([]any)
	for _, raw := range blocks {
		if block, ok := raw.(map[string]any); ok && block["type"] == "tool_result" {
			return true
		}
	}
	return false
}

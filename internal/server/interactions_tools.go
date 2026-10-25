package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/protocol/relaykit"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

type interactionsToolClientKey struct{}

func createInteractionsToolsMiddleware() chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			route, ok := relaykit.RouteForRequest(r.Method, r.URL.Path)
			if !ok || route.Format != relaykit.GeminiInteractions {
				next.ServeHTTP(w, r)
				return
			}
			body, err := readResponsesRequestBody(r)
			if err != nil {
				swaputil.SendResponse(w, r, 413, err.Error())
				return
			}
			var input map[string]any
			if err := json.Unmarshal(body, &input); err != nil {
				swaputil.SendResponse(w, r, 400, err.Error())
				return
			}
			if input["previous_interaction_id"] != nil {
				swaputil.SendResponse(w, r, 400, "MCP tool execution with previous_interaction_id requires a gateway conversation")
				return
			}
			data, ok := swaputil.ReadContext(r.Context())
			if !ok {
				swaputil.SendResponse(w, r, 500, "missing model context")
				return
			}
			meta := relaykit.NewMeta(data.ModelID, data.Streaming)
			converted, err := relaykit.ConvertRequest(r.Context(), meta, route.Format, relaykit.OpenAIResponses, body, data.ModelID)
			if err != nil {
				swaputil.SendResponse(w, r, 400, err.Error())
				return
			}
			encoded, err := relaykit.Marshal(converted.Value)
			if err != nil {
				swaputil.SendResponse(w, r, 400, err.Error())
				return
			}
			copy := r.Clone(context.WithValue(r.Context(), interactionsToolClientKey{}, true))
			copy.URL.Path = "/v1/responses"
			if err := setProtocolRequestBody(copy, encoded); err != nil {
				swaputil.SendResponse(w, r, 400, err.Error())
				return
			}
			writer, err := newProtocolResponseWriter(w, r, nil, data.ModelID, "", relaykit.OpenAIResponses, route.Format, body, meta, data.Streaming)
			if err != nil {
				swaputil.SendResponse(w, r, 400, err.Error())
				return
			}
			next.ServeHTTP(writer, copy)
			if err := writer.finish(); err != nil {
				if writer.streamWriter != nil {
					writer.streamWriter.emitTerminalStreamError(err)
				} else if !writer.committed() {
					swaputil.SendResponse(w, r, 502, err.Error())
				}
			}
		})
	}
}

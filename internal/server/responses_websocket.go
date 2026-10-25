package server

import (
	"net/http"

	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// handleResponsesCollectionGet answers GET on the Responses collection path.
// The API only creates responses with POST, so a plain GET is a 405.
//
// The exception is a WebSocket handshake: Codex enables its Responses
// WebSocket transport whenever its provider declares supports_websockets, and
// it only falls back to the HTTP transport on 426 Upgrade Required. Answering
// 405 there leaves the client retrying the handshake until its retry budget is
// gone, so the turn fails even though the HTTP endpoint works.
func handleResponsesCollectionGet(w http.ResponseWriter, r *http.Request) {
	if swaputil.IsWebSocketUpgrade(r) {
		w.Header().Set("Upgrade", "websocket")
		w.Header().Set("Connection", "Upgrade")
		http.Error(w, "this server does not expose the Responses WebSocket transport; retry over HTTP", http.StatusUpgradeRequired)
		return
	}
	w.Header().Set("Allow", http.MethodPost)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

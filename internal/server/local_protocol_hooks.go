package server

import (
	"bytes"
	"io"
	"net/http"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/conversation"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

type nativeCaptureWriter struct {
	http.ResponseWriter
	body      bytes.Buffer
	status    int
	truncated bool
}

func (w *nativeCaptureWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *nativeCaptureWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	remaining := (16 << 20) - w.body.Len()
	if len(data) > remaining {
		w.truncated = true
		dataCopy := data[:remaining]
		_, _ = w.body.Write(dataCopy)
	} else {
		_, _ = w.body.Write(data)
	}
	return w.ResponseWriter.Write(data)
}
func (w *nativeCaptureWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) serveLocalWithProtocolHooks(w http.ResponseWriter, r *http.Request, model string, cfg config.Config, serve http.HandlerFunc) {
	hooks := swaputil.UpstreamHooksFrom(r.Context())
	if hooks.Prepare == nil && hooks.Complete == nil {
		serve(w, r)
		return
	}
	copy := r.Clone(r.Context())
	format := protocolTargetFormat(copy)
	if format == "" {
		route, ok := dialect.RouteForRequest(copy.Method, copy.URL.Path)
		if ok {
			format = route.Format
		} else {
			format = dialect.OpenAIChat
		}
	}
	remote := cfg.Models[model].UseModelName
	if remote == "" {
		remote = model
	}
	scope := conversation.Scope{Provider: "local", Adapter: cfg.Models[model].Backend.Type, Model: remote, Format: format}
	if hooks.Prepare != nil {
		if err := hooks.Prepare(copy.Context(), scope, copy); err != nil {
			swaputil.SendResponse(w, r, 400, err.Error())
			return
		}
	}
	var input []byte
	if copy.GetBody != nil {
		body, err := copy.GetBody()
		if err == nil {
			input, _ = io.ReadAll(io.LimitReader(body, (16<<20)+1))
			body.Close()
		}
	}
	capture := &nativeCaptureWriter{ResponseWriter: w}
	serve(capture, copy)
	if hooks.Complete != nil {
		status := capture.status
		if status == 0 {
			status = 200
		}
		hooks.Complete(copy.Context(), scope, input, capture.body.Bytes(), capture.Header().Clone(), status, capture.truncated)
	}
}

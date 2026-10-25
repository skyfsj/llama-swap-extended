package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

type conversationWriter struct {
	parent    http.ResponseWriter
	stream    bool
	format    dialect.Format
	status    int
	committed bool
	body      bytes.Buffer
	pending   bytes.Buffer
	held      bytes.Buffer
	terminal  bool
	failed    bool
	truncated bool
}

func newConversationWriter(w http.ResponseWriter, stream bool, format dialect.Format) *conversationWriter {
	return &conversationWriter{parent: w, stream: stream, format: format}
}
func (w *conversationWriter) Header() http.Header { return w.parent.Header() }
func (w *conversationWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *conversationWriter) Flush() {
	if w.committed {
		if f, ok := w.parent.(http.Flusher); ok {
			f.Flush()
		}
	}
}
func (w *conversationWriter) send(data []byte) error {
	if !w.committed {
		if w.stream {
			w.Header().Del("Content-Length")
		}
		w.parent.WriteHeader(w.status)
		w.committed = true
	}
	_, err := w.parent.Write(data)
	w.Flush()
	return err
}

func (w *conversationWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n := len(data)
	remaining := (16 << 20) - w.body.Len()
	if remaining < len(data) {
		w.truncated = true
		if remaining > 0 {
			_, _ = w.body.Write(data[:remaining])
		}
	} else {
		_, _ = w.body.Write(data)
	}
	if w.truncated {
		return n, nil
	}
	if encoding := w.Header().Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return n, nil
	}
	if !w.stream || w.status >= 400 {
		return n, nil
	}
	if w.terminal {
		_, _ = w.held.Write(data)
		return n, nil
	}
	_, _ = w.pending.Write(data)
	for {
		event, ok := takeSSEEvent(&w.pending)
		if !ok {
			break
		}
		wire := append(append([]byte(nil), event...), []byte("\n\n")...)
		payload, hasData := sseData(event)
		if !hasData {
			if err := w.send(wire); err != nil {
				return 0, err
			}
			continue
		}
		terminal, failed := conversationTerminal(w.format, []byte(payload))
		w.failed = w.failed || failed
		if terminal {
			w.terminal = true
			_, _ = w.held.Write(wire)
			_, _ = w.held.Write(w.pending.Bytes())
			w.pending.Reset()
			break
		}
		if err := w.send(wire); err != nil {
			return 0, err
		}
	}
	return n, nil
}

func conversationTerminal(format dialect.Format, data []byte) (bool, bool) {
	if string(data) == "[DONE]" {
		return true, false
	}
	var obj map[string]any
	if json.Unmarshal(data, &obj) != nil {
		return false, false
	}
	if obj["error"] != nil {
		return true, true
	}
	switch format {
	case dialect.OpenAIResponses:
		typ, _ := obj["type"].(string)
		return typ == "response.completed" || typ == "response.incomplete" || typ == "response.failed" || typ == "error", typ == "response.failed" || typ == "error"
	case dialect.ClaudeMessages:
		typ, _ := obj["type"].(string)
		if typ == "message_stop" {
			return true, false
		}
		if typ == "message_delta" {
			if delta, ok := obj["delta"].(map[string]any); ok && delta["stop_reason"] != nil {
				return true, false
			}
		}
	case dialect.OpenAIChat:
		if choices, ok := obj["choices"].([]any); ok {
			for _, raw := range choices {
				choice, _ := raw.(map[string]any)
				if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
					return true, false
				}
			}
		}
	case dialect.Gemini:
		if candidates, ok := obj["candidates"].([]any); ok {
			for _, raw := range candidates {
				candidate, _ := raw.(map[string]any)
				if reason, ok := candidate["finishReason"].(string); ok && reason != "" {
					return true, false
				}
			}
		}
	case dialect.GeminiInteractions:
		typ, _ := obj["event_type"].(string)
		if typ == "interaction.completed" {
			response, _ := obj["interaction"].(map[string]any)
			return true, response["status"] == "failed" || response["status"] == "cancelled"
		}
	}
	return false, false
}

func (w *conversationWriter) finish(commit func() error) error {
	if w.status == 0 {
		w.status = 200
	}
	if encoding := w.Header().Get("Content-Encoding"); w.status < 400 && encoding != "" && encoding != "identity" {
		decoded, err := decompressBody(w.body.Bytes(), encoding)
		if err != nil {
			return w.fail(err)
		}
		w.Header().Del("Content-Encoding")
		w.Header().Del("Content-Length")
		w.body.Reset()
		if _, err := w.Write(decoded); err != nil {
			return w.fail(err)
		}
	}
	if w.status >= 400 {
		w.Header().Set("X-Llama-Swap-Conversation-Status", "failed")
		return w.send(w.body.Bytes())
	}
	if w.failed {
		w.Header().Set("X-Llama-Swap-Conversation-Status", "failed")
		if w.stream {
			return w.send(append(w.held.Bytes(), w.pending.Bytes()...))
		}
		return w.send(w.body.Bytes())
	}
	if w.truncated {
		return w.fail(errors.New("conversation response exceeded the checkpoint limit"))
	}
	if err := commit(); err != nil {
		return w.fail(err)
	}
	w.Header().Set("X-Llama-Swap-Conversation-Status", "completed")
	if !w.stream {
		return w.send(w.body.Bytes())
	}
	if json.Valid(bytes.TrimSpace(w.body.Bytes())) {
		frames, _, err := dialect.ConvertResponseToStream(w.format, w.format, w.body.Bytes(), dialect.StreamOptions{IncludeUsage: true})
		if err != nil {
			return w.fail(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range frames {
			var wire []byte
			if frame.Event != "" {
				wire = append(wire, []byte("event: "+frame.Event+"\n")...)
			}
			wire = append(wire, []byte("data: "+string(frame.Data)+"\n\n")...)
			if err := w.send(wire); err != nil {
				return err
			}
		}
		return nil
	}
	return w.send(append(w.held.Bytes(), w.pending.Bytes()...))
}

func (w *conversationWriter) fail(err error) error {
	w.Header().Set("X-Llama-Swap-Conversation-Status", "failed")
	if w.stream {
		w.Header().Set("Content-Type", "text/event-stream")
		if !w.committed {
			w.parent.WriteHeader(w.status)
			w.committed = true
		}
		writer := &protocolStreamWriter{writer: w.parent, client: w.format}
		writer.emitTerminalStreamError(err)
		return err
	}
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "application/json")
	w.status = http.StatusInsufficientStorage
	body := []byte(`{"error":{"type":"conversation_commit_failed","message":"conversation could not be saved"}}`)
	_ = w.send(body)
	return err
}

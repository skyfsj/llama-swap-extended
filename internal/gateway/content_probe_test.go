package gateway

import (
	"testing"
)

func TestGateway_ContentProbeClassifiesDialects(t *testing.T) {
	tests := []struct {
		name    string
		dialect string
		sse     bool
		chunks  []string
		want    probeVerdict
	}{
		{
			name:    "chat stream: lifecycle then content",
			dialect: "openai", sse: true,
			chunks: []string{
				"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"index\":0,\"finish_reason\":null}]}\n\n",
				"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"index\":0}]}\n\n",
			},
			want: probeContent,
		},
		{
			name:    "chat stream: lifecycle only is empty",
			dialect: "openai", sse: true,
			chunks: []string{
				"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"index\":0}]}\n\n",
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}]}\n\ndata: [DONE]\n\n",
			},
			want: probeEmpty,
		},
		{
			name:    "chat: tool call counts as output",
			dialect: "openai", sse: true,
			chunks: []string{
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"f\"}}]},\"index\":0}]}\n\n",
			},
			want: probeContent,
		},
		{
			name:    "responses stream: reasoning delta counts",
			dialect: "openai-responses", sse: true,
			chunks: []string{
				"data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n",
				"data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"think\"}\n\n",
			},
			want: probeContent,
		},
		{
			name:    "responses stream: lifecycle only is empty",
			dialect: "openai-responses", sse: true,
			chunks: []string{
				"data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n",
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\",\"content\":[]}}\n\n",
				"data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n",
			},
			want: probeEmpty,
		},
		{
			name:    "anthropic stream: text delta counts",
			dialect: "anthropic", sse: true,
			chunks: []string{
				"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\n",
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
			},
			want: probeContent,
		},
		{
			name:    "anthropic stream: message_stop without content is empty",
			dialect: "anthropic", sse: true,
			chunks: []string{
				"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\n",
				"data: {\"type\":\"message_stop\"}\n\n",
			},
			want: probeEmpty,
		},
		{
			name:    "gemini stream: functionCall counts",
			dialect: "gemini", sse: true,
			chunks: []string{
				"data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"f\"}}]}}]}\n\n",
			},
			want: probeContent,
		},
		{
			name:    "gemini stream: finishReason only is empty",
			dialect: "gemini", sse: true,
			chunks: []string{
				"data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"STOP\"}]}\n\n",
			},
			want: probeEmpty,
		},
		{
			name:    "chat whole body with content",
			dialect: "openai", sse: false,
			chunks: []string{`{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`},
			want:   probeContent,
		},
		{
			name:    "chat whole body with empty content",
			dialect: "openai", sse: false,
			chunks: []string{`{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"total_tokens":5}}`},
			want:   probeEmpty,
		},
		{
			name:    "unknown shape fails open",
			dialect: "openai", sse: false,
			chunks: []string{`{"unexpected":"shape"}`},
			want:   probePending,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			probe := newContentProbe(tc.dialect, tc.sse)
			for _, chunk := range tc.chunks {
				probe.Write([]byte(chunk))
			}
			probe.Finish()
			if got := probe.Verdict(); got != tc.want {
				t.Fatalf("verdict = %v, want %v (hasContent=%v terminal=%v)", got, tc.want, probe.hasContent, probe.terminal)
			}
		})
	}
}

func TestGateway_ContentProbePendingDoesNotRetry(t *testing.T) {
	probe := newContentProbe("openai", true)
	probe.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"index\":0}]}\n\n"))
	if got := probe.Verdict(); got != probePending {
		t.Fatalf("an unterminated stream must stay pending, got %v", got)
	}
}

func TestGateway_ContentProbeIgnoresUnrecognizedBody(t *testing.T) {
	probe := newContentProbe("openai", false)
	probe.Write([]byte(`{"id":"img","data":[{"b64_json":"AAAA"}]}`))
	probe.Finish()
	if got := probe.Verdict(); got != probePending {
		t.Fatalf("verdict = %v, want pending so an unknown shape is forwarded, not retried", got)
	}
}

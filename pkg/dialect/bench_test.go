package dialect

import (
	"testing"
)

func chatRequestFixture() []byte {
	return []byte(`{"model":"m","messages":[
		{"role":"system","content":"be concise"},
		{"role":"user","content":"hello there"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"found"},
		{"role":"user","content":"summarize in one line"}
	],"tools":[{"type":"function","function":{"name":"lookup","description":"look up","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}],"max_tokens":256,"temperature":0.2}`)
}

func BenchmarkConvertRequest_ChatToClaude(b *testing.B) {
	body := chatRequestFixture()
	options := ConvertOptions{Model: "m"}
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		if _, _, err := ConvertRequest(OpenAIChat, ClaudeMessages, body, options); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkConvertRequest_ChatToGemini(b *testing.B) {
	body := chatRequestFixture()
	options := ConvertOptions{Model: "m"}
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		if _, _, err := ConvertRequest(OpenAIChat, Gemini, body, options); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStreamChunk_ChatToClaude(b *testing.B) {
	frame := []byte(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"token"}}]}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(frame)))
	for i := 0; i < b.N; i++ {
		converter, err := NewStreamConverter(OpenAIChat, ClaudeMessages, StreamOptions{Model: "m"})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := converter.Feed(frame); err != nil {
			b.Fatal(err)
		}
	}
}

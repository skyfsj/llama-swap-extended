package auditstore

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func chatBody(messages ...string) []byte {
	return []byte(`{"model":"m","temperature":0,"messages":[` + strings.Join(messages, ",") + `],"stream":true}`)
}

func TestPlanBody_RoundTripsChatMessages(t *testing.T) {
	body := chatBody(
		`{"role":"system","content":"`+strings.Repeat("You are helpful. ", 40)+`"}`,
		`{"role":"user","content":"`+strings.Repeat("explain the storage design ", 30)+`"}`,
		`{"role":"assistant","content":"sure"}`,
	)
	plan := planBody(body)
	if plan.kind != kindChat {
		t.Fatalf("kind = %q, want %q", plan.kind, kindChat)
	}
	if len(plan.blocks) != 3 {
		t.Fatalf("blocks = %d, want 3", len(plan.blocks))
	}
	rebuilt, err := reassemble(plan.template, plan.blocks)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rebuilt, body) {
		t.Fatalf("reassembled body differs:\n got %s\nwant %s", rebuilt, body)
	}
}

// The point of message-level chunking: the next turn of a session re-sends the
// whole history, and every already-seen message keeps its block identity.
func TestPlanBody_NextTurnSharesHistoryBlocks(t *testing.T) {
	first := chatBody(
		`{"role":"system","content":"`+strings.Repeat("system prompt ", 60)+`"}`,
		`{"role":"user","content":"turn one"}`,
	)
	second := chatBody(
		`{"role":"system","content":"`+strings.Repeat("system prompt ", 60)+`"}`,
		`{"role":"user","content":"turn one"}`,
		`{"role":"assistant","content":"answer one"}`,
		`{"role":"user","content":"turn two"}`,
	)
	firstPlan := planBody(first)
	secondPlan := planBody(second)
	if len(firstPlan.blocks) != 2 || len(secondPlan.blocks) != 4 {
		t.Fatalf("block counts = %d/%d, want 2/4", len(firstPlan.blocks), len(secondPlan.blocks))
	}
	for index := 0; index < len(firstPlan.blocks); index++ {
		if hashBlock(firstPlan.blocks[index]) != hashBlock(secondPlan.blocks[index]) {
			t.Fatalf("history block %d changed identity across turns", index)
		}
	}
}

func TestPlanBody_FallsBackToRawForNonChatShapes(t *testing.T) {
	cases := map[string][]byte{
		"embeddings": []byte(`{"input":["` + strings.Repeat("x", 2000) + `"],"model":"e"}` + "\n" + `{"pad":"` + strings.Repeat("y", 1200) + `"}`),
		"sse":        []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" + strings.Repeat("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n", 40)),
		"garbage":    []byte("not json at all " + strings.Repeat("z", 4000)),
		"tiny":       []byte(`{"model":"m"}`),
	}
	for name, body := range cases {
		plan := planBody(body)
		if plan.kind != kindRaw {
			t.Errorf("%s: kind = %q, want raw", name, plan.kind)
		}
		rebuilt, err := reassemble(plan.template, plan.blocks)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !bytes.Equal(rebuilt, body) {
			t.Errorf("%s: raw fallback did not round-trip", name)
		}
	}
}

func TestReassemble_RejectsTemplateWithoutPlaceholder(t *testing.T) {
	if _, err := reassemble([]byte(`{"a":1}`), [][]byte{[]byte(`{"b":2}`)}); err == nil {
		t.Fatal("expected a missing-placeholder error")
	}
}

func TestPlanBody_InputArrayShapeIsChunked(t *testing.T) {
	body := []byte(`{"model":"gpt","input":[` +
		`{"role":"user","content":"` + strings.Repeat("hello ", 200) + `"},` +
		`{"role":"assistant","content":"` + strings.Repeat("world ", 200) + `"}],` +
		`"stream":true}`)
	plan := planBody(body)
	if plan.kind != kindChat || len(plan.blocks) != 2 {
		t.Fatalf("plan = %s/%d blocks, want chat/2", plan.kind, len(plan.blocks))
	}
	rebuilt, err := reassemble(plan.template, plan.blocks)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rebuilt, body) {
		t.Fatal("input-array body did not round-trip")
	}
}

func TestPlanBody_LargeHistoryKeepsManyBlocks(t *testing.T) {
	messages := make([]string, 0, 60)
	for index := 0; index < 60; index++ {
		messages = append(messages, fmt.Sprintf(`{"role":"user","content":"%s"}`, strings.Repeat(fmt.Sprintf("turn %d ", index), 60)))
	}
	body := chatBody(messages...)
	plan := planBody(body)
	if plan.kind != kindChat || len(plan.blocks) != 60 {
		t.Fatalf("blocks = %d, want 60", len(plan.blocks))
	}
	rebuilt, err := reassemble(plan.template, plan.blocks)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rebuilt, body) {
		t.Fatal("long history did not round-trip")
	}
	if len(plan.template) > len(body)/4 {
		t.Fatalf("template is %d bytes of a %d byte body; expected the messages to carry the bulk", len(plan.template), len(body))
	}
}

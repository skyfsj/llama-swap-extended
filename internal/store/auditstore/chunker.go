package auditstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/tidwall/gjson"
)

// bodyKind labels how a stored body was split.
const (
	kindChat = "chat"
	kindRaw  = "raw"
)

// minChunkedBodySize skips the descriptor/template overhead for trivial
// payloads (a one-line health probe). Conversation bodies are chunked from the
// first turn on: the dedup identity of a message must not depend on which turn
// happened to cross a size threshold.
const minChunkedBodySize = 256

// messageFields lists the JSON paths whose arrays carry conversation turns,
// across the OpenAI chat, Responses and Anthropic message shapes. Each element
// of the array is content-addressed independently, so the accumulated context
// a session re-sends every turn is stored once.
var messageFields = []string{"messages", "input"}

// descriptor records how one body decomposes into stored blocks. It is what a
// conversation row's body reference resolves to.
type descriptor struct {
	Version  int      `json:"v"`
	Kind     string   `json:"k"`
	Size     int      `json:"s"`
	Template string   `json:"t,omitempty"`
	Blocks   []string `json:"b,omitempty"`
}

// chunker turns a body into blocks. Implementations of the invariant: the
// concatenation of template (with placeholders substituted) and blocks
// reproduces the original bytes exactly.
type bodyPlan struct {
	kind     string
	size     int
	template []byte
	blocks   [][]byte
}

// planBody splits a body at message boundaries when it carries a recognizable
// conversation array. Anything else — SSE transcripts, audio, embeddings,
// malformed JSON — becomes a single block, so chunking can never fail a write.
func planBody(body []byte) bodyPlan {
	if len(body) < minChunkedBodySize {
		return bodyPlan{kind: kindRaw, size: len(body), blocks: [][]byte{body}}
	}
	for _, field := range messageFields {
		if plan, ok := planMessagesField(body, field); ok {
			return plan
		}
	}
	return bodyPlan{kind: kindRaw, size: len(body), blocks: [][]byte{body}}
}

// planMessagesField extracts one conversation array and replaces every element
// with a placeholder so the template stays small while reassembly stays exact.
func planMessagesField(body []byte, field string) (bodyPlan, bool) {
	result := gjson.GetBytes(body, field)
	if !result.IsArray() {
		return bodyPlan{}, false
	}
	elements := result.Array()
	if len(elements) == 0 {
		return bodyPlan{}, false
	}

	var template bytes.Buffer
	blocks := make([][]byte, 0, len(elements))
	cursor := 0
	for index, element := range elements {
		// Only objects are conversation turns. An embeddings request reuses the
		// "input" key with an array of raw strings, which stays a single block.
		if !element.IsObject() {
			return bodyPlan{}, false
		}
		raw := element.Raw
		if raw == "" {
			return bodyPlan{}, false
		}
		start := int(element.Index)
		end := start + len(raw)
		if start < cursor || end > len(body) || start >= end {
			return bodyPlan{}, false
		}
		template.Write(body[cursor:start])
		template.WriteString(placeholder(index))
		cursor = end
		if index < len(elements)-1 {
			// The original separators (comma and whitespace) stay in the
			// template, so reassembly does not have to guess formatting.
			if end >= len(body) {
				return bodyPlan{}, false
			}
			if body[end] == ',' {
				template.WriteByte(',')
				cursor = end + 1
			}
		}
		blocks = append(blocks, []byte(raw))
	}
	template.Write(body[cursor:])

	return bodyPlan{
		kind:     kindChat,
		size:     len(body),
		template: template.Bytes(),
		blocks:   blocks,
	}, true
}

func placeholder(index int) string {
	return "\x00MSG" + strconv.Itoa(index) + "\x00"
}

// reassemble rebuilds the original body from its template and blocks. A raw
// plan has no template: its single block is the body.
func reassemble(template []byte, blocks [][]byte) ([]byte, error) {
	if len(template) == 0 {
		if len(blocks) == 1 {
			return blocks[0], nil
		}
		if len(blocks) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("audit body has %d blocks and no template", len(blocks))
	}
	out := make([]byte, 0, len(template))
	cursor := 0
	for index, block := range blocks {
		marker := placeholder(index)
		position := bytes.Index(template[cursor:], []byte(marker))
		if position < 0 {
			return nil, fmt.Errorf("audit body template is missing placeholder %d", index)
		}
		out = append(out, template[cursor:cursor+position]...)
		out = append(out, block...)
		cursor += position + len(marker)
	}
	out = append(out, template[cursor:]...)
	return out, nil
}

// marshalDescriptor encodes a descriptor for storage.
func marshalDescriptor(d descriptor) ([]byte, error) {
	return json.Marshal(d)
}

// unmarshalDescriptor decodes a stored descriptor.
func unmarshalDescriptor(data []byte) (descriptor, error) {
	var d descriptor
	if err := json.Unmarshal(data, &d); err != nil {
		return descriptor{}, fmt.Errorf("decode audit body descriptor: %w", err)
	}
	return d, nil
}

package core

import (
	"math"
)

// EstimateTokens approximates the token count of text. It is used only for
// the Anthropic message_start input count, which the real usage frame
// corrects; it never affects billing.
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	// ~4 bytes per token for English, ~1 token per 2 bytes for CJK. Using
	// len/3 is a middle ground; the value is an estimate only.
	estimate := int(math.Ceil(float64(len(text)) / 3.0))
	if estimate < 1 {
		estimate = 1
	}
	return estimate
}

// estimateRequestTokens estimates the prompt size for message_start.
// EstimateRequestTokens approximates the prompt size of a request.
func EstimateRequestTokens(request *Request) int {
	if request == nil {
		return 0
	}
	total := 0
	for _, part := range request.System {
		total += EstimateTokens(part.Text)
	}
	for _, message := range request.Messages {
		total += estimateMessageTokens(message)
	}
	return total
}

func estimateMessageTokens(message Message) int {
	total := 0
	for _, part := range message.Parts {
		switch part.Kind {
		case PartText, PartReasoning, PartRefusal:
			total += EstimateTokens(part.Text)
		case PartToolCall:
			total += EstimateTokens(part.ToolName) + EstimateTokens(part.ArgumentsText) + 8
		case PartToolResult:
			total += 4
			for _, nested := range part.Content {
				total += EstimateTokens(nested.Text)
			}
		case PartImage:
			// Vision tokens are model specific; a flat allowance is safer
			// than zero.
			total += 256
		}
	}
	return total
}

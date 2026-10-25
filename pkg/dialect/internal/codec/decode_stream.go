package codec

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
)

func DecodeStream(format Format, body []byte) (*Response, []Diagnostic, error) {
	target := OpenAIResponses
	if format == target {
		target = OpenAIChat
	}
	c, err := NewStreamConverter(format, target, StreamOptions{})
	if err != nil {
		return nil, nil, err
	}
	var data []string
	events := 0
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		events++
		data = nil
		if payload == "[DONE]" {
			c.MarkUpstreamTerminated()
			return nil
		}
		_, err := c.Feed([]byte(payload))
		return err
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), 16<<20)
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return nil, c.Diagnostics(), err
			}
			continue
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(value, " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, c.Diagnostics(), err
	}
	if err := flush(); err != nil {
		return nil, c.Diagnostics(), err
	}
	if events == 0 {
		return nil, c.Diagnostics(), errors.New("response contains no SSE data events")
	}
	if _, err := c.Finish(); err != nil {
		return nil, c.Diagnostics(), err
	}
	for i := range c.response.Parts {
		part := &c.response.Parts[i]
		if part.Kind == PartToolCall && part.Arguments == nil {
			part.Arguments = parseArguments(part.ArgumentsText, &c.diagnostics, "tool_calls.arguments")
		}
	}
	if c.response.Status == "" {
		c.response.Status = "completed"
		if c.response.FinishReason == FinishLength || c.response.FinishReason == FinishContentFilter {
			c.response.Status = "incomplete"
		}
	}
	return &c.response, c.Diagnostics(), nil
}

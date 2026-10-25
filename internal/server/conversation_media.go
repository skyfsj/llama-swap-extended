package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/conversation"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

func (s *Server) hydrateConversationParts(ctx context.Context, parts []dialect.Part) ([]dialect.Part, error) {
	result := make([]dialect.Part, len(parts))
	for index, part := range parts {
		if strings.HasPrefix(part.URL, "gfile_") {
			file, err := s.store.GetProtocolFile(ctx, part.URL, identityIDFromContext(ctx))
			if err != nil {
				return nil, err
			}
			part.URL = ""
			part.Base64 = base64.StdEncoding.EncodeToString(file.Content)
			part.MediaType = file.MIME
			part.Filename = file.Filename
		}
		nested, err := s.hydrateConversationParts(ctx, part.Content)
		if err != nil {
			return nil, err
		}
		part.Content = nested
		result[index] = part
	}
	return result, nil
}
func (s *Server) hydrateConversationState(ctx context.Context, state conversation.State) (conversation.State, error) {
	state.Messages = append([]dialect.Message(nil), state.Messages...)
	for index := range state.Messages {
		parts, err := s.hydrateConversationParts(ctx, state.Messages[index].Parts)
		if err != nil {
			return state, err
		}
		state.Messages[index].Parts = parts
	}
	return state, nil
}
func (s *Server) compactConversationParts(ctx context.Context, parts []dialect.Part) []dialect.Part {
	result := make([]dialect.Part, len(parts))
	for index, part := range parts {
		if part.Base64 != "" {
			if raw, err := base64.StdEncoding.DecodeString(part.Base64); err == nil {
				if file, err := s.store.FindProtocolFile(ctx, mediaDigest(raw), identityIDFromContext(ctx)); err == nil {
					part.Base64 = ""
					part.URL = file.ID
					part.Filename = file.Filename
					part.MediaType = file.MIME
				}
			}
		}
		part.Content = s.compactConversationParts(ctx, part.Content)
		result[index] = part
	}
	return result
}
func (s *Server) compactConversationRequest(ctx context.Context, body []byte) ([]byte, error) {
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	_, err := walkMedia(raw, func(block map[string]any) (map[string]any, error) {
		encoded, _ := inlineMedia(block)
		if encoded == "" {
			return nil, nil
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, nil
		}
		file, err := s.store.FindProtocolFile(ctx, mediaDigest(data), identityIDFromContext(ctx))
		if err != nil {
			return nil, nil
		}
		return map[string]any{"type": "file", "file_id": file.ID}, nil
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(raw)
}
func (s *Server) compactConversationMedia(ctx context.Context, state *conversation.State) error {
	for index := range state.Messages {
		state.Messages[index].Parts = s.compactConversationParts(ctx, state.Messages[index].Parts)
	}
	for key, native := range state.Native {
		request, err := s.compactConversationRequest(ctx, native.Request)
		if err != nil {
			return err
		}
		native.Request = request
		state.Native[key] = native
	}
	return nil
}

func conversationSummaryParts(parts []dialect.Part) []dialect.Part {
	result := make([]dialect.Part, len(parts))
	for index, part := range parts {
		if strings.HasPrefix(part.URL, "gfile_") {
			part = dialect.Part{Kind: dialect.PartText, Text: "Attached file: " + part.URL + " (" + part.Filename + ", " + part.MediaType + ")"}
		}
		part.Content = conversationSummaryParts(part.Content)
		result[index] = part
	}
	return result
}

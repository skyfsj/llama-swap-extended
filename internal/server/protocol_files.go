package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/conversation"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

func (s *Server) handleProtocolFiles(w http.ResponseWriter, r *http.Request) {
	owner := identityIDFromContext(r.Context())
	if r.Method == http.MethodGet {
		files, err := s.store.ListProtocolFiles(r.Context(), owner)
		if err != nil {
			swaputil.SendResponse(w, r, 500, err.Error())
			return
		}
		protocolJSON(w, 200, map[string]any{"object": "list", "data": files, "has_more": len(files) == 1000})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, store.MaxProtocolFileBytes+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		swaputil.SendResponse(w, r, 413, "invalid multipart upload or file too large")
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, header, err := r.FormFile("file")
	if err != nil {
		swaputil.SendResponse(w, r, 400, "file is required")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, store.MaxProtocolFileBytes+1))
	if err != nil || len(data) > store.MaxProtocolFileBytes {
		swaputil.SendResponse(w, r, 413, "file exceeds 16 MiB")
		return
	}
	filename := filepath.Base(strings.ReplaceAll(header.Filename, "\\", "/"))
	mediaType, _, _ := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if mediaType == "" || mediaType == "application/octet-stream" {
		mediaType = mime.TypeByExtension(filepath.Ext(filename))
		if mediaType == "" {
			mediaType = http.DetectContentType(data)
		}
	}
	f := store.ProtocolFile{ID: protocolObjectID("gfile_"), Owner: owner, Filename: filename, MIME: mediaType, Content: data}
	if err := s.store.PutProtocolFile(r.Context(), f); err != nil {
		swaputil.SendResponse(w, r, 400, err.Error())
		return
	}
	saved, err := s.store.GetProtocolFile(r.Context(), f.ID, owner)
	if err != nil {
		swaputil.SendResponse(w, r, 500, err.Error())
		return
	}
	protocolJSON(w, 201, saved)
}
func (s *Server) handleProtocolFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("file_id")
	owner := identityIDFromContext(r.Context())
	f, err := s.store.GetProtocolFile(r.Context(), id, owner)
	if err != nil {
		swaputil.SendResponse(w, r, 404, "file not found")
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.store.DeleteProtocolFile(r.Context(), id, owner); err != nil {
			swaputil.SendResponse(w, r, 409, err.Error())
			return
		}
		protocolJSON(w, 200, map[string]any{"id": id, "object": "file", "deleted": true})
		return
	}
	if strings.HasSuffix(r.URL.Path, "/content") {
		w.Header().Set("Content-Type", f.MIME)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.Filename}))
		w.Header().Set("Cache-Control", "private, no-store")
		w.WriteHeader(200)
		_, _ = w.Write(f.Content)
		return
	}
	protocolJSON(w, 200, f)
}

type ownedMedia struct{ File store.ProtocolFile }

func mediaDigest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func fileBlock(format dialect.Format, f store.ProtocolFile) map[string]any {
	data := base64.StdEncoding.EncodeToString(f.Content)
	kind := "document"
	if strings.HasPrefix(f.MIME, "image/") {
		kind = "image"
	} else if strings.HasPrefix(f.MIME, "audio/") {
		kind = "audio"
	} else if strings.HasPrefix(f.MIME, "video/") {
		kind = "video"
	}
	switch format {
	case dialect.OpenAIChat:
		if kind == "image" {
			return map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + f.MIME + ";base64," + data}}
		}
		return map[string]any{"type": "file", "file": map[string]any{"filename": f.Filename, "file_data": "data:" + f.MIME + ";base64," + data}}
	case dialect.OpenAIResponses:
		if kind == "image" {
			return map[string]any{"type": "input_image", "image_url": "data:" + f.MIME + ";base64," + data}
		}
		return map[string]any{"type": "input_file", "filename": f.Filename, "file_data": "data:" + f.MIME + ";base64," + data}
	case dialect.ClaudeMessages:
		return map[string]any{"type": kind, "source": map[string]any{"type": "base64", "media_type": f.MIME, "data": data}}
	case dialect.Gemini:
		return map[string]any{"inlineData": map[string]any{"mimeType": f.MIME, "data": data}}
	default:
		return map[string]any{"type": kind, "mime_type": f.MIME, "data": data}
	}
}
func ownedFileID(block map[string]any) string {
	for _, key := range []string{"file_id", "uri", "file_uri", "fileUri"} {
		if value, ok := block[key].(string); ok && strings.HasPrefix(value, "gfile_") {
			return value
		}
	}
	for _, key := range []string{"file", "source", "fileData", "file_data"} {
		if child, ok := block[key].(map[string]any); ok {
			if id := ownedFileID(child); id != "" {
				return id
			}
		}
	}
	return ""
}
func walkMedia(value any, replace func(map[string]any) (map[string]any, error)) (any, error) {
	switch value := value.(type) {
	case map[string]any:
		if value["type"] == "function_call" || value["type"] == "tool_use" || value["functionCall"] != nil {
			return value, nil
		}
		next, err := replace(value)
		if err != nil {
			return nil, err
		}
		if next != nil {
			return next, nil
		}
		for key, child := range value {
			if key == "tools" || key == "arguments" || key == "parameters" || key == "input_schema" {
				continue
			}
			converted, err := walkMedia(child, replace)
			if err != nil {
				return nil, err
			}
			value[key] = converted
		}
		return value, nil
	case []any:
		for i, child := range value {
			converted, err := walkMedia(child, replace)
			if err != nil {
				return nil, err
			}
			value[i] = converted
		}
		return value, nil
	default:
		return value, nil
	}
}

func createProtocolFilesMiddleware(s *Server) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			route, ok := dialect.RouteForRequest(r.Method, r.URL.Path)
			if !ok || s.store == nil {
				next.ServeHTTP(w, r)
				return
			}
			body, err := readResponsesRequestBody(r)
			if err != nil {
				swaputil.SendResponse(w, r, 413, err.Error())
				return
			}
			// walkMedia only rewrites blocks that carry one of this daemon's own
			// file ids, so a body that mentions none of them cannot change. The
			// scan is deliberately conservative: a JSON body may spell any
			// character with a \u escape, so a body that contains a backslash
			// escape at all still takes the full path — the byte search is only
			// trusted for bodies whose text is written literally.
			//
			// The upstream hook is installed either way: a conversation replays
			// its earlier turns through this same middleware, and those turns
			// carry the file blocks an earlier request resolved.
			if !bytes.Contains(body, []byte("gfile_")) && !bytes.Contains(body, []byte("\\")) {
				if err := setProtocolRequestBody(r, body); err != nil {
					swaputil.SendResponse(w, r, 400, err.Error())
					return
				}
				owned := map[string]ownedMedia{}
				r = r.WithContext(swaputil.WithUpstreamHooks(r.Context(), swaputil.UpstreamHooks{
					Prepare: func(ctx context.Context, scope conversation.Scope, request *http.Request) error {
						return s.prepareOwnedMedia(ctx, scope, request, owned)
					},
				}))
				next.ServeHTTP(w, r)
				return
			}
			var input any
			if json.Unmarshal(body, &input) != nil {
				swaputil.SendResponse(w, r, 400, "invalid JSON request")
				return
			}
			owned := map[string]ownedMedia{}
			_, err = walkMedia(input, func(block map[string]any) (map[string]any, error) {
				id := ownedFileID(block)
				if id == "" {
					return nil, nil
				}
				f, err := s.store.GetProtocolFile(r.Context(), id, identityIDFromContext(r.Context()))
				if err != nil {
					return nil, errors.New("gateway file not found or not owned by caller")
				}
				owned[mediaDigest(f.Content)] = ownedMedia{File: f}
				return fileBlock(route.Format, f), nil
			})
			if err != nil {
				swaputil.SendResponse(w, r, 400, err.Error())
				return
			}
			if len(owned) > 0 {
				body, err = json.Marshal(input)
				if err != nil {
					swaputil.SendResponse(w, r, 400, err.Error())
					return
				}
			}
			if err := setProtocolRequestBody(r, body); err != nil {
				swaputil.SendResponse(w, r, 400, err.Error())
				return
			}
			hooks := swaputil.UpstreamHooks{Prepare: func(ctx context.Context, scope conversation.Scope, request *http.Request) error {
				return s.prepareOwnedMedia(ctx, scope, request, owned)
			}}
			r = r.WithContext(swaputil.WithUpstreamHooks(r.Context(), hooks))
			next.ServeHTTP(w, r)
		})
	}
}

func inlineMedia(block map[string]any) (string, string) {
	for _, key := range []string{"inlineData", "inline_data", "source", "input_audio"} {
		if child, ok := block[key].(map[string]any); ok {
			data, _ := child["data"].(string)
			if data != "" {
				mime, _ := child["mimeType"].(string)
				if mime == "" {
					mime, _ = child["media_type"].(string)
				}
				return data, mime
			}
		}
	}
	if data, ok := block["data"].(string); ok && block["mime_type"] != nil {
		return data, fmt.Sprint(block["mime_type"])
	}
	for _, key := range []string{"file_data", "image_url", "url"} {
		if data, ok := block[key].(string); ok && strings.HasPrefix(data, "data:") {
			parts := strings.SplitN(data, ",", 2)
			if len(parts) == 2 && strings.HasSuffix(parts[0], ";base64") {
				return parts[1], strings.TrimSuffix(strings.TrimPrefix(parts[0], "data:"), ";base64")
			}
		}
	}
	if file, ok := block["file"].(map[string]any); ok {
		return inlineMedia(file)
	}
	if image, ok := block["image_url"].(map[string]any); ok {
		return inlineMedia(image)
	}
	return "", ""
}
func (s *Server) prepareOwnedMedia(ctx context.Context, scope conversation.Scope, r *http.Request, owned map[string]ownedMedia) error {
	if r.GetBody == nil {
		return errors.New("file request cannot be replayed")
	}
	body, err := r.GetBody()
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(body, swaputil.MaxRequestBodySize+1))
	body.Close()
	if err != nil {
		return err
	}
	var input any
	if json.Unmarshal(data, &input) != nil {
		return errors.New("file request is invalid JSON")
	}
	_, err = walkMedia(input, func(block map[string]any) (map[string]any, error) {
		if id := ownedFileID(block); id != "" {
			file, err := s.store.GetProtocolFile(ctx, id, identityIDFromContext(ctx))
			if err != nil {
				return nil, errors.New("gateway file not found or not owned by caller")
			}
			resolved := fileBlock(scope.Format, file)
			for key := range block {
				delete(block, key)
			}
			for key, value := range resolved {
				block[key] = value
			}
			owned[mediaDigest(file.Content)] = ownedMedia{File: file}
		}
		encoded, _ := inlineMedia(block)
		if encoded == "" {
			return nil, nil
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, err
		}
		item, ok := owned[mediaDigest(raw)]
		if !ok {
			file, err := s.store.FindProtocolFile(ctx, mediaDigest(raw), identityIDFromContext(ctx))
			if err != nil {
				return nil, nil
			}
			item = ownedMedia{File: file}
		}
		if scope.Provider == "local" {
			return nil, nil
		}
		if scope.Format == dialect.OpenAIChat && item.File.MIME != "application/pdf" {
			return nil, nil
		}
		ref, err := s.uploadOwnedFile(ctx, scope, r, item.File)
		if err != nil {
			return nil, err
		}
		switch scope.Format {
		case dialect.OpenAIChat:
			return map[string]any{"type": "file", "file": map[string]any{"file_id": ref}}, nil
		case dialect.OpenAIResponses:
			kind := "input_file"
			if strings.HasPrefix(item.File.MIME, "image/") {
				kind = "input_image"
			}
			return map[string]any{"type": kind, "file_id": ref}, nil
		case dialect.ClaudeMessages:
			kind := "document"
			if strings.HasPrefix(item.File.MIME, "image/") {
				kind = "image"
			}
			return map[string]any{"type": kind, "source": map[string]any{"type": "file", "file_id": ref}}, nil
		case dialect.Gemini:
			return map[string]any{"fileData": map[string]any{"mimeType": item.File.MIME, "fileUri": ref}}, nil
		default:
			kind := "document"
			if strings.HasPrefix(item.File.MIME, "image/") {
				kind = "image"
			}
			return map[string]any{"type": kind, "mime_type": item.File.MIME, "uri": ref}, nil
		}
	})
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, encoded) {
		return setProtocolRequestBody(r, encoded)
	}
	return nil
}

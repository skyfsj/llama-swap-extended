package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/conversation"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

func providerFilesURL(r *http.Request, format dialect.Format, upload bool) (*url.URL, error) {
	u := *r.URL
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	prefix := ""
	found := false
	for _, marker := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages", "/v1beta/models/", "/v1beta/interactions", "/v1/interactions"} {
		if index := strings.LastIndex(u.Path, marker); index >= 0 {
			prefix = u.Path[:index]
			found = true
			break
		}
	}
	if !found {
		return nil, errors.New("cannot derive provider Files API from inference endpoint")
	}
	u.Path = prefix + "/v1/files"
	if format == dialect.Gemini || format == dialect.GeminiInteractions {
		u.Path = prefix + "/v1beta/files"
		if upload {
			u.Path = prefix + "/upload/v1beta/files"
		}
	}
	return &u, nil
}
func fileRequest(ctx context.Context, source *http.Request, method string, u *url.URL, body io.Reader) *http.Request {
	r, _ := http.NewRequestWithContext(ctx, method, u.String(), body)
	for _, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Anthropic-Version", "Anthropic-Beta"} {
		if value := source.Header.Get(name); value != "" {
			r.Header.Set(name, value)
		}
	}
	r.Header.Set("Accept-Encoding", "identity")
	return r
}
func fileResponse(client *http.Client, r *http.Request) ([]byte, http.Header, int, error) {
	response, err := client.Do(r)
	if err != nil {
		return nil, nil, 0, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return nil, nil, response.StatusCode, err
	}
	if len(body) > 1<<20 {
		return nil, nil, response.StatusCode, errors.New("files API response exceeds capacity")
	}
	return body, response.Header, response.StatusCode, nil
}
func requireFileSuccess(body []byte, status int) error {
	if status < 200 || status >= 300 {
		return fmt.Errorf("provider Files API returned HTTP %d", status)
	}
	return nil
}

func (s *Server) uploadOwnedFile(ctx context.Context, scope conversation.Scope, source *http.Request, f store.ProtocolFile) (string, error) {
	client := swaputil.UpstreamHTTPClient(ctx)
	if client == nil {
		return "", errors.New("provider file upload transport is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	u, err := providerFilesURL(source, scope.Format, false)
	if err != nil {
		return "", err
	}
	scopeKey := scope.Key()
	owner := identityIDFromContext(ctx)
	if scope.Format == dialect.ClaudeMessages {
		beta := source.Header.Get("Anthropic-Beta")
		if !strings.Contains(beta, "files-api-2025-04-14") {
			if beta != "" {
				beta += ","
			}
			source.Header.Set("Anthropic-Beta", beta+"files-api-2025-04-14")
		}
		if source.Header.Get("Anthropic-Version") == "" {
			source.Header.Set("Anthropic-Version", "2023-06-01")
		}
	}
	if reference, err := s.store.ProtocolFileReference(ctx, f.ID, owner, scopeKey); err == nil {
		check := *u
		if scope.Format == dialect.Gemini || scope.Format == dialect.GeminiInteractions {
			remote, err := url.Parse(reference)
			if err != nil || remote.Host != u.Host || remote.Scheme != u.Scheme {
				return "", errors.New("invalid cached provider file origin")
			}
			check = *remote
		} else {
			check.Path += "/" + url.PathEscape(reference)
		}
		body, _, status, err := fileResponse(client, fileRequest(ctx, source, http.MethodGet, &check, nil))
		if err != nil {
			return "", err
		}
		if status >= 200 && status < 300 {
			return reference, nil
		}
		if status != 404 && status != 410 {
			return "", requireFileSuccess(body, status)
		}
	}
	var reference string
	expires := time.Now().Add(24 * time.Hour)
	if scope.Format == dialect.Gemini || scope.Format == dialect.GeminiInteractions {
		startURL, err := providerFilesURL(source, scope.Format, true)
		if err != nil {
			return "", err
		}
		metadata, _ := json.Marshal(map[string]any{"file": map[string]any{"display_name": f.Filename}})
		request := fileRequest(ctx, source, http.MethodPost, startURL, bytes.NewReader(metadata))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Goog-Upload-Protocol", "resumable")
		request.Header.Set("X-Goog-Upload-Command", "start")
		request.Header.Set("X-Goog-Upload-Header-Content-Length", fmt.Sprint(len(f.Content)))
		request.Header.Set("X-Goog-Upload-Header-Content-Type", f.MIME)
		body, headers, status, err := fileResponse(client, request)
		if err != nil {
			return "", err
		}
		if err := requireFileSuccess(body, status); err != nil {
			return "", err
		}
		upload, err := url.Parse(headers.Get("X-Goog-Upload-URL"))
		if err != nil || upload.Host != startURL.Host || upload.Scheme != startURL.Scheme || upload.User != nil {
			return "", errors.New("provider returned an invalid cross-origin upload URL")
		}
		request = fileRequest(ctx, source, http.MethodPost, upload, bytes.NewReader(f.Content))
		request.Header.Set("Content-Type", f.MIME)
		request.Header.Set("X-Goog-Upload-Offset", "0")
		request.Header.Set("X-Goog-Upload-Command", "upload, finalize")
		body, _, status, err = fileResponse(client, request)
		if err != nil {
			return "", err
		}
		if err := requireFileSuccess(body, status); err != nil {
			return "", err
		}
		var response struct {
			File struct {
				URI        string `json:"uri"`
				Name       string `json:"name"`
				Expiration string `json:"expirationTime"`
				State      string `json:"state"`
			} `json:"file"`
		}
		if json.Unmarshal(body, &response) != nil || response.File.URI == "" {
			return "", errors.New("provider returned no file URI")
		}
		reference = response.File.URI
		if value, err := time.Parse(time.RFC3339, response.File.Expiration); err == nil {
			expires = value
		}
		for response.File.State == "PROCESSING" {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
			poll := *u
			poll.Path = strings.TrimSuffix(u.Path, "/files") + "/" + response.File.Name
			body, _, status, err = fileResponse(client, fileRequest(ctx, source, http.MethodGet, &poll, nil))
			if err != nil {
				return "", err
			}
			if err := requireFileSuccess(body, status); err != nil {
				return "", err
			}
			if json.Unmarshal(body, &response.File) != nil {
				return "", errors.New("invalid processing file response")
			}
		}
		if response.File.State == "FAILED" {
			return "", errors.New("provider file processing failed")
		}
	} else {
		var payload bytes.Buffer
		writer := multipart.NewWriter(&payload)
		if scope.Format == dialect.OpenAIChat || scope.Format == dialect.OpenAIResponses {
			if err := writer.WriteField("purpose", "user_data"); err != nil {
				return "", err
			}
		}
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": f.Filename}))
		header.Set("Content-Type", f.MIME)
		part, err := writer.CreatePart(header)
		if err != nil {
			return "", err
		}
		if _, err := part.Write(f.Content); err != nil {
			return "", err
		}
		if err := writer.Close(); err != nil {
			return "", err
		}
		request := fileRequest(ctx, source, http.MethodPost, u, &payload)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		body, _, status, err := fileResponse(client, request)
		if err != nil {
			return "", err
		}
		if err := requireFileSuccess(body, status); err != nil {
			return "", err
		}
		var response struct {
			ID      string `json:"id"`
			Expires int64  `json:"expires_at"`
		}
		if json.Unmarshal(body, &response) != nil || response.ID == "" {
			return "", errors.New("provider returned no file ID")
		}
		reference = response.ID
		if response.Expires > 0 {
			expires = time.Unix(response.Expires, 0)
		}
	}
	if err := s.store.PutProtocolFileReference(ctx, f.ID, owner, scopeKey, reference, expires); err != nil {
		return "", err
	}
	return reference, nil
}

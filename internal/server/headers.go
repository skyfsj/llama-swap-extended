package server

import (
	"net/http"
	"strings"
)

// sensitiveHeaders lists headers that are redacted before durable audit
// conversations are persisted.
var sensitiveHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
	"api-key":             true,
	"api_key":             true,
	"x-anthropic-api-key": true,
	"x-goog-api-key":      true,
	"x-auth-token":        true,
	"token":               true,
}

// headerMap flattens an http.Header to a single-value map.
func headerMap(h http.Header) map[string]string {
	m := make(map[string]string, len(h))
	for key, values := range h {
		if len(values) > 0 {
			m[key] = values[0]
		}
	}
	return m
}

// redactHeaders replaces sensitive header values in-place with "[REDACTED]".
func redactHeaders(headers map[string]string) {
	for key := range headers {
		lower := strings.ToLower(key)
		if sensitiveHeaders[lower] || strings.HasSuffix(lower, "-api-key") || strings.HasSuffix(lower, "_api_key") || strings.HasSuffix(lower, "-auth-token") || strings.HasSuffix(lower, "-token") {
			headers[key] = "[REDACTED]"
		}
	}
}

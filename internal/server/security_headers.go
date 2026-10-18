package server

import (
	"net/http"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/chain"
)

// uiContentSecurityPolicy allows the SPA to load its own bundle, fonts and
// workers while denying plugin content, framing and cross-origin requests.
// style-src needs 'unsafe-inline' because the UI renders ANSI log spans and
// syntax-highlighted HTML with inline style attributes; script-src does not,
// and no inline script is emitted by the build.
const uiContentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; " +
	"font-src 'self' data:; " +
	"connect-src 'self'; " +
	"worker-src 'self' blob:; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// passthroughPrefixes are reverse-proxied applications: their response
// headers belong to the upstream server, and imposing our CSP or frame
// denial would break them.
var headerPassthroughPrefixes = []string{"/upstream", "/comfyui"}

// CreateSecurityHeadersMiddleware adds the defensive response headers to
// llama-swap's own responses. It runs before routing so every handler,
// including errors written by middleware, carries them.
func CreateSecurityHeadersMiddleware(enabled bool) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !enabled || isHeaderPassthroughPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			header := w.Header()
			// The API never returns a sniffable payload: audit bodies are
			// served as attachments and everything else declares JSON.
			header.Set("X-Content-Type-Options", "nosniff")
			header.Set("Referrer-Policy", "no-referrer")
			header.Set("X-Frame-Options", "DENY")
			header.Set("Permissions-Policy", "accelerometer=(), camera=(), geolocation=(), microphone=(), payment=(), usb=()")
			if isUIPath(r.URL.Path) {
				header.Set("Content-Security-Policy", uiContentSecurityPolicy)
			}
			next.ServeHTTP(w, r)
		})
	}
}

func isHeaderPassthroughPath(path string) bool {
	for _, prefix := range headerPassthroughPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// isUIPath reports whether the request is served by the embedded SPA.
func isUIPath(path string) bool {
	return path == "/ui" || strings.HasPrefix(path, "/ui/") || path == "/" || path == "/favicon.ico"
}

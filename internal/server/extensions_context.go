package server

import (
	"net/http"
	"sort"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/extensions"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// maxExtensionModelSnapshot bounds the embedded model list: it travels inside
// every worker request.
const maxExtensionModelSnapshot = 200

// requestLocale resolves the caller's preferred UI locale from Accept-Language.
// Unknown and partial tags fall back to zh-CN, the default console language.
func requestLocale(r *http.Request) string {
	for _, tag := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		language, _, _ := strings.Cut(tag, ";")
		language = strings.TrimSpace(strings.ToLower(language))
		switch language {
		case "", "*":
			continue
		case "zh-tw", "zh-hk", "zh-hant", "zh-mo":
			return "zh-TW"
		case "zh", "zh-cn", "zh-hans", "zh-sg":
			return "zh-CN"
		case "en", "en-us", "en-gb":
			return "en"
		}
		primary, _, _ := strings.Cut(language, "-")
		if primary == "zh" {
			return "zh-CN"
		}
		if primary == "en" {
			return "en"
		}
	}
	return "zh-CN"
}

// extensionSession builds the read-only session snapshot attached to hook
// contexts. The key id mirrors what metrics records: absent on keyless
// deployments.
func extensionSession(r *http.Request, identity auth.Identity) *extensions.SessionContext {
	session := &extensions.SessionContext{KeyID: "anonymous", Anonymous: true}
	if identity.ID != "" && identity.ID != "anonymous" {
		session.KeyID = identity.ID
		session.Anonymous = identity.Legacy
	}
	if data, ok := swaputil.ReadContext(r.Context()); ok {
		session.ID = data.Metadata["session_id"]
		if keyID := data.Metadata["key_id"]; keyID != "" {
			session.KeyID = keyID
			session.Anonymous = false
		}
	}
	return session
}

// extensionModelSnapshot lists the models the caller may reach, which is the
// set a script may forward to. Disabled models are skipped exactly like the
// model list endpoint.
func extensionModelSnapshot(cfg config.Config, identity auth.Identity) []extensions.ModelInfo {
	ids := make([]string, 0, len(cfg.Models))
	for id, model := range cfg.Models {
		if model.Disabled {
			continue
		}
		if !modelAllowedForIdentity(cfg, identity, id) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	snapshot := make([]extensions.ModelInfo, 0, len(ids))
	for _, id := range ids {
		entry := extensions.ModelInfo{ID: id, Name: cfg.Models[id].Name}
		if entry.Name == id {
			entry.Name = ""
		}
		snapshot = append(snapshot, entry)
		if len(snapshot) >= maxExtensionModelSnapshot {
			break
		}
	}
	return snapshot
}

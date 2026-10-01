// Package auth contains key hashing and authorization primitives shared by
// inference and control-plane handlers.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"unicode"
)

const (
	// SessionCookieName is a transient, same-origin browser credential used by
	// the WebUI. The cookie contains the user-supplied key only in the browser
	// session; authentication continues to use the store's salted hash.
	SessionCookieName = "llama_swap_api_key"
	ScopeInference    = "inference"
	ScopeLogs         = "logs"
	ScopeModelUnload  = "model-unload"
	ScopeRuntimeRead  = "runtime-read"
	ScopeRuntimeAdmin = "runtime-admin"
	ScopeConfigAdmin  = "config-admin"
	ScopeKeysAdmin    = "keys-admin"
	ScopeAuditRead    = "audit-read"
	ScopeAuditAdmin   = "audit-admin"
	ScopeCacheAdmin   = "cache-admin"
	// MaxAPIKeyNameLength bounds the operator-supplied display name stored in
	// SQLite and returned by the control plane. It is intentionally separate
	// from the secret length, which is generated and has its own format.
	MaxAPIKeyNameLength = 256
	// KeyKindManagement marks a control-plane credential that may sign in to the
	// management panel. KeyKindAccess marks a model-access credential nested
	// under a management key. Legacy YAML credentials have no kind.
	KeyKindManagement = "management"
	KeyKindAccess     = "access"
	// MaxAccessIPEntries and MaxAccessGroupLength bound the access-key
	// restriction fields so a single key cannot carry an unbounded allowlist or
	// label into the key cache snapshot.
	MaxAccessIPEntries    = 64
	MaxAccessIPTextLength = 64
	MaxAccessGroupLength  = 64
)

type Identity struct {
	ID                   string
	Scopes               map[string]struct{}
	Models               []string
	AllowManagementLogin bool
	Legacy               bool
	// Kind records whether the credential is a management key, an access key
	// nested under one, or empty for a legacy YAML credential. Middleware uses
	// it to decide whether the access-key restrictions apply.
	Kind string
	// ParentID names the management key that owns an access key. It is empty
	// for management and legacy credentials.
	ParentID string
	// AllowedIPs is the access key's caller allowlist, held as normalized
	// addresses or CIDR blocks. An empty list is unrestricted.
	AllowedIPs []string
	// MaxConcurrency bounds how many requests the key may have in flight at
	// once. Zero means unlimited.
	MaxConcurrency int
	// Group is the free-form usage grouping label attached to an access key.
	Group string
}

func (i Identity) Has(scope string) bool {
	if i.Legacy {
		return true
	}
	_, ok := i.Scopes[scope]
	return ok
}

func (i Identity) HasModel(model string) bool {
	if len(i.Models) == 0 {
		return true
	}
	for _, pattern := range i.Models {
		if wildcardMatch(pattern, model) {
			return true
		}
	}
	return false
}

func wildcardMatch(pattern, value string) bool {
	if pattern == "*" || pattern == value {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(value, strings.TrimSuffix(pattern, "*"))
	}
	if strings.HasPrefix(pattern, "*") {
		return strings.HasSuffix(value, strings.TrimPrefix(pattern, "*"))
	}
	return false
}

func Hash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// SaltedHash hashes a secret with a caller-provided random salt. The salt is
// safe to persist beside the digest; authentication never reads the retained
// raw value used by explicit server-side integrations.
func SaltedHash(raw string, salt []byte) string {
	h := sha256.New()
	_, _ = h.Write(salt)
	_, _ = h.Write([]byte(raw))
	return hex.EncodeToString(h.Sum(nil))
}

func GenerateSalted(raw string) (salt []byte, digest string, err error) {
	salt = make([]byte, 16)
	if _, err = rand.Read(salt); err != nil {
		return nil, "", fmt.Errorf("generate api key salt: %w", err)
	}
	return salt, SaltedHash(raw, salt), nil
}

func VerifySalted(raw string, salt []byte, expected string) bool {
	actual := SaltedHash(raw, salt)
	return subtle.ConstantTimeCompare([]byte(actual), []byte(strings.ToLower(strings.TrimSpace(expected)))) == 1
}

func Verify(raw, expected string) bool {
	actual := Hash(raw)
	return subtle.ConstantTimeCompare([]byte(actual), []byte(strings.ToLower(strings.TrimSpace(expected)))) == 1
}

func Generate() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate api key: %w", err)
	}
	return "sk-" + base64.RawURLEncoding.EncodeToString(b), nil
}

func NormalizeScopes(scopes []string) (map[string]struct{}, error) {
	allowed := map[string]struct{}{
		ScopeInference: {}, ScopeLogs: {}, ScopeModelUnload: {}, ScopeRuntimeRead: {}, ScopeRuntimeAdmin: {},
		ScopeConfigAdmin: {}, ScopeKeysAdmin: {}, ScopeAuditRead: {}, ScopeAuditAdmin: {}, ScopeCacheAdmin: {},
	}
	out := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		for _, r := range scope {
			if (unicode.IsSpace(r) && r != ' ') || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				return nil, errors.New("api key scope must not contain whitespace or control characters")
			}
		}
		scope = strings.TrimSpace(scope)
		if scope == "" {
			continue
		}
		if _, ok := allowed[scope]; !ok {
			return nil, errors.New("unknown api key scope: " + scope)
		}
		out[scope] = struct{}{}
	}
	return out, nil
}

// ManagementScopes is the complete scope set needed by the local management
// panel. The UI intentionally exposes this as one opt-in rather than making a
// local operator maintain a list of internal route permissions.
func ManagementScopes() []string {
	return []string{
		ScopeInference, ScopeLogs, ScopeModelUnload, ScopeRuntimeRead,
		ScopeRuntimeAdmin, ScopeConfigAdmin, ScopeKeysAdmin, ScopeAuditRead,
		ScopeAuditAdmin, ScopeCacheAdmin,
	}
}

// NormalizeModels validates the optional model allowlist attached to a key.
// Only exact names and a single leading/trailing '*' wildcard are supported;
// accepting arbitrary glob syntax here would make the authorization boundary
// difficult to audit.
func NormalizeModels(models []string) ([]string, error) {
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, model := range models {
		// Trim ordinary ASCII padding for backwards compatibility, but inspect
		// the original value first so strings.TrimSpace cannot erase a
		// non-ASCII whitespace or zero-width marker that should invalidate an
		// authorization pattern.
		for _, r := range model {
			if (unicode.IsSpace(r) && r != ' ') || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				return nil, errors.New("invalid api key model restriction")
			}
		}
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if len(model) > 256 {
			return nil, errors.New("invalid api key model restriction")
		}
		if strings.Count(model, "*") > 1 || (strings.Contains(model, "*") && model != "*" && !strings.HasPrefix(model, "*") && !strings.HasSuffix(model, "*")) {
			return nil, errors.New("api key model restrictions support only exact names or leading/trailing *")
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		out = append(out, model)
	}
	return out, nil
}

// NormalizeKeyName trims an API key display name and rejects whitespace,
// control and zero-width format characters. Empty names are valid (the UI
// falls back to the generated key id), while bounded printable names remain
// safe for table/log rendering and config round-trips.
func NormalizeKeyName(name string) (string, error) {
	for _, r := range name {
		if (unicode.IsSpace(r) && r != ' ') || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", errors.New("api key name must not contain whitespace or control characters")
		}
	}
	name = strings.TrimSpace(name)
	if len(name) > MaxAPIKeyNameLength {
		return "", fmt.Errorf("api key name must be at most %d characters", MaxAPIKeyNameLength)
	}
	return name, nil
}

// NormalizeAccessIPs validates an access key's caller allowlist. Both single
// addresses and CIDR blocks are accepted; anything else is rejected here so an
// invalid entry fails at write time instead of silently matching nothing at
// request time. The list is normalized (lowercase, sorted, de-duplicated) so
// the stored value and the in-memory snapshot compare equal.
func NormalizeAccessIPs(entries []string) ([]string, error) {
	if len(entries) > MaxAccessIPEntries {
		return nil, fmt.Errorf("api key ip allowlist supports at most %d entries", MaxAccessIPEntries)
	}
	seen := make(map[string]struct{}, len(entries))
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		for _, r := range entry {
			if (unicode.IsSpace(r) && r != ' ') || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				return nil, errors.New("api key ip allowlist must not contain whitespace or control characters")
			}
		}
		value := strings.TrimSpace(entry)
		if value == "" {
			continue
		}
		if len(value) > MaxAccessIPTextLength {
			return nil, fmt.Errorf("api key ip allowlist entry must be at most %d characters", MaxAccessIPTextLength)
		}
		normalized, err := normalizeAccessIP(value)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[normalized]; duplicate {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	sort.Strings(out)
	return out, nil
}

func normalizeAccessIP(value string) (string, error) {
	if strings.Contains(value, "/") {
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			return "", fmt.Errorf("api key ip allowlist entry %q is not a valid address or CIDR block", value)
		}
		return network.String(), nil
	}
	address := net.ParseIP(value)
	if address == nil {
		return "", fmt.Errorf("api key ip allowlist entry %q is not a valid address or CIDR block", value)
	}
	return address.String(), nil
}

// IPAllowed reports whether a caller address satisfies an access key's
// allowlist. An empty or nil allowlist is unrestricted, mirroring the model
// allowlist semantics. The caller address is normalized the same way the
// stored entries are, including an IPv6 zone suffix.
func IPAllowed(allowed []string, caller string) bool {
	if len(allowed) == 0 {
		return true
	}
	caller = strings.TrimSpace(caller)
	if caller == "" {
		return false
	}
	if host, _, err := net.SplitHostPort(caller); err == nil {
		caller = host
	}
	// An IPv6 scoped address (fe80::1%eth0) must match an unscoped entry.
	if index := strings.IndexByte(caller, '%'); index >= 0 {
		caller = caller[:index]
	}
	address := net.ParseIP(caller)
	if address == nil {
		return false
	}
	for _, entry := range allowed {
		if strings.Contains(entry, "/") {
			_, network, err := net.ParseCIDR(entry)
			if err != nil {
				continue
			}
			if network.Contains(address) {
				return true
			}
			continue
		}
		if entry == address.String() {
			return true
		}
	}
	return false
}

// PatternsOverlap reports whether two model restrictions can authorize the
// same model name. It is used when a model-access key is nested under a
// management key to keep the narrower of the two: an access key must never
// widen its parent's restriction, but it may narrow it further. Normalized
// patterns are exact names or a single leading/trailing '*', so an overlap is
// either an exact match, a shared wildcard base, or one pattern being a
// concrete name inside the other's wildcard range.
func PatternsOverlap(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b || a == "*" || b == "*" {
		return true
	}
	aBase := strings.TrimSuffix(strings.TrimPrefix(a, "*"), "*")
	bBase := strings.TrimSuffix(strings.TrimPrefix(b, "*"), "*")
	if aBase == "" || bBase == "" {
		return true
	}
	// Each pattern's wildcard end is where the other's literal base must land
	// for the two to describe an overlapping set of names.
	if strings.HasSuffix(a, "*") && strings.HasPrefix(bBase, aBase) {
		return true
	}
	if strings.HasSuffix(b, "*") && strings.HasPrefix(aBase, bBase) {
		return true
	}
	if strings.HasPrefix(a, "*") && strings.HasSuffix(bBase, aBase) {
		return true
	}
	if strings.HasPrefix(b, "*") && strings.HasSuffix(aBase, bBase) {
		return true
	}
	return aBase == bBase
}

// NormalizeGroup trims an access key's usage group label. The label only ever
// appears as a grouping dimension in the usage records page, so it stays short
// and free of characters that would break CSV or HTML rendering.
func NormalizeGroup(group string) (string, error) {
	for _, r := range group {
		if (unicode.IsSpace(r) && r != ' ') || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", errors.New("api key group must not contain whitespace or control characters")
		}
	}
	group = strings.TrimSpace(group)
	if len(group) > MaxAccessGroupLength {
		return "", fmt.Errorf("api key group must be at most %d characters", MaxAccessGroupLength)
	}
	return group, nil
}

// NormalizeMaxConcurrency bounds the per-key concurrency ceiling. Zero means
// unlimited and is the default; the upper bound keeps a typo from turning into
// an effectively closed key that would only be noticed when traffic 429s.
func NormalizeMaxConcurrency(max int) int {
	if max <= 0 {
		return 0
	}
	if max > 1024 {
		return 1024
	}
	return max
}

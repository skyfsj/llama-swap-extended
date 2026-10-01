package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/ccswitch"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// handleAPIAuthSession provides the WebUI with a browser-native credential
// bootstrap. EventSource cannot attach an Authorization header, so a valid
// legacy or managed API key may be exchanged for a SameSite/HttpOnly session
// cookie. The store keeps a hash for authentication and a server-side copy for
// explicitly authorized integrations. This endpoint intentionally returns no
// key metadata or secret.
func (s *Server) handleAPIAuthSession(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	// Authentication state and Set-Cookie responses must not be cached by a
	// browser, proxy, or embedded WebView. The endpoint never returns a secret,
	// but a cached authenticated=true response is still misleading after logout.
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodDelete:
		http.SetCookie(w, &http.Cookie{
			Name:     auth.SessionCookieName,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Secure:   r.TLS != nil,
		})
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
		identity, valid, err := s.lookupIdentity(r.Context(), cfg, swaputil.ExtractAPIKey(r))
		if err != nil {
			swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
			return
		}
		if valid && !identity.Legacy && !identity.AllowManagementLogin {
			valid = false
		}
		// The configured flag is served from the in-memory key cache so a
		// transient storage failure cannot make the login prompt flicker.
		configured := s.authConfigured()
		w.Header().Set("Content-Type", "application/json")
		response := map[string]any{"authenticated": valid, "configured": configured}
		if valid {
			response["scopes"] = identityScopeList(identity)
			response["models"] = append([]string(nil), identity.Models...)
		}
		_ = json.NewEncoder(w).Encode(response)
		return
	case http.MethodPost:
		var request struct {
			Key string `json:"key"`
		}
		if err := decodeJSONBody(w, r, &request, 64<<10); err != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid authentication request")
			return
		}
		provided := strings.TrimSpace(request.Key)
		identity, valid, err := s.lookupIdentity(r.Context(), cfg, provided)
		if err != nil {
			swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
			return
		}
		if !valid {
			w.Header().Set("WWW-Authenticate", apiKeyAuthChallenge)
			swaputil.SendResponse(w, r, http.StatusUnauthorized, "unauthorized: invalid or missing API key")
			return
		}
		if !identity.Legacy && !identity.AllowManagementLogin {
			swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: this API key cannot sign in to the management panel")
			return
		}
		// Query escaping keeps legacy secrets containing cookie-special
		// characters round-trippable through net/http's cookie validator.
		http.SetCookie(w, &http.Cookie{
			Name:     auth.SessionCookieName,
			Value:    url.QueryEscape(provided),
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Secure:   r.TLS != nil,
		})
		if !identity.Legacy && s.store != nil {
			_ = s.store.TouchAPIKey(r.Context(), identity.ID, time.Now())
		}
		w.Header().Set("Content-Type", "application/json")
		// Keep the login response backward-compatible; clients can call the GET
		// session endpoint immediately afterwards to obtain scope/model metadata.
		_ = json.NewEncoder(w).Encode(map[string]bool{"authenticated": true})
		return
	default:
		swaputil.SendResponse(w, r, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func identityScopeList(identity auth.Identity) []string {
	if len(identity.Scopes) == 0 {
		return []string{}
	}
	result := make([]string, 0, len(identity.Scopes))
	for scope := range identity.Scopes {
		result = append(result, scope)
	}
	sort.Strings(result)
	return result
}

func (s *Server) seedStartupAPIKeys(cfg config.Config) error {
	if s == nil || s.store == nil {
		return nil
	}
	seed := func(id, name, secret string, scopes []string, models []string, expiresAt *time.Time, allowManagementLogin bool) error {
		secret = strings.TrimSpace(secret)
		if secret == "" {
			return nil
		}
		normalizedScopes, err := auth.NormalizeScopes(scopes)
		if err != nil {
			return err
		}
		if len(normalizedScopes) == 0 {
			normalizedScopes[auth.ScopeInference] = struct{}{}
		}
		normalizedModels, modelErr := auth.NormalizeModels(models)
		if modelErr != nil {
			return modelErr
		}
		scopeList := make([]string, 0, len(normalizedScopes))
		for scope := range normalizedScopes {
			scopeList = append(scopeList, scope)
		}
		sort.Strings(scopeList)
		id = strings.TrimSpace(id)
		if id == "" {
			id = "startup-" + auth.Hash(secret)[:16]
		}
		salt, digest, err := auth.GenerateSalted(secret)
		if err != nil {
			return err
		}
		if err := s.store.UpsertAPIKey(context.Background(), store.APIKeyRecord{
			ID: id, Name: strings.TrimSpace(name), KeyHash: digest,
			KeySalt: salt, KeySecret: secret, AllowManagementLogin: allowManagementLogin,
			Scopes: scopeList, Models: normalizedModels,
			ExpiresAt: expiresAt, CreatedAt: time.Now(),
		}); err != nil {
			return err
		}
		return nil
	}
	for _, entry := range cfg.StartupAPIKeys {
		if err := seed(entry.ID, entry.Name, entry.Key, entry.Scopes, entry.Models, entry.ExpiresAt, true); err != nil {
			return err
		}
	}
	// Scalar apiKeys are still accepted for backwards compatibility. Mirror
	// them into the durable key table as full-management keys so the CC Switch
	// importer can resolve their secret just like structured startup keys.
	for _, secret := range cfg.RequiredAPIKeys {
		if err := seed(store.KeyFingerprint(secret), "Configured API key", secret, auth.ManagementScopes(), nil, nil, true); err != nil {
			return err
		}
	}
	return nil
}

type createKeyRequest struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	ParentID string   `json:"parentId"`
	Scopes   []string `json:"scopes"`
	Models   []string `json:"models"`
	// AllowedIPs is the access-key caller allowlist. A nil or empty list is
	// unrestricted.
	AllowedIPs []string `json:"allowedIps"`
	// MaxConcurrency caps simultaneous requests for an access key; 0 is
	// unlimited.
	MaxConcurrency int        `json:"maxConcurrency"`
	Group          string     `json:"group"`
	ExpiresAt      *time.Time `json:"expiresAt"`

	// AllowManagementLogin is retained for management keys. An access key can
	// never sign in to the control plane, so it is ignored for that kind.
	AllowManagementLogin *bool `json:"allowManagementLogin"`
}

const maxAPIKeyJSONBody = 64 << 10

const managementRecoveryKeyRequired = "at least one non-expiring management API key must remain"

type updateKeyRequest struct {
	Name                 *string      `json:"name"`
	Scopes               *[]string    `json:"scopes"`
	Models               *[]string    `json:"models"`
	AllowedIPs           *[]string    `json:"allowedIps"`
	MaxConcurrency       *int         `json:"maxConcurrency"`
	Group                *string      `json:"group"`
	AllowManagementLogin *bool        `json:"allowManagementLogin"`
	ExpiresAt            optionalTime `json:"expiresAt"`
}

// hasManagementRecoveryKey keeps the durable key store from becoming an
// authentication dead end. A legacy config key is always full-management;
// managed keys need a permanent browser login and keys-admin to repair every
// other key through the control plane.
func hasManagementRecoveryKey(cfg config.Config, keys []store.APIKeyRecord) bool {
	if len(cfg.RequiredAPIKeys) > 0 {
		return true
	}
	for _, key := range keys {
		if key.RevokedAt != nil || key.ExpiresAt != nil || !key.AllowManagementLogin {
			continue
		}
		scopes, err := auth.NormalizeScopes(key.Scopes)
		if err != nil {
			continue
		}
		if _, ok := scopes[auth.ScopeKeysAdmin]; ok {
			return true
		}
	}
	return false
}

// optionalTime distinguishes an omitted field from an explicit JSON null.
// That lets the management API clear an expiry without introducing a second
// endpoint or treating a malformed timestamp as a request to clear it.
type optionalTime struct {
	Set   bool
	Value *time.Time
}

func (o *optionalTime) UnmarshalJSON(data []byte) error {
	o.Set = true
	if strings.TrimSpace(string(data)) == "null" {
		o.Value = nil
		return nil
	}
	var value time.Time
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	o.Value = &value
	return nil
}

func keyID() string {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "key-" + time.Now().UTC().Format("20060102150405.000000000")
	}
	return "key-" + hex.EncodeToString(random[:])
}

func (s *Server) handleAPIKeys(w http.ResponseWriter, r *http.Request) {
	includeRevoked := r.URL.Query().Get("include_revoked") == "true"
	keys, err := s.store.ListAPIKeys(r.Context(), includeRevoked)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	// The UI renders management keys with their nested access keys, so it needs
	// the live per-key in-flight count to show concurrency against the ceiling.
	for i := range keys {
		keys[i].KeyHash = ""
		keys[i].ActiveRequests = accessKeyLimits.count(keys[i].ID)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": keys})
}

func (s *Server) handleAPICreateKey(w http.ResponseWriter, r *http.Request) {
	var req createKeyRequest
	if err := decodeJSONBody(w, r, &req, maxAPIKeyJSONBody); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid key request")
		return
	}
	kind, kindErr := normalizeRequestedKeyKind(req.Kind)
	if kindErr != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, kindErr.Error())
		return
	}
	name, err := auth.NormalizeKeyName(req.Name)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if req.ExpiresAt != nil && !req.ExpiresAt.After(time.Now()) {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "expiresAt must be in the future")
		return
	}
	models, err := auth.NormalizeModels(req.Models)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	allowedIPs, err := auth.NormalizeAccessIPs(req.AllowedIPs)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	group, err := auth.NormalizeGroup(req.Group)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	keys, err := s.store.ListAPIKeys(r.Context(), false)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	var parent *store.APIKeyRecord
	var scopeList []string
	record := store.APIKeyRecord{
		Name:           name,
		Kind:           kind,
		ParentID:       strings.TrimSpace(req.ParentID),
		Models:         models,
		AllowedIPs:     allowedIPs,
		MaxConcurrency: auth.NormalizeMaxConcurrency(req.MaxConcurrency),
		Group:          group,
		ExpiresAt:      req.ExpiresAt,
		CreatedAt:      time.Now(),
	}
	if kind == auth.KeyKindAccess {
		parent, err = resolveAccessKeyParent(keys, record.ParentID)
		if err != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
			return
		}
		// Access keys inherit the management key's scopes but keep only the
		// inference one (see accessKeyIdentity): they are API credentials.
		record.AllowManagementLogin = false
		record.Scopes = []string{auth.ScopeInference}
		if err := intersectModelsError(record.Models, parent.Models); err != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
			return
		}
		record.Models = intersectModels(record.Models, parent.Models)
	} else {
		scopes, scopeErr := auth.NormalizeScopes(req.Scopes)
		if scopeErr != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, scopeErr.Error())
			return
		}
		if len(scopes) == 0 {
			scopes[auth.ScopeInference] = struct{}{}
		}
		record.AllowManagementLogin = true
		if req.AllowManagementLogin != nil {
			record.AllowManagementLogin = *req.AllowManagementLogin
			if record.AllowManagementLogin {
				scopes, scopeErr = auth.NormalizeScopes(auth.ManagementScopes())
				if scopeErr != nil {
					swaputil.SendResponse(w, r, http.StatusInternalServerError, scopeErr.Error())
					return
				}
			}
		}
		scopeList = make([]string, 0, len(scopes))
		for scope := range scopes {
			scopeList = append(scopeList, scope)
		}
		sort.Strings(scopeList)
		record.Scopes = scopeList
	}
	if !hasManagementRecoveryKey(s.currentConfig(), append(keys, record)) {
		swaputil.SendResponse(w, r, http.StatusConflict, managementRecoveryKeyRequired)
		return
	}
	secret, err := auth.Generate()
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	salt, digest, err := auth.GenerateSalted(secret)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	record.ID = keyID()
	record.KeyHash = digest
	record.KeySalt = salt
	record.KeySecret = secret
	if err := s.store.UpsertAPIKey(r.Context(), record); err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	s.refreshKeyCache()
	record.KeyHash = ""
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"key": secret, "record": record, "warning": "the key is shown only once"})
}

// normalizeRequestedKeyKind maps the request's kind onto the canonical kinds.
// An omitted kind keeps the management default, which preserves the API
// contract for clients created before the split.
func normalizeRequestedKeyKind(kind string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", auth.KeyKindManagement:
		return auth.KeyKindManagement, nil
	case auth.KeyKindAccess:
		return auth.KeyKindAccess, nil
	default:
		return "", errors.New("unknown api key kind: " + kind)
	}
}

// resolveAccessKeyParent finds the management key an access key is being
// nested under. The parent must exist, still be active, and must expire after
// the child: an access key that outlives its parent would silently extend
// access past the point the operator intended to end it.
func resolveAccessKeyParent(keys []store.APIKeyRecord, parentID string) (*store.APIKeyRecord, error) {
	parentID = strings.TrimSpace(parentID)
	if parentID == "" {
		return nil, errors.New("access keys require a parent management key")
	}
	for i := range keys {
		if keys[i].ID != parentID {
			continue
		}
		if keys[i].RevokedAt != nil {
			return nil, errors.New("parent management key is revoked")
		}
		if keys[i].Kind != auth.KeyKindManagement {
			return nil, errors.New("access keys can only be nested under a management key")
		}
		return &keys[i], nil
	}
	return nil, errors.New("parent management key not found")
}

// cascadeRevoked marks every access key nested under a revoked key as revoked
// in the in-memory copy used for the recovery-key check, matching what
// Store.RevokeAPIKeyCascade persists.
func cascadeRevoked(keys []store.APIKeyRecord, parentID string, when *time.Time) {
	for i := range keys {
		if keys[i].RevokedAt != nil || keys[i].ParentID != parentID {
			continue
		}
		keys[i].RevokedAt = when
		cascadeRevoked(keys, keys[i].ID, when)
	}
}

// intersectModels narrows an access key's model allowlist to what its
// management parent also allows. An empty list means "unrestricted" throughout
// the authorization model, so the result is deliberately never an empty slice:
// when no access-key pattern overlaps the parent's, the parent's own list is
// returned as the narrowest representation that still fails closed relative to
// the parent. Writing a restriction that narrows to nothing is rejected at the
// control-plane boundary (see intersectModelsError), so this branch is the
// defensive path for rows written before that check existed.
func intersectModels(access, management []string) []string {
	if len(management) == 0 {
		return append([]string(nil), access...)
	}
	if len(access) == 0 {
		return append([]string(nil), management...)
	}
	out := make([]string, 0, len(access))
	for _, pattern := range access {
		for _, allowed := range management {
			if auth.PatternsOverlap(pattern, allowed) {
				out = append(out, pattern)
				break
			}
		}
	}
	if len(out) == 0 {
		return append([]string(nil), management...)
	}
	return out
}

// intersectModelsError rejects a model allowlist that no longer overlaps the
// management key's. Without this check the stored intersection would silently
// fall back to the parent's whole list, widening a credential the operator
// explicitly narrowed — so the conflict is surfaced instead of guessed at.
func intersectModelsError(access, management []string) error {
	if len(management) == 0 || len(access) == 0 {
		return nil
	}
	for _, pattern := range access {
		for _, allowed := range management {
			if auth.PatternsOverlap(pattern, allowed) {
				return nil
			}
		}
	}
	return errors.New("access key models must overlap the management key's model restrictions")
}

func (s *Server) handleAPIRevokeKey(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "key id is required")
		return
	}
	keys, err := s.store.ListAPIKeys(r.Context(), true)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	for i := range keys {
		if keys[i].ID != id || keys[i].RevokedAt != nil {
			continue
		}
		now := time.Now()
		keys[i].RevokedAt = &now
		// Revoking a management key also revokes the access keys nested under
		// it: leaving them active would extend the parent's access past the
		// moment the operator ended it.
		cascadeRevoked(keys, id, &now)
		if !hasManagementRecoveryKey(s.currentConfig(), keys) {
			swaputil.SendResponse(w, r, http.StatusConflict, managementRecoveryKeyRequired)
			return
		}
		break
	}
	if err := s.store.RevokeAPIKeyCascade(r.Context(), id, time.Now()); err != nil {
		if errors.Is(err, store.ErrAPIKeyNotFound) {
			swaputil.SendResponse(w, r, http.StatusNotFound, err.Error())
			return
		}
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	s.refreshKeyCache()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAPIUpdateKey(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "key id is required")
		return
	}
	keys, err := s.store.ListAPIKeys(r.Context(), true)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	var record *store.APIKeyRecord
	for i := range keys {
		if keys[i].ID == id {
			record = &keys[i]
			break
		}
	}
	if record == nil {
		swaputil.SendResponse(w, r, http.StatusNotFound, "key not found")
		return
	}
	var req updateKeyRequest
	if err := decodeJSONBody(w, r, &req, maxAPIKeyJSONBody); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid key update request")
		return
	}
	if req.Name != nil {
		name, nameErr := auth.NormalizeKeyName(*req.Name)
		if nameErr != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, nameErr.Error())
			return
		}
		record.Name = name
	}
	// The kind is fixed at creation: an access key cannot promote itself into a
	// management key (or the reverse) through a metadata update. Re-keying a
	// credential is what rotate is for.
	if record.Kind == auth.KeyKindAccess {
		parent, parentErr := resolveAccessKeyParent(keys, record.ParentID)
		if parentErr != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, parentErr.Error())
			return
		}
		if req.Models != nil {
			models, modelErr := auth.NormalizeModels(*req.Models)
			if modelErr != nil {
				swaputil.SendResponse(w, r, http.StatusBadRequest, modelErr.Error())
				return
			}
			if overlapErr := intersectModelsError(models, parent.Models); overlapErr != nil {
				swaputil.SendResponse(w, r, http.StatusBadRequest, overlapErr.Error())
				return
			}
			record.Models = intersectModels(models, parent.Models)
		}
		if req.Scopes != nil {
			// Access keys are inference-only by construction; accepting a scope
			// change here would reopen a control-plane hole through a nested
			// credential.
			swaputil.SendResponse(w, r, http.StatusBadRequest, "access key scopes cannot be modified")
			return
		}
		if req.AllowManagementLogin != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, "access keys cannot sign in to the management panel")
			return
		}
	} else if req.Models != nil {
		models, modelErr := auth.NormalizeModels(*req.Models)
		if modelErr != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, modelErr.Error())
			return
		}
		record.Models = models
		if err := s.cascadeNarrowAccessKeys(r.Context(), keys, *record); err != nil {
			swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if req.Scopes != nil && record.Kind != auth.KeyKindAccess {
		scopes, scopeErr := auth.NormalizeScopes(*req.Scopes)
		if scopeErr != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, scopeErr.Error())
			return
		}
		if len(scopes) == 0 {
			scopes[auth.ScopeInference] = struct{}{}
		}
		record.Scopes = record.Scopes[:0]
		for scope := range scopes {
			record.Scopes = append(record.Scopes, scope)
		}
		sort.Strings(record.Scopes)
	}
	if req.AllowedIPs != nil {
		allowedIPs, ipErr := auth.NormalizeAccessIPs(*req.AllowedIPs)
		if ipErr != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, ipErr.Error())
			return
		}
		record.AllowedIPs = allowedIPs
	}
	if req.MaxConcurrency != nil {
		record.MaxConcurrency = auth.NormalizeMaxConcurrency(*req.MaxConcurrency)
	}
	if req.Group != nil {
		group, groupErr := auth.NormalizeGroup(*req.Group)
		if groupErr != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, groupErr.Error())
			return
		}
		record.Group = group
	}
	if req.AllowManagementLogin != nil && record.Kind != auth.KeyKindAccess {
		record.AllowManagementLogin = *req.AllowManagementLogin
		if record.AllowManagementLogin {
			scopes, scopeErr := auth.NormalizeScopes(auth.ManagementScopes())
			if scopeErr != nil {
				swaputil.SendResponse(w, r, http.StatusInternalServerError, scopeErr.Error())
				return
			}
			record.Scopes = record.Scopes[:0]
			for scope := range scopes {
				record.Scopes = append(record.Scopes, scope)
			}
			sort.Strings(record.Scopes)
		}
	}
	if req.ExpiresAt.Set {
		if req.ExpiresAt.Value != nil && !req.ExpiresAt.Value.After(time.Now()) {
			swaputil.SendResponse(w, r, http.StatusBadRequest, "expiresAt must be in the future")
			return
		}
		record.ExpiresAt = req.ExpiresAt.Value
	}
	// A nested access key must never outlive the management key that owns it.
	if record.Kind == auth.KeyKindAccess && record.ExpiresAt != nil {
		if parent := findAPIKey(keys, record.ParentID); parent != nil && parent.ExpiresAt != nil && record.ExpiresAt.After(*parent.ExpiresAt) {
			swaputil.SendResponse(w, r, http.StatusBadRequest, "access keys cannot outlive their management key")
			return
		}
	}
	if !hasManagementRecoveryKey(s.currentConfig(), keys) {
		swaputil.SendResponse(w, r, http.StatusConflict, managementRecoveryKeyRequired)
		return
	}
	if err := s.store.UpsertAPIKey(r.Context(), *record); err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	s.refreshKeyCache()
	record.KeyHash = ""
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(record)
}

func findAPIKey(keys []store.APIKeyRecord, id string) *store.APIKeyRecord {
	for i := range keys {
		if keys[i].ID == id {
			return &keys[i]
		}
	}
	return nil
}

// cascadeNarrowAccessKeys re-applies a narrowed management model allowlist to
// every active access key nested under it and persists the result, so a model
// the operator just revoked from the parent stops being reachable through a
// child without a manual second edit.
func (s *Server) cascadeNarrowAccessKeys(ctx context.Context, keys []store.APIKeyRecord, parent store.APIKeyRecord) error {
	for i := range keys {
		if keys[i].Kind != auth.KeyKindAccess || keys[i].ParentID != parent.ID || keys[i].RevokedAt != nil {
			continue
		}
		narrowed := intersectModels(keys[i].Models, parent.Models)
		if len(narrowed) == len(keys[i].Models) {
			continue
		}
		keys[i].Models = narrowed
		if err := s.store.UpsertAPIKey(ctx, keys[i]); err != nil {
			return fmt.Errorf("narrow nested access key %q: %w", keys[i].ID, err)
		}
	}
	return nil
}

func (s *Server) handleAPIRotateKey(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	keys, err := s.store.ListAPIKeys(r.Context(), false)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	var old store.APIKeyRecord
	for _, candidate := range keys {
		if candidate.ID == id {
			old = candidate
			break
		}
	}
	if old.ID == "" {
		swaputil.SendResponse(w, r, http.StatusNotFound, "key not found")
		return
	}
	secret, err := auth.Generate()
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	salt, digest, err := auth.GenerateSalted(secret)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	old.ID = keyID()
	old.KeyHash = digest
	old.KeySalt = salt
	old.KeySecret = secret
	old.CreatedAt = time.Now()
	old.LastUsedAt = nil
	old.RevokedAt = nil
	if err := s.store.RotateAPIKey(r.Context(), id, old, time.Now()); err != nil {
		if errors.Is(err, store.ErrAPIKeyNotFound) {
			swaputil.SendResponse(w, r, http.StatusNotFound, err.Error())
			return
		}
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	s.refreshKeyCache()
	old.KeyHash = ""
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"key": secret, "record": old, "warning": "the key is shown only once"})
}

func (s *Server) handleAPIAudit(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	auditQuery, parseErr := parseAuditQuery(r)
	if parseErr != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, parseErr.Error())
		return
	}
	identity := identityFromContext(r.Context())
	if auditQuery.KeyID != "" && !keyUsageAllowedForIdentity(identity, auditQuery.KeyID) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key cannot read another key's audit records")
		return
	}
	if auditQuery.Model != "" {
		if !modelAllowedForIdentity(cfg, identity, auditQuery.Model) {
			swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+auditQuery.Model)
			return
		}
		if canonical, ok := cfg.RealModelName(auditQuery.Model); ok {
			auditQuery.Model = canonical
		}
	} else if models, restricted := allowedCanonicalModels(cfg, identity); restricted {
		// A restricted key with no matching configured models sees an empty
		// result, never the unfiltered audit table. An empty IN list cannot be
		// represented by AuditQuery, so finish the request explicitly here.
		if len(models) == 0 {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(store.AuditConversationPage{
				Data: []store.AuditConversation{}, Page: auditQuery.Page, Limit: auditQuery.Limit,
			})
			return
		}
		auditQuery.Models = models
	}
	conversations, err := s.store.ListAuditConversationsPage(r.Context(), auditQuery)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(conversations)
}

// handleAPIAuditConversation returns one persisted raw conversation. The list
// endpoint intentionally remains bounded for dashboards; this resource form
// lets clients fetch a specific request/response pair without relying on a
// large list page while applying the same model-scope check as audit queries.
func (s *Server) handleAPIAuditConversation(w http.ResponseWriter, r *http.Request) {
	conversation, ok := s.loadAuthorizedAuditConversation(w, r, strings.TrimSpace(r.PathValue("id")))
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(conversation)
}

// handleAPIAuditConversationBody streams one stored body of a conversation.
//
// The detail view publishes only a reference and a size, so a transcript is
// fetched when the operator actually opens it instead of making one response
// carry every turn's payload — with attachments that is tens of megabytes, and
// Go's []byte JSON encoding adds another third on top. Serving the blob file
// itself keeps server memory flat no matter how large the transcript is.
func (s *Server) handleAPIAuditConversationBody(w http.ResponseWriter, r *http.Request) {
	which := strings.ToLower(strings.TrimSpace(r.PathValue("which")))
	if which != "request" && which != "response" {
		swaputil.SendResponse(w, r, http.StatusNotFound, "unknown body selector")
		return
	}
	conversation, ok := s.loadAuthorizedAuditConversation(w, r, strings.TrimSpace(r.PathValue("id")))
	if !ok {
		return
	}
	ref := conversation.RequestBodyRef
	if which == "response" {
		ref = conversation.ResponseBodyRef
	}
	if ref == "" {
		// Not externalized: an in-memory store already carried the body inside
		// the detail response, and a capture that stored no body has nothing.
		swaputil.SendResponse(w, r, http.StatusNotFound, "body is not stored separately")
		return
	}
	file, _, err := s.store.OpenBlob(ref)
	if err != nil {
		if errors.Is(err, store.ErrBlobNotFound) {
			swaputil.SendResponse(w, r, http.StatusNotFound, "body is unavailable")
			return
		}
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	defer file.Close()
	// Bodies are captured verbatim: JSON, an SSE transcript, or a proxied binary
	// response all land here, so the bytes are served untyped rather than guessed
	// at, and never sniffed into something the browser would try to render.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", time.Time{}, file)
}

// loadAuthorizedAuditConversation resolves a conversation by numeric activity ID
// (the Web UI path) or raw ID (administrative clients) and applies the read
// authorization every audit surface shares. It writes its own error response and
// reports false when the caller must stop, so the detail view and the body
// stream cannot drift apart on who may read a record.
func (s *Server) loadAuthorizedAuditConversation(w http.ResponseWriter, r *http.Request, id string) (store.AuditConversation, bool) {
	cfg := s.currentConfig()
	if id == "" {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "conversation id is required")
		return store.AuditConversation{}, false
	}
	var conversation store.AuditConversation
	var found bool
	var err error
	if activityID, parseErr := strconv.Atoi(id); parseErr == nil && activityID > 0 {
		conversation, found, err = s.store.GetAuditConversationByActivityID(r.Context(), activityID)
	} else {
		// Keep the raw string lookup for existing administrative API clients. The
		// Web UI always uses the numeric activity ID path above.
		conversation, found, err = s.store.GetAuditConversation(r.Context(), id)
	}
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return store.AuditConversation{}, false
	}
	if !found {
		swaputil.SendResponse(w, r, http.StatusNotFound, "conversation not found")
		return store.AuditConversation{}, false
	}
	identity := identityFromContext(r.Context())
	if !auditConversationAllowed(identity, conversation) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key cannot read another key's audit record")
		return store.AuditConversation{}, false
	}
	if !modelAllowedForIdentity(cfg, identity, conversation.Model) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+conversation.Model)
		return store.AuditConversation{}, false
	}
	return conversation, true
}

func (s *Server) handleAPIDeleteAudit(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "conversation id is required")
		return
	}
	conversation, found, err := s.store.GetAuditConversation(r.Context(), id)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		swaputil.SendResponse(w, r, http.StatusNotFound, "conversation not found")
		return
	}
	identity := identityFromContext(r.Context())
	if !auditConversationAllowed(identity, conversation) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key cannot delete another key's audit record")
		return
	}
	if !identity.Legacy && len(identity.Models) > 0 {
		if !modelAllowedForIdentity(cfg, identity, conversation.Model) {
			swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+conversation.Model)
			return
		}
	}
	if err := s.store.DeleteAuditConversation(r.Context(), id); err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	s.signalAuditDirty()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAPIClearAudit(w http.ResponseWriter, r *http.Request) {
	if identity := identityFromContext(r.Context()); !identity.Legacy && len(identity.Models) > 0 {
		// Clearing is intentionally all-or-nothing. A model-scoped key cannot
		// express a safe partial delete through this endpoint, so reject it
		// rather than silently clearing another model's conversations.
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: clearing all audit conversations requires an unrestricted key")
		return
	}
	if err := s.store.ClearAuditConversations(r.Context()); err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	s.signalAuditDirty()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAPIUsage(w http.ResponseWriter, r *http.Request) {
	s.handleAPIUsageForKey(w, r, "")
}

// handleAPIKeyUsage is the resource-oriented alias for /api/usage?key_id=... .
// Keeping the key in the path makes per-key dashboards less error-prone while
// the query form remains available for combined model/session/time filters.
func (s *Server) handleAPIKeyUsage(w http.ResponseWriter, r *http.Request) {
	keyID := strings.TrimSpace(r.PathValue("id"))
	if keyID == "" {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "key id is required")
		return
	}
	s.handleAPIUsageForKey(w, r, keyID)
}

func (s *Server) handleAPIUsageForKey(w http.ResponseWriter, r *http.Request, pathKey string) {
	cfg := s.currentConfig()
	filter := store.ActivityFilter{KeyID: strings.TrimSpace(pathKey), SessionID: strings.TrimSpace(r.URL.Query().Get("session_id"))}
	if filter.KeyID == "" {
		filter.KeyID = strings.TrimSpace(r.URL.Query().Get("key_id"))
	}
	identity := identityFromContext(r.Context())
	if filter.KeyID != "" && !keyUsageAllowedForIdentity(identity, filter.KeyID) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key cannot read another key's usage")
		return
	}
	if model := strings.TrimSpace(r.URL.Query().Get("model")); model != "" {
		if !modelAllowedForIdentity(cfg, identity, model) {
			swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+model)
			return
		}
		if canonical, ok := cfg.RealModelName(model); ok {
			model = canonical
		}
		filter.Models = []string{model}
	} else if models, restricted := allowedCanonicalModels(cfg, identity); restricted {
		if len(models) == 0 {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(store.UsageSummary{ByModel: []store.ModelUsage{}})
			return
		}
		filter.Models = models
	}
	if start := strings.TrimSpace(r.URL.Query().Get("start")); start != "" {
		parsed, err := time.Parse(time.RFC3339, start)
		if err != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid start timestamp, use RFC3339 format")
			return
		}
		filter.Start = parsed
	}
	if end := strings.TrimSpace(r.URL.Query().Get("end")); end != "" {
		parsed, err := time.Parse(time.RFC3339, end)
		if err != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid end timestamp, use RFC3339 format")
			return
		}
		filter.End = parsed
	}
	if !filter.Start.IsZero() && !filter.End.IsZero() && filter.Start.After(filter.End) {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "start timestamp must not be after end timestamp")
		return
	}
	summary, err := s.store.UsageSummary(r.Context(), filter)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(summary)
}

// keyUsageAllowedForIdentity prevents an ordinary audit-read credential from
// using the filter as a cross-key usage oracle. A managed key can inspect its
// own usage, while the explicit keys-admin scope is required for an operator
// dashboard that aggregates or drills into other credentials. Legacy keys and
// the anonymous default-allow mode retain their historical unrestricted view.
func keyUsageAllowedForIdentity(identity auth.Identity, keyID string) bool {
	keyID = strings.TrimSpace(keyID)
	if keyID == "" || identity.Legacy || identity.Has(auth.ScopeKeysAdmin) {
		return true
	}
	return strings.TrimSpace(identity.ID) == keyID
}

func auditConversationAllowed(identity auth.Identity, conversation store.AuditConversation) bool {
	if identity.Legacy || identity.Has(auth.ScopeKeysAdmin) {
		return true
	}
	keyID := strings.TrimSpace(conversation.KeyID)
	return keyID == "" || keyID == strings.TrimSpace(identity.ID)
}

func parseAuditQuery(r *http.Request) (store.AuditQuery, error) {
	query := r.URL.Query()
	result := store.AuditQuery{
		KeyID: strings.TrimSpace(query.Get("key_id")), Model: strings.TrimSpace(query.Get("model")),
		SessionID: strings.TrimSpace(query.Get("session_id")), Limit: 25, Page: 1,
	}
	for name, target := range map[string]*time.Time{"start": &result.Start, "end": &result.End} {
		value := strings.TrimSpace(query.Get(name))
		if value == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return store.AuditQuery{}, fmt.Errorf("invalid %s timestamp, use RFC3339 format", name)
		}
		*target = parsed
	}
	if !result.Start.IsZero() && !result.End.IsZero() && result.Start.After(result.End) {
		return store.AuditQuery{}, errors.New("start timestamp must not be after end timestamp")
	}
	if value := strings.TrimSpace(query.Get("limit")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 500 {
			return store.AuditQuery{}, errors.New("limit must be between 1 and 500")
		}
		result.Limit = parsed
	}
	if value := strings.TrimSpace(query.Get("offset")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			return store.AuditQuery{}, errors.New("offset must be a non-negative integer")
		}
		result.Offset = parsed
	}
	if value := strings.TrimSpace(query.Get("page")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			return store.AuditQuery{}, errors.New("page must be at least 1")
		}
		result.Page = parsed
	}
	result.Sort = strings.TrimSpace(query.Get("sort"))
	result.Order = strings.TrimSpace(query.Get("order"))
	return result, nil
}

type deeplinkRequest struct {
	App         string `json:"app"`
	Endpoint    string `json:"endpoint"`
	Name        string `json:"name"`
	Model       string `json:"model"`
	HaikuModel  string `json:"haikuModel"`
	SonnetModel string `json:"sonnetModel"`
	OpusModel   string `json:"opusModel"`
	IncludeKey  bool   `json:"includeKey"`
	CreateKey   bool   `json:"createKey"`
	KeyID       string `json:"keyId"`
	Key         string `json:"key"`
}

// CC Switch requires a non-empty API key field even when llama-swap has no
// authentication configured. This is only a compatibility placeholder;
// authenticated sessions still use their real session key below.
const defaultCCSwitchAPIKey = "llama-swap"

type ccSwitchKeyOption struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Models []string `json:"models,omitempty"`
}

func (s *Server) handleAPICCOptions(w http.ResponseWriter, r *http.Request) {
	identity := identityFromContext(r.Context())
	keys, err := s.store.ListAPIKeys(r.Context(), false)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	canSelectOthers := identity.Legacy || identity.Has(auth.ScopeKeysAdmin)
	now := time.Now()
	options := make([]ccSwitchKeyOption, 0, len(keys))
	for _, key := range keys {
		if !canSelectOthers && strings.TrimSpace(identity.ID) != key.ID {
			continue
		}
		if key.ExpiresAt != nil && !key.ExpiresAt.After(now) {
			continue
		}
		hasInference := false
		for _, scope := range key.Scopes {
			if scope == auth.ScopeInference {
				hasInference = true
				break
			}
		}
		if !hasInference {
			continue
		}
		secret, found, secretErr := s.store.GetAPIKeySecret(r.Context(), key.ID)
		if secretErr != nil {
			swaputil.SendResponse(w, r, http.StatusInternalServerError, secretErr.Error())
			return
		}
		if !found || strings.TrimSpace(secret) == "" {
			continue
		}
		options = append(options, ccSwitchKeyOption{ID: key.ID, Name: key.Name, Models: key.Models})
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data":         options,
		"currentKeyId": identity.ID,
	})
}

func (s *Server) handleAPICCLinks(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	var req deeplinkRequest
	// The WebUI only sends the browser-visible endpoint and the target app is
	// selected locally. Keep an empty-body request valid for simple integrations
	// while retaining the structured request for existing API clients.
	if r.Body != nil && r.Body != http.NoBody {
		if err := decodeJSONBody(w, r, &req, maxAPIKeyJSONBody); err != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid deep link request")
			return
		}
	}
	if strings.TrimSpace(req.Endpoint) == "" {
		req.Endpoint = requestEndpoint(r)
	}
	if strings.TrimSpace(req.Name) == "" {
		req.Name = "llama-swap"
	}
	req.App = strings.TrimSpace(req.App)
	req.KeyID = strings.TrimSpace(req.KeyID)
	identity := identityFromContext(r.Context())
	if req.CreateKey && (req.KeyID != "" || strings.TrimSpace(req.Key) != "") {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "createKey cannot be combined with keyId or key")
		return
	}
	if keyID := req.KeyID; keyID != "" {
		// A different key can be selected for import only by a keys-admin
		// identity. The normal key list still exposes metadata only; the original
		// value is resolved inside this dedicated server-side operation.
		if !identity.Legacy && strings.TrimSpace(identity.ID) != keyID && !identity.Has(auth.ScopeKeysAdmin) {
			swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: selecting another API key requires keys-admin scope")
			return
		}
		provided := strings.TrimSpace(req.Key)
		if provided == "" {
			var found bool
			var err error
			provided, found, err = s.store.GetAPIKeySecret(r.Context(), keyID)
			if err != nil {
				swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
				return
			}
			if !found {
				swaputil.SendResponse(w, r, http.StatusNotFound, "selected API key was not found")
				return
			}
			if strings.TrimSpace(provided) == "" {
				swaputil.SendResponse(w, r, http.StatusConflict, "selected API key predates secret retention; rotate it before importing")
				return
			}
		}
		selected, valid, err := s.lookupIdentity(r.Context(), cfg, provided)
		if err != nil {
			swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
			return
		}
		if !valid || strings.TrimSpace(selected.ID) != keyID {
			swaputil.SendResponse(w, r, http.StatusBadRequest, "selected API key is invalid or does not match its ID")
			return
		}
		if !selected.Has(auth.ScopeInference) {
			swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: selected API key has no inference scope")
			return
		}
		identity = selected
		req.Key = provided
		req.IncludeKey = true
	}
	for _, requested := range []string{req.Model, req.HaikuModel, req.SonnetModel, req.OpusModel} {
		requested = strings.TrimSpace(requested)
		if requested != "" && !modelAllowedForIdentity(cfg, identity, requested) {
			swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+requested)
			return
		}
	}
	if strings.TrimSpace(req.Model) == "" && !identity.Legacy && len(identity.Models) > 0 {
		// A model-scoped key cannot create a link whose target is left
		// unspecified; doing so would produce an unrestricted client profile.
		swaputil.SendResponse(w, r, http.StatusBadRequest, "model is required for a model-scoped API key")
		return
	}
	createdKeyID := ""
	if req.CreateKey {
		if req.App == "" || strings.TrimSpace(req.Model) == "" {
			swaputil.SendResponse(w, r, http.StatusBadRequest, "app and model are required when createKey is true")
			return
		}
		// Validate every provider field before creating a durable key, so malformed
		// requests cannot leave unused credentials behind.
		if _, err := ccswitch.BuildProvider(ccswitch.Provider{
			App: req.App, Name: req.Name, Endpoint: req.Endpoint, Model: req.Model,
			HaikuModel: req.HaikuModel, SonnetModel: req.SonnetModel, OpusModel: req.OpusModel,
		}); err != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
			return
		}
		secret, err := auth.Generate()
		if err != nil {
			swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
			return
		}
		salt, digest, err := auth.GenerateSalted(secret)
		if err != nil {
			swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
			return
		}
		models := make([]string, 0, 4)
		seenModels := map[string]struct{}{}
		for _, model := range []string{req.Model, req.HaikuModel, req.SonnetModel, req.OpusModel} {
			model = strings.TrimSpace(model)
			if model == "" {
				continue
			}
			if _, seen := seenModels[model]; seen {
				continue
			}
			seenModels[model] = struct{}{}
			models = append(models, model)
		}
		createdKeyID = keyID()
		if err := s.store.UpsertAPIKey(r.Context(), store.APIKeyRecord{
			ID: createdKeyID, Name: req.Name, KeyHash: digest, KeySalt: salt, KeySecret: secret,
			Scopes: []string{auth.ScopeInference}, Models: models, AllowManagementLogin: false,
			CreatedAt: time.Now(),
		}); err != nil {
			swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
			return
		}
		s.refreshKeyCache()
		req.Key = secret
		req.IncludeKey = true
	}
	if req.IncludeKey && strings.TrimSpace(req.Key) == "" {
		// The browser session is backed by the current API key, so the UI can
		// pass it through to CC Switch without ever reading or storing the raw
		// secret in JavaScript.
		req.Key = swaputil.ExtractAPIKey(r)
		if strings.TrimSpace(req.Key) == "" {
			req.Key = defaultCCSwitchAPIKey
		}
	}
	if req.App != "" {
		link, err := ccswitch.BuildProvider(ccswitch.Provider{
			App: req.App, Name: req.Name, Endpoint: req.Endpoint, Model: req.Model,
			HaikuModel: req.HaikuModel, SonnetModel: req.SonnetModel, OpusModel: req.OpusModel,
			APIKey: req.Key, IncludeKey: req.IncludeKey,
		})
		if err != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if req.IncludeKey {
			w.Header().Set("Referrer-Policy", "no-referrer")
		}
		w.Header().Set("Content-Type", "application/json")
		payload := map[string]string{"app": req.App, "link": link}
		if createdKeyID != "" {
			payload["keyId"] = createdKeyID
		}
		_ = json.NewEncoder(w).Encode(payload)
		return
	}
	codex, claude, err := ccswitch.BuildPair(req.Endpoint, req.Name, req.Model)
	if req.IncludeKey {
		if strings.TrimSpace(req.Key) == "" {
			swaputil.SendResponse(w, r, http.StatusBadRequest, "key is required when includeKey is true")
			return
		}
		codex, err = ccswitch.BuildProvider(ccswitch.Provider{App: "codex", Name: req.Name, Endpoint: req.Endpoint, Model: req.Model, APIKey: req.Key, IncludeKey: true})
		if err == nil {
			claude, err = ccswitch.BuildProvider(ccswitch.Provider{App: "claude", Name: req.Name, Endpoint: req.Endpoint, Model: req.Model, APIKey: req.Key, IncludeKey: true})
		}
	}
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if req.IncludeKey {
		w.Header().Set("Referrer-Policy", "no-referrer")
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"codex": codex, "claude": claude})
}

func requestEndpoint(r *http.Request) string {
	if r == nil || strings.TrimSpace(r.Host) == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/v1"
}

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/dop251/goja"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/gateway"
)

// Placeholders the CC Switch-style script may embed in its request fields.
// They are replaced after the script evaluated, so the secret never appears in
// the script source (and therefore never in a goja error message).
const (
	quotaScriptBaseURLPlaceholder = "{{baseUrl}}"
	quotaScriptAPIKeyPlaceholder  = "{{apiKey}}"
)

// quotaScriptRequest is the HTTP call a CC Switch-style script declares.
type quotaScriptRequest struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    any               `json:"body,omitempty"`
}

// quotaScriptResult is the snapshot a script returns. The windows/extras shape
// is this project's native one; the flat fields below mirror CC Switch's
// custom usage script so an existing script works unchanged.
type quotaScriptResult struct {
	Windows []quotaScriptWindow `json:"windows"`
	Extras  []quotaScriptExtra  `json:"extras"`

	IsValid        *bool    `json:"isValid"`
	InvalidMessage string   `json:"invalidMessage"`
	PlanName       string   `json:"planName"`
	Extra          string   `json:"extra"`
	Total          *float64 `json:"total"`
	Used           *float64 `json:"used"`
	Remaining      *float64 `json:"remaining"`
	Unit           string   `json:"unit"`
}

type quotaScriptWindow struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Percent   *float64 `json:"percent"`
	Remaining *float64 `json:"remaining"`
	Used      *float64 `json:"used"`
	Limit     *float64 `json:"limit"`
	Status    string   `json:"status"`
	ResetsAt  string   `json:"resetsAt"`
}

type quotaScriptExtra struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Value  any    `json:"value"`
	Suffix string `json:"suffix"`
}

// quotaScriptOutcome is one script run: the snapshot plus the HTTP call it
// made (nil for the function form), which the editor's test panel shows.
type quotaScriptOutcome struct {
	windows        []quotaWindowSnapshot
	extras         []quotaExtraSnapshot
	invalidMessage string
	request        *quotaScriptRequest
}

// runQuotaScript executes one usage script for a credential. The script is
// evaluated as a single JavaScript expression; two shapes are supported:
//
//	({ request: {url, method, headers, body}, extractor: (response) => ... })
//	async (ctx) => ({ windows: [...], extras: [...] })
//
// The former runs the HTTP call on the Go side (so proxy, timeouts and body
// limits match the declarative mapping), the latter calls the synchronous
// fetch helper as many times as it needs.
func (s *Server) runQuotaScript(ctx context.Context, target credentialTarget, script string) (outcome quotaScriptOutcome, err error) {
	quota := target.provider.Quota
	if quota == nil {
		return quotaScriptOutcome{}, fmt.Errorf("provider %s has no quota configuration", target.providerID)
	}
	if strings.TrimSpace(target.cred.Value) == "" {
		return quotaScriptOutcome{}, fmt.Errorf("credential has no usable value (unset environment variable)")
	}
	if strings.TrimSpace(script) == "" {
		return quotaScriptOutcome{}, fmt.Errorf("quota script is empty")
	}

	apiKey := target.cred.Value
	baseURL := strings.TrimRight(quotaBaseURL(target.provider), "/")

	// One deadline covers the whole run: the VM interrupt bounds the script's
	// own CPU time, and this context bounds every fetch it makes, so a script
	// issuing several slow requests cannot multiply the budget.
	runCtx, cancel := context.WithTimeout(ctx, quota.Timeout())
	defer cancel()
	// Credential material must never leave through an error message: a script
	// may have interpolated the key into a URL that the transport echoes back.
	defer func() {
		if err != nil {
			err = fmt.Errorf("%s", quotaScriptErrorMessage(err, apiKey))
		}
	}()

	vm := goja.New()
	fetch := func(call goja.FunctionCall) goja.Value {
		url := strings.TrimSpace(call.Argument(0).String())
		options := map[string]any{}
		if arg := call.Argument(1); arg != nil && !goja.IsUndefined(arg) && !goja.IsNull(arg) {
			if exported, ok := arg.Export().(map[string]any); ok {
				options = exported
			}
		}
		response, err := s.performQuotaScriptRequest(runCtx, target, url, options)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(response)
	}
	if err := vm.Set("quotaCtx", map[string]any{
		"baseUrl":    baseURL,
		"apiKey":     apiKey,
		"providerID": target.providerID,
		"fetch":      fetch,
	}); err != nil {
		return quotaScriptOutcome{}, err
	}

	timeout := quota.Timeout()
	timer := time.AfterFunc(timeout, func() { vm.Interrupt("quota script timed out") })
	defer func() {
		timer.Stop()
		vm.ClearInterrupt()
	}()

	// The wrapper keeps the script on its first line (so goja's line numbers
	// still point at the user's code) and calls a function result from inside
	// the top-level evaluation: that is what drains the microtask queue, so an
	// async function's promise is already settled when RunString returns. The
	// terminator sits on its own line so a trailing `// comment` in the script
	// cannot swallow it.
	wrapped := "(function(){const __quota = " + script + "\n;" +
		"if (typeof __quota === 'function') { return __quota(quotaCtx); }" +
		"return __quota;})()"
	value, err := vm.RunString(wrapped)
	if err != nil {
		return quotaScriptOutcome{}, fmt.Errorf("quota script failed: %s", quotaScriptErrorMessage(err, apiKey))
	}
	value, err = settleQuotaScriptValue(value)
	if err != nil {
		return quotaScriptOutcome{}, err
	}

	resultValue := value
	if result := value.ToObject(vm); result != nil {
		requestValue := quotaScriptProperty(result, "request")
		extractorValue := quotaScriptProperty(result, "extractor")
		hasRequest := requestValue != nil && !goja.IsUndefined(requestValue) && !goja.IsNull(requestValue)
		hasExtractor := extractorValue != nil && !goja.IsUndefined(extractorValue) && !goja.IsNull(extractorValue)
		switch {
		case hasRequest && hasExtractor:
			request, err := exportQuotaScriptRequest(vm, requestValue)
			if err != nil {
				return quotaScriptOutcome{}, err
			}
			request.URL = substituteQuotaScriptSecret(request.URL, baseURL, apiKey)
			for name, header := range request.Headers {
				request.Headers[name] = substituteQuotaScriptSecret(header, baseURL, apiKey)
			}
			request.Body = substituteQuotaScriptBody(request.Body, baseURL, apiKey)
			outcome.request = &request

			response, err := s.performQuotaScriptRequest(runCtx, target, request.URL, map[string]any{
				"method":  request.Method,
				"headers": request.Headers,
				"body":    request.Body,
			})
			if err != nil {
				return quotaScriptOutcome{}, err
			}
			parsed, ok := response["json"]
			if !ok || parsed == nil {
				return quotaScriptOutcome{}, fmt.Errorf("quota endpoint did not return JSON (status %v)", response["status"])
			}
			extractor, ok := goja.AssertFunction(extractorValue)
			if !ok {
				return quotaScriptOutcome{}, fmt.Errorf("quota script: extractor must be a function")
			}
			mapped, err := extractor(goja.Undefined(), vm.ToValue(parsed))
			if err != nil {
				return quotaScriptOutcome{}, fmt.Errorf("quota script extractor failed: %s", quotaScriptErrorMessage(err, apiKey))
			}
			resultValue = mapped
		case hasRequest:
			return quotaScriptOutcome{}, fmt.Errorf("quota script: request requires an extractor function")
		case hasExtractor:
			return quotaScriptOutcome{}, fmt.Errorf("quota script: extractor requires a request")
		}
	}

	windows, extras, invalidMessage, err := quotaScriptSnapshot(quota, resultValue)
	if err != nil {
		return quotaScriptOutcome{}, err
	}
	outcome.windows = windows
	outcome.extras = extras
	outcome.invalidMessage = invalidMessage
	return outcome, nil
}

// settleQuotaScriptValue resolves a promise result. Only a settled promise is
// accepted: the fetch helper is synchronous, so a promise still pending here
// can never settle.
func settleQuotaScriptValue(value goja.Value) (goja.Value, error) {
	promise, ok := value.Export().(*goja.Promise)
	if !ok {
		return value, nil
	}
	switch promise.State() {
	case goja.PromiseStateFulfilled:
		return promise.Result(), nil
	case goja.PromiseStateRejected:
		return nil, fmt.Errorf("quota script rejected: %s", quotaScriptRejectionMessage(promise.Result()))
	default:
		return nil, fmt.Errorf("quota script returned a pending promise; the fetch helper is synchronous, so nothing can settle it")
	}
}

func quotaScriptRejectionMessage(value goja.Value) string {
	if value == nil || goja.IsUndefined(value) || goja.IsNull(value) {
		return "rejected"
	}
	if object, ok := value.(*goja.Object); ok {
		if message := object.Get("message"); message != nil && !goja.IsUndefined(message) {
			return message.String()
		}
	}
	if err, ok := value.Export().(error); ok {
		return err.Error()
	}
	return value.String()
}

// quotaScriptErrorMessage strips the credential from an error: a script may
// have interpolated it into a URL that then shows up in the message.
func quotaScriptErrorMessage(err error, apiKey string) string {
	message := err.Error()
	if apiKey != "" {
		message = strings.ReplaceAll(message, apiKey, "[redacted]")
	}
	return message
}

// quotaScriptProperty reads one property, mapping a missing property to
// undefined: this goja build returns a nil Value for it.
func quotaScriptString(value goja.Value) string {
	if value == nil || goja.IsUndefined(value) || goja.IsNull(value) {
		return ""
	}
	return value.String()
}

func quotaScriptProperty(object *goja.Object, name string) goja.Value {
	value := object.Get(name)
	if value == nil {
		return goja.Undefined()
	}
	return value
}

func exportQuotaScriptRequest(vm *goja.Runtime, value goja.Value) (quotaScriptRequest, error) {
	if value == nil || goja.IsUndefined(value) || goja.IsNull(value) {
		return quotaScriptRequest{}, fmt.Errorf("quota script: request must be an object")
	}
	object := value.ToObject(vm)
	if object == nil {
		return quotaScriptRequest{}, fmt.Errorf("quota script: request must be an object")
	}
	request := quotaScriptRequest{Method: http.MethodGet}
	request.URL = strings.TrimSpace(quotaScriptString(quotaScriptProperty(object, "url")))
	if request.URL == "" {
		return quotaScriptRequest{}, fmt.Errorf("quota script: request.url is required")
	}
	if method := strings.TrimSpace(quotaScriptString(quotaScriptProperty(object, "method"))); method != "" {
		request.Method = strings.ToUpper(method)
	}
	switch request.Method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return quotaScriptRequest{}, fmt.Errorf("quota script: request.method %q is not supported", request.Method)
	}
	if headersValue := quotaScriptProperty(object, "headers"); headersValue != nil && !goja.IsUndefined(headersValue) && !goja.IsNull(headersValue) {
		headersObject := headersValue.ToObject(vm)
		if headersObject != nil {
			request.Headers = make(map[string]string)
			for _, name := range headersObject.Keys() {
				request.Headers[name] = quotaScriptString(quotaScriptProperty(headersObject, name))
			}
		}
	}
	if bodyValue := quotaScriptProperty(object, "body"); bodyValue != nil && !goja.IsUndefined(bodyValue) && !goja.IsNull(bodyValue) {
		request.Body = bodyValue.Export()
	}
	return request, nil
}

func substituteQuotaScriptSecret(value, baseURL, apiKey string) string {
	value = strings.ReplaceAll(value, quotaScriptBaseURLPlaceholder, baseURL)
	return strings.ReplaceAll(value, quotaScriptAPIKeyPlaceholder, apiKey)
}

// substituteQuotaScriptBody rewrites placeholders inside a request body. A
// structured body is serialized first, so nested strings are covered too.
func substituteQuotaScriptBody(body any, baseURL, apiKey string) any {
	if body == nil {
		return nil
	}
	if text, ok := body.(string); ok {
		return substituteQuotaScriptSecret(text, baseURL, apiKey)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return body
	}
	return json.RawMessage(substituteQuotaScriptSecret(string(encoded), baseURL, apiKey))
}

// performQuotaScriptRequest runs one script HTTP call with the credential's
// proxy and the provider's timeouts. It returns the response in the shape the
// JS side sees: status/ok/url/json/text.
func (s *Server) performQuotaScriptRequest(ctx context.Context, target credentialTarget, url string, options map[string]any) (map[string]any, error) {
	quota := target.provider.Quota
	if quota == nil {
		return nil, fmt.Errorf("provider %s has no quota configuration", target.providerID)
	}
	if strings.TrimSpace(url) == "" {
		return nil, fmt.Errorf("quota script request has no URL")
	}

	method := http.MethodGet
	if raw, ok := options["method"].(string); ok && strings.TrimSpace(raw) != "" {
		method = strings.ToUpper(strings.TrimSpace(raw))
	}

	headers := map[string]string{}
	switch typed := options["headers"].(type) {
	case map[string]string:
		for name, value := range typed {
			headers[name] = value
		}
	case map[string]any:
		for name, value := range typed {
			headers[name] = fmt.Sprint(value)
		}
	}

	var body io.Reader
	switch typed := options["body"].(type) {
	case nil:
	case string:
		body = strings.NewReader(typed)
	case []byte:
		body = bytes.NewReader(typed)
	case json.RawMessage:
		body = bytes.NewReader(typed)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return nil, fmt.Errorf("quota script request body is not serializable: %w", err)
		}
		body = bytes.NewReader(encoded)
	}

	reqCtx, cancel := context.WithTimeout(ctx, quota.Timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("invalid quota script URL: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	transport, err := gateway.BuildTransport(
		target.cred.Proxy,
		target.provider.Timeouts.Connect,
		target.provider.Timeouts.KeepAlive,
		target.provider.Timeouts.ResponseHeader,
		target.provider.Timeouts.TLSHandshake,
		target.provider.Timeouts.IdleConn,
	)
	if err != nil {
		return nil, err
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("quota request failed: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, quotaResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("reading quota response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := strings.TrimSpace(string(responseBody))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, fmt.Errorf("quota endpoint returned status %d: %s", resp.StatusCode, snippet)
	}

	text := string(responseBody)
	var parsed any
	if err := json.Unmarshal(responseBody, &parsed); err != nil {
		parsed = nil
	}
	return map[string]any{
		"status": resp.StatusCode,
		"ok":     resp.StatusCode == http.StatusOK,
		"url":    url,
		"json":   parsed,
		"text":   text,
	}, nil
}

// quotaScriptSnapshot normalizes a script result. The native windows/extras
// shape wins; otherwise CC Switch's flat usage fields are mapped onto a
// single amount window plus balance rows.
func quotaScriptSnapshot(quota *config.QuotaConfig, value goja.Value) ([]quotaWindowSnapshot, []quotaExtraSnapshot, string, error) {
	var result quotaScriptResult
	if value != nil && !goja.IsUndefined(value) && !goja.IsNull(value) {
		encoded, err := json.Marshal(value.Export())
		if err != nil {
			return nil, nil, "", fmt.Errorf("quota script result is not JSON-serializable: %w", err)
		}
		if err := json.Unmarshal(encoded, &result); err != nil {
			return nil, nil, "", fmt.Errorf("quota script must return an object with windows/extras or usage fields")
		}
	}

	divideBy := quota.DivideBy
	windows := make([]quotaWindowSnapshot, 0, len(result.Windows))
	for _, window := range result.Windows {
		id := strings.TrimSpace(window.ID)
		if id == "" {
			return nil, nil, "", fmt.Errorf("quota script: window needs an id")
		}
		label := window.Label
		if label == "" {
			label = id
		}
		snapshot := quotaWindowSnapshot{ID: id, Label: label, Status: window.Status, ResetsAt: window.ResetsAt}
		var percent int
		switch {
		case window.Limit != nil && *window.Limit > 0:
			used := 0.0
			if window.Used != nil {
				used = *window.Used
			}
			snapshot.Used = scaleScriptAmount(used, divideBy)
			snapshot.Limit = scaleScriptAmount(*window.Limit, divideBy)
			percent = int(math.Round(used / *window.Limit * 100))
		case window.Percent != nil:
			percent = int(*window.Percent)
		case window.Remaining != nil:
			percent = 100 - int(*window.Remaining)
		default:
			return nil, nil, "", fmt.Errorf("quota script: window %q needs percent, remaining, or used+limit", id)
		}
		snapshot.Percent = clampQuotaPercent(percent)
		snapshot.RemainingPercent = 100 - snapshot.Percent
		windows = append(windows, snapshot)
	}

	extras := make([]quotaExtraSnapshot, 0, len(result.Extras))
	for _, extra := range result.Extras {
		id := strings.TrimSpace(extra.ID)
		if id == "" {
			return nil, nil, "", fmt.Errorf("quota script: extra needs an id")
		}
		label := extra.Label
		if label == "" {
			label = id
		}
		extras = append(extras, quotaExtraSnapshot{
			ID:     id,
			Label:  label,
			Value:  config.ScaleQuotaValue(extra.Value, divideBy),
			Suffix: extra.Suffix,
		})
	}

	if len(windows) == 0 && len(extras) == 0 {
		if result.IsValid == nil && result.Total == nil && result.Used == nil &&
			result.Remaining == nil && result.PlanName == "" && result.Extra == "" {
			return nil, nil, "", fmt.Errorf("quota script returned neither windows/extras nor usage fields")
		}
		unit := strings.TrimSpace(result.Unit)
		suffix := ""
		if unit != "" {
			suffix = " " + unit
		}
		if result.Total != nil && *result.Total > 0 {
			used := 0.0
			if result.Used != nil {
				used = *result.Used
			}
			percent := int(math.Round(used / *result.Total * 100))
			windows = append(windows, quotaWindowSnapshot{
				ID:               "quota",
				Label:            "quota",
				Percent:          clampQuotaPercent(percent),
				RemainingPercent: 100 - clampQuotaPercent(percent),
				Used:             scaleScriptAmount(used, divideBy),
				Limit:            scaleScriptAmount(*result.Total, divideBy),
			})
		}
		if result.Remaining != nil {
			extras = append(extras, quotaExtraSnapshot{
				ID: "remaining", Label: "remaining", Suffix: suffix,
				Value: config.ScaleQuotaValue(*result.Remaining, divideBy),
			})
		}
		if result.Total != nil && len(windows) == 0 {
			extras = append(extras, quotaExtraSnapshot{
				ID: "total", Label: "total", Suffix: suffix,
				Value: config.ScaleQuotaValue(*result.Total, divideBy),
			})
		}
		if result.Used != nil && len(windows) == 0 {
			extras = append(extras, quotaExtraSnapshot{
				ID: "used", Label: "used", Suffix: suffix,
				Value: config.ScaleQuotaValue(*result.Used, divideBy),
			})
		}
		if strings.TrimSpace(result.PlanName) != "" {
			extras = append(extras, quotaExtraSnapshot{ID: "plan", Label: "plan", Value: result.PlanName})
		}
		if strings.TrimSpace(result.Extra) != "" {
			extras = append(extras, quotaExtraSnapshot{ID: "note", Label: "note", Value: result.Extra})
		}
	}

	invalidMessage := strings.TrimSpace(result.InvalidMessage)
	if result.IsValid != nil && !*result.IsValid && invalidMessage == "" {
		invalidMessage = "credential reported as invalid"
	}
	return windows, extras, invalidMessage, nil
}

func scaleScriptAmount(value float64, divideBy float64) float64 {
	if divideBy <= 0 {
		return value
	}
	scaled, _ := config.ScaleQuotaValue(value, divideBy).(float64)
	return scaled
}

func clampQuotaPercent(percent int) int {
	switch {
	case percent < 0:
		return 0
	case percent > 100:
		return 100
	default:
		return percent
	}
}

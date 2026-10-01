package extensions

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// A script declares its configurable settings as a plain object next to its
// hooks. The declaration doubles as the form the Extensions UI renders, so a
// user never has to open the code editor to change an API key or a switch.
//
//	export const settings = {
//	  apiKey: { type: "string", label: "API Key", secret: true, required: true },
//	  maxResults: { type: "number", label: "Results", default: 5, min: 1, max: 20 },
//	  locale: { type: "select", label: "Language", options: ["zh", "en"], default: "zh" }
//	};
//
// Values live in manifest.config and reach the script as ctx.config. Keys the
// user never touched fall back to the declared default at runtime, so the
// manifest only ever stores explicit choices.

// Setting components. The names match the settings center renderer so the UI
// can reuse SettingsFieldEditor without a translation table.
const (
	CompText     = "text"
	CompNumber   = "number"
	CompSwitch   = "switch"
	CompSelect   = "select"
	CompPassword = "password"
	CompTextarea = "textarea"
	CompList     = "list"
	CompMap      = "map"
	CompJSON     = "json"
)

// settingTypeComponents maps the types a script writes to the components the
// renderer understands. Authors write JavaScript-flavoured names
// ("string", "number", "boolean"); the UI sees the settings-center vocabulary.
var settingTypeComponents = map[string]string{
	"string":   CompText,
	"number":   CompNumber,
	"boolean":  CompSwitch,
	"select":   CompSelect,
	"password": CompPassword,
	"textarea": CompTextarea,
	"list":     CompList,
	"map":      CompMap,
	"json":     CompJSON,
}

// SettingField is one declared setting in its wire form.
type SettingField struct {
	Key               string            `json:"key"`
	Label             string            `json:"label"`
	Hint              string            `json:"hint,omitempty"`
	LabelTranslations map[string]string `json:"labelTranslations,omitempty"`
	HintTranslations  map[string]string `json:"hintTranslations,omitempty"`
	Component         string            `json:"component"`
	Options           []string          `json:"options,omitempty"`
	Default           any               `json:"default,omitempty"`
	Required          bool              `json:"required,omitempty"`
	Secret            bool              `json:"secret,omitempty"`
	Min               *float64          `json:"min,omitempty"`
	Max               *float64          `json:"max,omitempty"`
	Section           string            `json:"section,omitempty"`
}

// SettingDiagnostic reports one rejected setting value. Path is the setting
// key, which lets the UI anchor the message under the offending input.
type SettingDiagnostic struct {
	Path     string `json:"path"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// settingProperties are the declaration keys a script may use. Anything else is
// a typo and fails the build rather than being silently ignored.
var settingProperties = map[string]bool{
	"type": true, "label": true, "hint": true, "options": true, "default": true,
	"required": true, "secret": true, "min": true, "max": true, "section": true,
}

// settingDeclaredTypes are the types a script may write in a setting
// declaration. Anything else is a typo and fails the build.
var settingDeclaredTypes = map[string]bool{
	"string": true, "number": true, "boolean": true, "select": true,
	"password": true, "textarea": true, "list": true, "map": true, "json": true,
}

// parseSettingsSpec turns the settings export into fields. The input arrives
// from goja as map[string]any, so every value is untyped JSON.
func parseSettingsSpec(raw any) ([]SettingField, error) {
	if raw == nil {
		return nil, nil
	}
	declared, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("settings must be an object")
	}
	keys := make([]string, 0, len(declared))
	for key := range declared {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fields := make([]SettingField, 0, len(keys))
	for _, key := range keys {
		if err := validSettingKey(key); err != nil {
			return nil, err
		}
		field, err := parseSettingField(key, declared[key])
		if err != nil {
			return nil, err
		}
		fields = append(fields, field)
	}
	return fields, nil
}

func validSettingKey(key string) error {
	if len(key) > 40 {
		return fmt.Errorf("setting key %q is too long", key)
	}
	for index, char := range key {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char == '_':
		case char >= '0' && char <= '9' && index > 0:
		default:
			return fmt.Errorf("setting key %q must be a JavaScript identifier", key)
		}
	}
	return nil
}

func parseSettingField(key string, raw any) (SettingField, error) {
	field := SettingField{Key: key}
	declaration, ok := raw.(map[string]any)
	if !ok {
		return field, fmt.Errorf("setting %q must be an object", key)
	}
	for name := range declaration {
		if !settingProperties[name] {
			return field, fmt.Errorf("setting %q has unknown property %q", key, name)
		}
	}
	component, _ := declaration["type"].(string)
	if !settingDeclaredTypes[component] {
		return field, fmt.Errorf("setting %q has unknown type %q", key, component)
	}
	field.Component = settingTypeComponents[component]
	field.Secret, _ = declaration["secret"].(bool)
	if field.Secret && (field.Component == CompText || field.Component == CompTextarea) {
		// A secret string is a password input regardless of which text flavour
		// the author picked.
		field.Component = CompPassword
	}
	label, labelTexts, err := settingLocalizedText(key, "label", declaration["label"])
	if err != nil {
		return field, err
	}
	field.Label, field.LabelTranslations = label, labelTexts
	if field.Label == "" {
		field.Label = key
	}
	hint, hintTexts, err := settingLocalizedText(key, "hint", declaration["hint"])
	if err != nil {
		return field, err
	}
	field.Hint, field.HintTranslations = hint, hintTexts
	if section, ok := declaration["section"].(string); ok {
		field.Section = section
	}
	field.Required, _ = declaration["required"].(bool)
	if value, present := declaration["default"]; present {
		field.Default = value
	}
	if rawMin, present := declaration["min"]; present {
		value, err := settingNumber(key, "min", rawMin)
		if err != nil {
			return field, err
		}
		field.Min = &value
	}
	if rawMax, present := declaration["max"]; present {
		value, err := settingNumber(key, "max", rawMax)
		if err != nil {
			return field, err
		}
		field.Max = &value
	}
	if component == CompSelect {
		options, ok := declaration["options"].([]any)
		if !ok || len(options) == 0 {
			return field, fmt.Errorf("setting %q needs a non-empty options list", key)
		}
		for _, option := range options {
			text, ok := option.(string)
			if !ok {
				return field, fmt.Errorf("setting %q options must be strings", key)
			}
			field.Options = append(field.Options, text)
		}
		if field.Default != nil {
			if !containsString(field.Options, fmt.Sprint(field.Default)) {
				return field, fmt.Errorf("setting %q default %v is not one of its options", key, field.Default)
			}
		}
	}
	if field.Required && field.Default != nil {
		return field, fmt.Errorf("setting %q cannot be required and have a default", key)
	}
	return field, nil
}

func settingNumber(key, property string, raw any) (float64, error) {
	value, ok := numericSettingValue(raw)
	if !ok {
		return 0, fmt.Errorf("setting %q property %q must be a number", key, property)
	}
	return value, nil
}

// numericSettingValue accepts every numeric representation a config value can
// carry: JSON decoding yields float64, but a manifest re-read from YAML
// produces int/int64, so the type switch must cover both.
func numericSettingValue(raw any) (float64, bool) {
	switch value := raw.(type) {
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return 0, false
		}
		return value, true
	case float32:
		return float64(value), true
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	case json.Number:
		parsed, err := value.Float64()
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// validateSettingsValues checks the user's config against the declaration. Only
// declared keys are checked; keys the script never declared are left alone,
// because the manifest is the script's own namespace.
func validateSettingsValues(fields []SettingField, config map[string]any) []SettingDiagnostic {
	if len(fields) == 0 {
		return nil
	}
	var diagnostics []SettingDiagnostic
	for _, field := range fields {
		value, present := config[field.Key]
		if !present || value == nil {
			if field.Required {
				diagnostics = append(diagnostics, SettingDiagnostic{Path: field.Key, Severity: "error", Message: "is required"})
			}
			continue
		}
		if message := settingValueMessage(field, value); message != "" {
			diagnostics = append(diagnostics, SettingDiagnostic{Path: field.Key, Severity: "error", Message: message})
		}
	}
	return diagnostics
}

func settingValueMessage(field SettingField, value any) string {
	switch field.Component {
	case CompNumber:
		number, ok := numericSettingValue(value)
		if !ok {
			return "must be a number"
		}
		if field.Min != nil && number < *field.Min {
			return fmt.Sprintf("must be at least %s", trimNumber(*field.Min))
		}
		if field.Max != nil && number > *field.Max {
			return fmt.Sprintf("must be at most %s", trimNumber(*field.Max))
		}
	case CompSwitch:
		if _, ok := value.(bool); !ok {
			return "must be true or false"
		}
	case CompSelect:
		text, ok := value.(string)
		if !ok {
			return "must be a string"
		}
		if !containsString(field.Options, text) {
			return fmt.Sprintf("must be one of %s", strings.Join(field.Options, ", "))
		}
	case CompList:
		items, ok := value.([]any)
		if !ok {
			return "must be a list"
		}
		for _, item := range items {
			if _, ok := item.(string); !ok {
				return "must be a list of strings"
			}
		}
	case CompMap:
		entries, ok := value.(map[string]any)
		if !ok {
			return "must be an object"
		}
		for key, entry := range entries {
			if key == "" {
				return "keys must not be empty"
			}
			if _, ok := entry.(string); !ok && !settingIsScalar(entry) {
				return fmt.Sprintf("value for %q must be a string, number or boolean", key)
			}
		}
	default:
		if field.Component == CompJSON {
			if _, ok := value.(map[string]any); !ok {
				if _, isList := value.([]any); !isList {
					return "must be an object or a list"
				}
			}
			return ""
		}
		if _, ok := value.(string); !ok {
			return "must be a string"
		}
	}
	return ""
}

func settingIsScalar(value any) bool {
	switch value.(type) {
	case string, float64, bool:
		return true
	default:
		return false
	}
}

// applySettingDefaults returns the config the script sees: its own values with
// declared defaults filled in for the keys it left unset. The result is never
// nil, because a script that reads ctx.config.apiKey on an extension with no
// configuration at all would otherwise get a TypeError instead of undefined.
func applySettingDefaults(fields []SettingField, config map[string]any) map[string]any {
	merged := make(map[string]any, len(config)+len(fields))
	for key, value := range config {
		merged[key] = value
	}
	for _, field := range fields {
		if field.Default == nil {
			continue
		}
		if value, present := merged[field.Key]; !present || value == nil {
			merged[field.Key] = field.Default
		}
	}
	return merged
}

// seedRequiredSettings returns values that satisfy the declaration for the keys
// a user still has to fill in. Installing a preset must not be blocked by a
// credential the user will supply afterwards, and the alternative — dropping
// required — would let a typo reach a running extension.
func seedRequiredSettings(fields []SettingField) map[string]any {
	seed := map[string]any{}
	for _, field := range fields {
		if !field.Required {
			continue
		}
		switch field.Component {
		case CompNumber:
			seed[field.Key] = 0.0
		case CompSwitch:
			seed[field.Key] = false
		case CompSelect:
			if len(field.Options) > 0 {
				seed[field.Key] = field.Options[0]
			}
		case CompList:
			seed[field.Key] = []any{}
		case CompMap:
			seed[field.Key] = map[string]any{}
		case CompJSON:
			seed[field.Key] = map[string]any{}
		default:
			seed[field.Key] = ""
		}
	}
	return seed
}

// SettingsValidationError reports rejected setting values and carries the
// diagnostics so the API can answer with a payload the UI anchors to inputs.
type SettingsValidationError struct {
	Diagnostics []SettingDiagnostic
}

func (e *SettingsValidationError) Error() string {
	parts := make([]string, 0, len(e.Diagnostics))
	for _, diagnostic := range e.Diagnostics {
		parts = append(parts, fmt.Sprintf("%s %s", diagnostic.Path, diagnostic.Message))
	}
	if len(parts) == 0 {
		return "invalid extension settings"
	}
	return "extension settings: " + strings.Join(parts, "; ")
}

func settingsValidationError(diagnostics []SettingDiagnostic) error {
	if len(diagnostics) == 0 {
		return nil
	}
	return &SettingsValidationError{Diagnostics: diagnostics}
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

// trimNumber renders a bound without a trailing ".0" so messages read as
// "at least 5" rather than "at least 5.000000".
func trimNumber(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// supportedSettingLocales mirrors the UI locale set: an extension declares
// text per console language, and the renderer picks the matching entry.
var supportedSettingLocales = map[string]bool{"en": true, "zh-CN": true, "zh-TW": true}

// settingLocalizedText accepts a plain string or a { locale: string } object
// and returns the canonical string plus the per-locale map. The canonical
// value prefers en so manifests and keyless API readers stay stable.
func settingLocalizedText(key, property string, raw any) (string, map[string]string, error) {
	switch value := raw.(type) {
	case nil:
		return "", nil, nil
	case string:
		return value, nil, nil
	case map[string]any:
		if len(value) == 0 {
			return "", nil, fmt.Errorf("setting %q property %q must not be an empty language object", key, property)
		}
		translations := make(map[string]string, len(value))
		for language, rawText := range value {
			if !supportedSettingLocales[language] {
				return "", nil, fmt.Errorf("setting %q property %q has unsupported language %q", key, property, language)
			}
			text, ok := rawText.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return "", nil, fmt.Errorf("setting %q property %q language %q must be a non-empty string", key, property, language)
			}
			translations[language] = text
		}
		canonical := ""
		for _, language := range []string{"en", "zh-CN", "zh-TW"} {
			if text, ok := translations[language]; ok {
				canonical = text
				break
			}
		}
		if canonical == "" {
			return "", nil, fmt.Errorf("setting %q property %q has no text", key, property)
		}
		return canonical, translations, nil
	default:
		return "", nil, fmt.Errorf("setting %q property %q must be a string or language object", key, property)
	}
}

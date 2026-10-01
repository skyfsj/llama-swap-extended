// Package spec is the single source of truth for the llama-swap
// configuration: structure, defaults, constraints, sensitive attributes,
// restart gating and UI metadata all live in one declarative Go tree. The
// JSON Schema shipped to editors, the /api/settings/schema HTTP metadata and
// the unknown-field detector are all derived from this tree, so no consumer
// maintains a private copy. Drift tests in this package keep the tree aligned
// with the Go types in internal/config.
package spec

// Kind is the value shape of a configuration field.
type Kind int

const (
	// KString is a plain string scalar.
	KString Kind = iota
	// KBool is a boolean scalar.
	KBool
	// KInt is an integer scalar.
	KInt
	// KDuration is a time.Duration written in YAML as "5s", "24h" or
	// nanoseconds.
	KDuration
	// KStringList is a sequence of strings.
	KStringList
	// KStringMap is a mapping of string to string.
	KStringMap
	// KIntMap is a mapping of string to integer.
	KIntMap
	// KStringListMap is a mapping of string to a list of strings.
	KStringListMap
	// KAnyMap is a free-form mapping (metadata, macros, setParams).
	KAnyMap
	// KAny accepts any YAML value.
	KAny
	// KObject is a nested struct described by Children.
	KObject
	// KEntityMap is an identity-keyed map[string]T described by Template.
	// A key present in two sources is a hard merge error.
	KEntityMap
	// KObjectList is a sequence of objects described by Template.
	KObjectList
	// KScalarOrObject is a sequence whose items may each be a scalar or an
	// object (apiKeys).
	KScalarOrObject
)

// UI components understood by the settings center renderer.
const (
	CompText     = "text"
	CompNumber   = "number"
	CompSwitch   = "switch"
	CompSelect   = "select"
	CompTextarea = "textarea"
	CompPassword = "password"
	CompDuration = "duration"
	CompList     = "list"
	CompMap      = "map"
	CompJSON     = "json"
	CompEditor   = "editor"
)

// Option is one entry of a select field. Options come from this Spec or from
// a server-registered provider (Provider); the schema never carries URLs.
type Option struct {
	Value    string `json:"value"`
	Label    string `json:"label"`
	Provider string `json:"provider,omitempty"`
}

// UI carries the presentation metadata for one field.
type UI struct {
	Section   string   `json:"section"`
	Label     string   `json:"label"`
	Hint      string   `json:"hint,omitempty"`
	Component string   `json:"component"`
	Options   []Option `json:"options,omitempty"`
	// Editor names a dedicated settings-center editor (models, routing,
	// ...) for Component == CompEditor.
	Editor string `json:"editor,omitempty"`
	// Provider is a server-registered dynamic option provider ID.
	Provider string `json:"provider,omitempty"`
	Order    int    `json:"order"`
}

// Field is one node of the configuration specification tree.
type Field struct {
	// Name is the YAML key. For entity templates and list item templates it
	// is the placeholder used in paths ("{id}").
	Name string
	// Kind is the value shape.
	Kind Kind
	// Desc is the human description shown in hints and generated schemas.
	Desc string
	// Default is the effective default value for UI display. The load
	// pipeline remains the runtime authority.
	Default any
	// Sensitive marks fields whose values are redacted from every public
	// surface; only "configured" status is reported.
	Sensitive bool
	// Legacy marks input-normalization-only syntax (top-level groups/matrix)
	// that the loader still accepts but the settings center offers to migrate.
	Legacy bool
	// Restart marks daemon-restart-gated fields: a committed change is
	// accepted as desired state and shown as pending-restart.
	Restart bool
	// HiddenUI keeps the field out of the simple settings list while still
	// exposing it to search, raw mode and the schema.
	HiddenUI bool
	// Advanced demotes the field to the advanced area of its section.
	Advanced bool
	// Required makes the key mandatory in validation.
	Required bool
	// Enum restricts string values.
	Enum []string
	// Min/Max bound numeric values when set.
	Min *int64
	Max *int64
	// MinItems bounds sequence length when > 0.
	MinItems int
	// Item describes list elements (KStringList uses nil).
	Item *Field
	// Children describes object members (KObject).
	Children []*Field
	// Template describes entity map values / object list items.
	Template *Field
	// KeyPattern is a regex an entity map key must match, when non-empty.
	KeyPattern string
	// KeyLen is the entity key length bound, when > 0.
	KeyLen int
	// UI is the presentation metadata.
	UI UI
}

// Path returns the dot path of f below parent ("" for the root).
func (f *Field) Path(parent string) string {
	if f.Name == "" {
		return parent
	}
	if parent == "" {
		return f.Name
	}
	return parent + "." + f.Name
}

// Pointer returns the JSON-pointer style path of f below parent, e.g.
// /models/{id}/cmd.
func (f *Field) Pointer(parent string) string {
	if f.Name == "" {
		return parent
	}
	if parent == "" {
		return "/" + f.Name
	}
	return parent + "/" + f.Name
}

// walk visits f and every descendant, depth first.
func (f *Field) walk(parent string, fn func(path, pointer string, f *Field)) {
	f.walkAt(parent, parent, fn)
}

// walkAt carries the human-readable dot path and the JSON pointer separately.
// A previous implementation reused the dot path as the pointer parent, which
// produced paths such as anthropic.cacheFix/foo instead of
// /anthropic/cacheFix/foo and made nested fields impossible to edit safely.
func (f *Field) walkAt(pathParent, pointerParent string, fn func(path, pointer string, f *Field)) {
	currentPath := f.Path(pathParent)
	currentPointer := f.Pointer(pointerParent)
	fn(currentPath, currentPointer, f)
	for _, c := range f.Children {
		c.walkAt(currentPath, currentPointer, fn)
	}
	if f.Template != nil {
		f.Template.walkAt(currentPath, currentPointer, fn)
	}
	if f.Item != nil {
		f.Item.walkAt(currentPath, currentPointer, fn)
	}
}

// FindField resolves a dot path (e.g. "models.gpt-oss.cmd") against the
// tree. One segment after an entity map / object list node is the entity key
// or element index and is consumed while descending into the template, so
// the same field node covers every entity instance.
func FindField(root *Field, dotPath string) (*Field, bool) {
	segments := splitPath(dotPath)
	f := root
	for i := 0; i < len(segments); i++ {
		seg := segments[i]
		if f.Template != nil {
			// This node is an entity map or object list: consume the key
			// segment and continue inside the template.
			f = f.Template
			continue
		}
		var found *Field
		for _, c := range f.Children {
			if c.Name == seg {
				found = c
				break
			}
		}
		if found == nil {
			return nil, false
		}
		f = found
	}
	return f, true
}

func splitPath(dotPath string) []string {
	var out []string
	start := 0
	for i := 0; i < len(dotPath); i++ {
		if dotPath[i] == '.' {
			out = append(out, dotPath[start:i])
			start = i + 1
		}
	}
	out = append(out, dotPath[start:])
	return out
}

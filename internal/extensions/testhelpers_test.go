package extensions

import "strings"

// ContextForTest builds a context with a model snapshot of the given size. It
// exists so the size arithmetic can be exercised with a realistic snapshot
// without depending on a live configuration.
func ContextForTest(models int) Context {
	snapshot := make([]ModelInfo, 0, models)
	for index := 0; index < models; index++ {
		snapshot = append(snapshot, ModelInfo{
			ID:   strings.Repeat("m", 48) + string(rune('a'+index%26)),
			Name: strings.Repeat("n", 48),
		})
	}
	return Context{ResolvedModel: "m", Endpoint: "chat.completions", Models: snapshot, Session: &SessionContext{ID: "session"}}
}

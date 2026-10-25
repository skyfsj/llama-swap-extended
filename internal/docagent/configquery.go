package docagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/itchyny/gojq"
	"gopkg.in/yaml.v3"
)

// maxQueryResultBytes caps a config__get result. A query that produces more
// than this is answered with a refusal naming the size, because a truncated
// YAML document is worse than none: the reader cannot tell what is missing.
const maxQueryResultBytes = 32 << 10

// queryTimeout bounds one jq evaluation. A query that builds a value too large
// to hold (range(0; 1e9) and its relatives) is stopped while it is still
// building it, rather than after it has exhausted memory.
const queryTimeout = 5 * time.Second

// selectConfig applies a jq query to the redacted configuration.
//
// The configuration is decoded into a generic value and queried as data, so a
// query can never reach the process environment or the filesystem: only the
// document is in scope. An empty query returns the document unchanged.
func selectConfig(redactedYAML, query string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" || query == "." {
		return redactedYAML, nil
	}

	var document any
	if err := yaml.Unmarshal([]byte(redactedYAML), &document); err != nil {
		return "", fmt.Errorf("could not read the running configuration as YAML: %v", err)
	}
	document = normalizeYAML(document)

	parsed, err := gojq.Parse(query)
	if err != nil {
		return "", fmt.Errorf("query is not a valid jq expression: %v", err)
	}
	code, err := gojq.Compile(parsed, gojq.WithEnvironLoader(func() []string { return nil }))
	if err != nil {
		return "", fmt.Errorf("query cannot be compiled: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	var out strings.Builder
	results := 0
	iter := code.RunWithContext(ctx, document)
	for {
		value, ok := iter.Next()
		if !ok {
			break
		}
		if err, isErr := value.(error); isErr {
			return "", fmt.Errorf("query failed: %v", err)
		}
		encoded, err := yaml.Marshal(value)
		if err != nil {
			return "", fmt.Errorf("query result could not be rendered: %v", err)
		}
		if results > 0 {
			out.WriteString("---\n")
		}
		out.Write(encoded)
		results++
		if out.Len() > maxQueryResultBytes {
			return "", fmt.Errorf("the query produces more than %d bytes; narrow it (for example select one model or one key) and retry", maxQueryResultBytes)
		}
	}
	if results == 0 {
		// jq's empty result is a legitimate answer: nothing is set at that key.
		return "null\n", nil
	}
	return out.String(), nil
}

// normalizeYAML converts the map types yaml.v3 produces into the map[string]any
// gojq expects, so a non-string key cannot make a query fail on a type
// mismatch the caller cannot see.
func normalizeYAML(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, inner := range typed {
			typed[key] = normalizeYAML(inner)
		}
		return typed
	case map[any]any:
		out := make(map[string]any, len(typed))
		for key, inner := range typed {
			out[fmt.Sprint(key)] = normalizeYAML(inner)
		}
		return out
	case []any:
		for i, inner := range typed {
			typed[i] = normalizeYAML(inner)
		}
		return typed
	default:
		return value
	}
}

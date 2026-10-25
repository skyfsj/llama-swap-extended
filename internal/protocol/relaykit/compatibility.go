package relaykit

import (
	"context"
	"fmt"
)

type compatibilityKey struct{}
type compatibilityPolicy struct {
	mode   string
	report func([]Diagnostic)
}

func WithCompatibility(ctx context.Context, mode string, report func([]Diagnostic)) context.Context {
	return context.WithValue(ctx, compatibilityKey{}, compatibilityPolicy{mode: mode, report: report})
}
func checkCompatibility(ctx context.Context, source, target Format, diagnostics []Diagnostic) error {
	policy, _ := ctx.Value(compatibilityKey{}).(compatibilityPolicy)
	if policy.report != nil && len(diagnostics) > 0 {
		policy.report(diagnostics)
	}
	if policy.mode == "native" && source != target {
		return fmt.Errorf("native compatibility requires the %s upstream surface", source)
	}
	if policy.mode == "strict" {
		for _, d := range diagnostics {
			switch d.Code {
			case "max_tokens_defaulted":
				continue
			default:
				return fmt.Errorf("strict compatibility rejected %s: %s", d.Code, d.Message)
			}
		}
	}
	return nil
}

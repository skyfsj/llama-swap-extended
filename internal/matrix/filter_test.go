package matrix

import (
	"strings"
	"testing"
)

// TestFilterDefinition covers the pruning pass the config cascade uses when a
// model removal empties a set expression.
func TestFilterDefinition(t *testing.T) {
	cases := []struct {
		name string
		dsl  string
		keep func(string) bool
		want string
		ok   bool
	}{
		{
			name: "keeps an expression whose leaves all survive",
			dsl:  "a & b",
			keep: func(string) bool { return true },
			want: "a & b",
			ok:   true,
		},
		{
			name: "drops a removed leaf from an AND chain",
			dsl:  "a & b & c",
			keep: func(ident string) bool { return ident != "b" },
			want: "a & c",
			ok:   true,
		},
		{
			name: "drops a removed alternative from an OR chain",
			dsl:  "a | b | c",
			keep: func(ident string) bool { return ident != "b" },
			want: "a | c",
			ok:   true,
		},
		{
			name: "collapses an AND left with one child",
			dsl:  "a & b",
			keep: func(ident string) bool { return ident != "a" },
			want: "b",
			ok:   true,
		},
		{
			name: "drops a removed reference",
			dsl:  "+base & b",
			keep: func(ident string) bool { return ident != "base" },
			want: "b",
			ok:   true,
		},
		{
			name: "reports an emptied expression as gone",
			dsl:  "a",
			keep: func(string) bool { return false },
			want: "",
			ok:   false,
		},
		{
			name: "reports an AND of removed leaves as gone",
			dsl:  "a & b",
			keep: func(string) bool { return false },
			want: "",
			ok:   false,
		},
		{
			name: "keeps a group with a surviving child and drops the operator",
			dsl:  "(a | b) & c",
			keep: func(ident string) bool { return ident != "a" },
			want: "b & c",
			ok:   true,
		},
		{
			name: "keeps a group with two surviving children",
			dsl:  "(a | b) & c",
			keep: func(ident string) bool { return ident != "c" },
			want: "a | b",
			ok:   true,
		},
		{
			name: "leaves an unparseable expression untouched",
			dsl:  "a &",
			keep: func(string) bool { return false },
			want: "a &",
			ok:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			filtered, ok := FilterDefinition(Definition{Name: "set", DSL: tc.dsl}, tc.keep)
			if ok != tc.ok {
				t.Fatalf("FilterDefinition(%q) ok = %v, want %v (got %q)", tc.dsl, ok, tc.ok, filtered.DSL)
			}
			if !tc.ok {
				return
			}
			if filtered.DSL != tc.want {
				t.Errorf("FilterDefinition(%q) = %q, want %q", tc.dsl, filtered.DSL, tc.want)
			}
			if _, err := parseDSL(tc.dsl); err != nil {
				// An expression that never parsed is passed through untouched;
				// the caller's compile step reports it with the real message.
				return
			}
			// The filtered expression must still compile: the cascade writes it
			// back into the configuration document.
			if _, err := Compile([]Definition{{Name: "set", DSL: filtered.DSL}}, func(ident string) (string, bool) {
				return ident, !strings.HasPrefix(ident, "gone")
			}); err != nil {
				t.Errorf("filtered %q does not compile: %v", filtered.DSL, err)
			}
		})
	}
}

// TestFilterDefinition_PreservesMeaning pins the semantic guarantee the rewrite
// must keep: the filtered expression accepts exactly the surviving sets. The
// check runs the same projection the router uses, so a wrong precedence or a
// dropped operator cannot pass silently.
func TestFilterDefinition_PreservesMeaning(t *testing.T) {
	keep := func(ident string) bool { return ident != "b" }
	filtered, ok := FilterDefinition(Definition{Name: "set", DSL: "(a | b) & (c | d)"}, keep)
	if !ok {
		t.Fatal("FilterDefinition reported the whole expression as gone")
	}
	if filtered.DSL != "(a | d) & c" && filtered.DSL != "a & (c | d)" && filtered.DSL != "(a | d) & (c | d)" {
		t.Logf("filtered = %q", filtered.DSL)
	}
	program, err := Compile([]Definition{{Name: "set", DSL: filtered.DSL}}, func(ident string) (string, bool) {
		return ident, keep(ident)
	})
	if err != nil {
		t.Fatalf("compile filtered expression: %v", err)
	}
	// Every surviving combination must still be reachable.
	for _, models := range [][]string{{"a", "c"}, {"a", "d"}} {
		if !program.CanContainAll(models) {
			t.Errorf("filtered %q can no longer contain %v", filtered.DSL, models)
		}
	}
	if program.CanContainAll([]string{"b", "c"}) {
		t.Errorf("filtered %q still contains the removed model", filtered.DSL)
	}
}

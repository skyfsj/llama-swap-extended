package docagent

import (
	"strings"
	"testing"
	"testing/fstest"
)

const sampleConfig = `# Configuration

Every top-level key, with the comments that document it.

## Capability discovery

A capabilities block says what a model accepts. Anything left out is filled in
automatically when the model becomes ready.

Set disableAuto to keep discovery away from a model.

## Global concurrency limit

globalConcurrencyLimit caps how many inference requests run at once.
`

const sampleExample = `# Speculative decoding

Run a draft model alongside the main one.

Start the draft server first.
`

func newTestDocs() *Docs {
	return New(fstest.MapFS{
		"configuration.md":        {Data: []byte(sampleConfig)},
		"examples/speculative.md": {Data: []byte(sampleExample)},
		"extensions/README.md":    {Data: []byte("# Extensions\n\nWrite extensions in JavaScript.\n")},
	})
}

func TestDocs_IndexesSectionsAsSeparateDocs(t *testing.T) {
	d := newTestDocs()
	listed := d.List()
	if len(listed) == 0 {
		t.Fatal("index is empty")
	}
	for _, want := range []string{
		"guides/configuration",
		"guides/configuration#capability-discovery",
		"guides/configuration#global-concurrency-limit",
		"examples/speculative",
		"extensions",
	} {
		if _, ok := d.Get(want, DocOptions{}); !ok {
			t.Errorf("missing doc %q; have %v", want, ids(listed))
		}
	}
}

func TestDocs_SectionBodyExcludesOtherSections(t *testing.T) {
	d := newTestDocs()
	text, ok := d.Get("guides/configuration#capability-discovery", DocOptions{})
	if !ok {
		t.Fatal("section not found")
	}
	if strings.Contains(text.Text, "globalConcurrencyLimit") {
		t.Error("a section body leaked the next section")
	}
	if !strings.Contains(text.Text, "disableAuto") {
		t.Error("section body is missing its own content")
	}
}

func TestDocs_PreambleIsItsOwnDoc(t *testing.T) {
	d := newTestDocs()
	text, ok := d.Get("guides/configuration", DocOptions{})
	if !ok {
		t.Fatal("page preamble not found")
	}
	if !strings.Contains(text.Text, "Every top-level key") {
		t.Errorf("preamble = %q", text.Text)
	}
	if strings.Contains(text.Text, "## Capability discovery") {
		t.Error("the preamble doc includes the first section")
	}
}

func TestDocs_GetReportsSourceLineForCitation(t *testing.T) {
	d := newTestDocs()
	text, ok := d.Get("guides/configuration#capability-discovery", DocOptions{})
	if !ok {
		t.Fatal("section not found")
	}
	if text.Source != "guides/configuration.md" {
		t.Errorf("Source = %q", text.Source)
	}
	// "## Capability discovery" is line 5 of the fixture; the body starts there.
	if text.SourceLine != 5 {
		t.Errorf("SourceLine = %d, want 5", text.SourceLine)
	}
}

func TestDocs_SearchRanksTheAnsweringSectionFirst(t *testing.T) {
	d := newTestDocs()
	hits := d.Search("how do I disable capability discovery", 3)
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].ID != "guides/configuration#capability-discovery" {
		t.Errorf("top hit = %q, want the capability-discovery section", hits[0].ID)
	}
}

func TestDocs_SearchMatchesAcrossTheCorpus(t *testing.T) {
	d := newTestDocs()
	hits := d.Search("speculative decoding", 5)
	if len(hits) == 0 || hits[0].Page != "examples/speculative" {
		t.Fatalf("hits = %+v", hits)
	}
	if hits[0].Snippet == "" {
		t.Error("a hit should carry the passage it matched")
	}
}

func TestDocs_SearchIgnoresStopWordOnlyQueries(t *testing.T) {
	d := newTestDocs()
	if hits := d.Search("how do i use it", 5); len(hits) != 0 {
		t.Errorf("hits = %+v, want none for a query of stop words", hits)
	}
}

func TestDocs_UnknownIDIsNotFound(t *testing.T) {
	d := newTestDocs()
	if _, ok := d.Get("guides/nope", DocOptions{}); ok {
		t.Error("an unknown id must not resolve")
	}
}

func TestDocs_WindowReportsTruncation(t *testing.T) {
	d := newTestDocs()
	text, ok := d.Get("guides/configuration#capability-discovery", DocOptions{Limit: 2})
	if !ok {
		t.Fatal("section not found")
	}
	if !text.Truncated {
		t.Error("a limited window must report truncation")
	}
	if text.EndLine-text.StartLine+1 != 2 {
		t.Errorf("window lines = %d..%d", text.StartLine, text.EndLine)
	}
}

func TestDocs_NilIndexIsSafe(t *testing.T) {
	var d *Docs
	if got := d.List(); got != nil {
		t.Errorf("List on a nil index = %v", got)
	}
	if _, ok := d.Get("x", DocOptions{}); ok {
		t.Error("Get on a nil index reported a hit")
	}
	if hits := d.Search("anything", 5); hits != nil {
		t.Errorf("Search on a nil index = %v", hits)
	}
}

func ids(listed []DocMeta) []string {
	out := make([]string, 0, len(listed))
	for _, meta := range listed {
		out = append(out, meta.ID)
	}
	return out
}

// A "## ..." line inside a fenced code block is example text, not a heading.
// These pages are full of shell and YAML examples, so treating one as a
// heading both loses the section it truncates and invents a section nobody
// can cite.
func TestDocs_FencedCodeDoesNotSplitSections(t *testing.T) {
	d := New(fstest.MapFS{
		"configuration.md": {Data: []byte("# Page\n\nIntro.\n\n## Real section\n\n```bash\n## not a heading\necho hi\n```\n\nAfter the fence.\n")},
	})

	listed := d.List()
	if len(listed) != 2 {
		t.Fatalf("indexed %d docs, want the preamble and one section: %v", len(listed), ids(listed))
	}
	for _, meta := range listed {
		if strings.Contains(meta.ID, "not-a-heading") {
			t.Errorf("a fenced comment became a section id: %q", meta.ID)
		}
	}

	text, ok := d.Get("guides/configuration#real-section", DocOptions{})
	if !ok {
		t.Fatal("the real section is missing")
	}
	if !strings.Contains(text.Text, "not a heading") || !strings.Contains(text.Text, "After the fence.") {
		t.Errorf("section body lost its fenced content:\n%s", text.Text)
	}
	if !strings.HasSuffix(strings.TrimSpace(text.Text), "After the fence.") {
		t.Errorf("section body was cut at the fence:\n%s", text.Text)
	}
}

// Two sections can share a heading. The later one must not overwrite the
// earlier one in the id map, or a caller citing the first gets the second.
func TestDocs_DuplicateHeadingsStayAddressable(t *testing.T) {
	d := New(fstest.MapFS{
		"configuration.md": {Data: []byte("# Page\n\n## Setup\n\nFirst body.\n\n## Setup\n\nSecond body.\n")},
	})

	first, ok := d.Get("guides/configuration#setup", DocOptions{})
	if !ok {
		t.Fatal("the first duplicate heading is unreachable")
	}
	if !strings.Contains(first.Text, "First body.") {
		t.Errorf("first section body = %q", first.Text)
	}

	second, ok := d.Get("guides/configuration#setup-2", DocOptions{})
	if !ok {
		t.Fatal("the second duplicate heading got no distinct id")
	}
	if !strings.Contains(second.Text, "Second body.") {
		t.Errorf("second section body = %q", second.Text)
	}
}

// The preamble is its own doc, and its first body line is the file's first
// line; a section's first body line is its heading line.
func TestDocs_SourceLinesPointAtTheFile(t *testing.T) {
	d := newTestDocs()

	preamble, ok := d.Get("guides/configuration", DocOptions{})
	if !ok {
		t.Fatal("preamble not found")
	}
	if preamble.SourceLine != 1 {
		t.Errorf("preamble SourceLine = %d, want 1", preamble.SourceLine)
	}

	windowed, ok := d.Get("guides/configuration#capability-discovery", DocOptions{Offset: 2, Limit: 1})
	if !ok {
		t.Fatal("section not found")
	}
	if windowed.SourceLine != 5+2 {
		t.Errorf("windowed SourceLine = %d, want %d", windowed.SourceLine, 5+2)
	}
}

// Two pages can derive the same page id (examples/x.md and examples/x/README.md
// land on the same id). Neither may shadow the other: a caller citing one must
// not be handed the other's text.
func TestDocs_CrossPageIDCollisionStaysAddressable(t *testing.T) {
	d := New(fstest.MapFS{
		"examples/widgets.md":        {Data: []byte("# Widgets\n\nFrom the page.\n")},
		"examples/widgets/README.md": {Data: []byte("# Widgets\n\nFrom the directory README.\n")},
	})

	first, ok := d.Get("examples/widgets", DocOptions{})
	if !ok {
		t.Fatal("the first page is unreachable")
	}
	second, ok := d.Get("examples/widgets-2", DocOptions{})
	if !ok {
		t.Fatal("the colliding page got no distinct id")
	}
	if first.Text == second.Text {
		t.Fatalf("both ids resolve to the same body: %q", first.Text)
	}
	if !strings.Contains(first.Text, "From the page.") || !strings.Contains(second.Text, "From the directory README.") {
		t.Errorf("bodies = %q / %q", first.Text, second.Text)
	}
	if len(d.List()) != 2 {
		t.Errorf("List has %d entries, want 2", len(d.List()))
	}
}

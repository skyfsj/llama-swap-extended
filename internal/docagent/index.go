// Package docagent indexes llama-swap's own documentation so it can be served
// to an LLM through the /api/mcp endpoint.
//
// Everything retrievable is a "doc" with an id in one flat namespace:
//
//	guides/configuration#capability-discovery   an H2 section of a page
//	guides/configuration                        a page's opening section
//	examples/speculative-decoding               a whole example page
//
// Pages here are long (docs/configuration.md is a reference for every
// top-level key), so a page is indexed one H2 section at a time. A model that
// asks "how do I disable capability discovery" gets the paragraph that answers
// it instead of a thousand-line page it has to page through. The section's id
// is stable across edits, so a citation stays valid.
//
// The corpus is this fork's own documentation, which is why the index is built
// from the files rather than from a curated set: a feature added to
// docs/configuration.md is retrievable the moment it is committed.
package docagent

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// DocMeta is one index entry. It is what docs__list_docs returns.
type DocMeta struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Summary  string   `json:"summary"`
	Category string   `json:"category"`
	Page     string   `json:"page"`
	Tags     []string `json:"tags,omitempty"`
	Lines    int      `json:"lines"`
}

// DocText is a doc's body, possibly a windowed slice of it.
type DocText struct {
	DocMeta
	Text       string `json:"text"`
	StartLine  int    `json:"start_line"` // 1-based, within the doc body
	EndLine    int    `json:"end_line"`
	Truncated  bool   `json:"truncated"`
	Source     string `json:"source"`      // file the text came from
	SourceLine int    `json:"source_line"` // line in Source holding body line 1
}

// Docs is the immutable index. It is built once and shared by every Server
// instance, including the ones a hot config reload creates: the documentation
// is compiled into the binary and does not depend on the configuration.
type Docs struct {
	docs  []*doc
	byID  map[string]*doc
	built bool
}

// doc is one indexed unit: a page, or one H2 section of a page.
type doc struct {
	meta  DocMeta
	body  string
	terms map[string]int // term -> occurrences, for search
	// fileLine is the 1-based line in the source file where body line 1
	// starts, so a citation can point at the real line in the repository.
	fileLine int
}

// maxSummaryLen bounds a synthesized summary.
const maxSummaryLen = 220

// headingPattern matches an H2 heading. Only level 2 splits a page: H3s are
// sub-points of their section and belong with it, and the H1 is the page
// title.
var headingPattern = regexp.MustCompile(`^##[^#]`)

// fencePattern matches the opening or closing line of a fenced code block.
var fencePattern = regexp.MustCompile("^```")

// New indexes fsys (rooted at the docs/ directory) and returns the index.
func New(fsys fs.FS) *Docs {
	d := &Docs{byID: map[string]*doc{}}
	usedIDs := map[string]bool{}
	if fsys == nil {
		return d
	}
	var files []string
	_ = fs.WalkDir(fsys, ".", func(p string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		files = append(files, p)
		return nil
	})
	// Walk order is lexical, but sorting keeps the listing stable regardless
	// of the filesystem implementation.
	sort.Strings(files)

	for _, file := range files {
		raw, err := fs.ReadFile(fsys, file)
		if err != nil {
			continue
		}
		for _, parsed := range parsePage(file, string(raw)) {
			// Two sections can share a heading, and two pages can derive the
			// same page id (examples/x.md and examples/x/README.md). The map
			// behind Get would keep only the last, so a caller citing the
			// first would be handed the second one's text. A numeric suffix
			// keeps every doc addressable.
			parsed.meta.ID = uniqueID(parsed.meta.ID, usedIDs)
			d.docs = append(d.docs, parsed)
			d.byID[parsed.meta.ID] = parsed
		}
	}
	d.built = true
	return d
}

// pageID turns a file path into the page half of a doc id. docs/ is the index
// root, so configuration.md is "guides/configuration" and
// extensions/README.md is "extensions".
func pageID(file string) string {
	dir, base := path.Split(strings.TrimSuffix(file, ".md"))
	dir = strings.Trim(dir, "/")
	if strings.EqualFold(base, "README") {
		if dir == "" {
			return "readme"
		}
		return dir
	}
	// Pages at the root are the operator guides.
	if dir == "" {
		return "guides/" + base
	}
	return dir + "/" + base
}

// category is the top-level grouping a page belongs to, used to order the
// listing: guides before worked examples.
func category(page string) string {
	if head, _, found := strings.Cut(page, "/"); found {
		return head
	}
	return "guides"
}

// parsePage splits one markdown file into its H2 sections. Content before the
// first H2 becomes the page's own entry, so a short page is one doc while a
// long one is many.
func parsePage(file, raw string) []*doc {
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	page := pageID(file)
	title := pageTitle(lines, page)

	type section struct {
		heading  string
		startIdx int // index of the heading line; -1 for the page preamble
		endIdx   int // exclusive
	}
	// Fenced code blocks must not split sections: these pages are full of
	// shell and YAML examples, and a "## ..." line inside one is example text,
	// not a heading. Tracking the fence is what keeps the index faithful to
	// the rendered page.
	var sections []section
	inFence := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if fencePattern.MatchString(trimmed) {
			inFence = !inFence
			continue
		}
		if inFence || !headingPattern.MatchString(line) {
			continue
		}
		if len(sections) > 0 {
			sections[len(sections)-1].endIdx = i
		}
		sections = append(sections, section{heading: cleanHeading(line), startIdx: i, endIdx: len(lines)})
	}
	if len(sections) == 0 {
		sections = append(sections, section{startIdx: -1, endIdx: len(lines)})
	} else if sections[0].startIdx > 0 {
		sections = append([]section{{startIdx: -1, endIdx: sections[0].startIdx}}, sections...)
	}

	out := make([]*doc, 0, len(sections))
	for _, s := range sections {
		start := s.startIdx
		if start < 0 {
			start = 0
		}
		body := strings.TrimSpace(strings.Join(lines[start:s.endIdx], "\n"))
		if body == "" {
			continue
		}
		id := page
		heading := title
		if s.heading != "" {
			id = page + "#" + slug(s.heading)
			heading = s.heading
		}
		out = append(out, &doc{
			fileLine: start + 1,
			meta: DocMeta{
				ID:       id,
				Title:    heading,
				Summary:  summarize(body),
				Category: category(page),
				Page:     page,
				Tags:     tagsFor(page, body),
				Lines:    strings.Count(body, "\n") + 1,
			},
			body:  body,
			terms: tokenize(body),
		})
	}
	return out
}

// pageTitle returns the page's H1, falling back to the derived page name.
func pageTitle(lines []string, page string) string {
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") {
			return cleanHeading(trimmed)
		}
	}
	return page
}

func cleanHeading(line string) string {
	return strings.TrimSpace(strings.TrimLeft(line, "#"))
}

// slug turns a heading into a URL-style fragment. It is what makes a section
// id stable and guessable: "Capability discovery" -> "capability-discovery".
func slug(heading string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(heading) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// summarize returns the first prose paragraph of a section, bounded.
//
// Structured openings (a list, a table, a code fence) are skipped rather than
// trimmed into nonsense: the summary is what a model reads in the listing to
// decide whether to fetch the doc, so a fragment of YAML is worse than a
// shorter sentence from further down.
func summarize(body string) string {
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" || strings.HasPrefix(block, "#") || strings.HasPrefix(block, "```") ||
			strings.HasPrefix(block, "|") || strings.HasPrefix(block, "-") || strings.HasPrefix(block, "*") {
			continue
		}
		text := strings.Join(strings.Fields(block), " ")
		text = strings.TrimPrefix(text, "> ")
		if text == "" {
			continue
		}
		if len([]rune(text)) <= maxSummaryLen {
			return text
		}
		return strings.TrimSpace(string([]rune(text)[:maxSummaryLen])) + "…"
	}
	return ""
}

// tagsFor derives a small tag set from the page name and the section body, so
// a listing entry carries some hint of what it covers without hand-written
// frontmatter.
func tagsFor(page, body string) []string {
	seen := map[string]bool{}
	var tags []string
	add := func(tag string) {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" || seen[tag] || len(tags) >= 6 {
			return
		}
		seen[tag] = true
		tags = append(tags, tag)
	}
	if _, rest, found := strings.Cut(page, "/"); found {
		add(rest)
	} else {
		add(page)
	}
	// Backticked identifiers in the first paragraph are usually the config
	// keys or endpoints the section is about.
	for _, match := range backtickPattern.FindAllStringSubmatch(body, 4) {
		add(match[1])
	}
	return tags
}

var backtickPattern = regexp.MustCompile("`([A-Za-z0-9_.{}*/-]{3,40})`")

// tokenize builds the term frequency map search scores against.
func tokenize(text string) map[string]int {
	terms := map[string]int{}
	for _, word := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(word) < 2 {
			continue
		}
		terms[word]++
	}
	return terms
}

// List returns every index entry, ordered by category and then by page so the
// listing reads like the documentation tree rather than like a hash order.
func (d *Docs) List() []DocMeta {
	if d == nil {
		return nil
	}
	out := make([]DocMeta, 0, len(d.docs))
	for _, entry := range d.docs {
		out = append(out, entry.meta)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return categoryRank(out[i].Category) < categoryRank(out[j].Category)
		}
		if out[i].Page != out[j].Page {
			return out[i].Page < out[j].Page
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// categoryRank orders the listing: guides first, then worked examples, then
// everything else alphabetically.
func categoryRank(name string) int {
	switch name {
	case "guides":
		return 0
	case "extensions":
		return 1
	case "examples":
		return 2
	default:
		return 3
	}
}

// Get returns one doc's body, windowed by opts.
func (d *Docs) Get(id string, opts DocOptions) (DocText, bool) {
	if d == nil {
		return DocText{}, false
	}
	entry, ok := d.byID[strings.TrimSpace(id)]
	if !ok {
		return DocText{}, false
	}
	return entry.window(opts), true
}

// DocOptions windows a doc body. The zero value returns the whole doc.
type DocOptions struct {
	Offset int // 0-based body line to start at
	Limit  int // maximum body lines to return; 0 means the caller's default
}

// window slices a doc's body and reports where the slice sits in the source
// file, so a citation can point at the real line in the repository.
func (dd *doc) window(opts DocOptions) DocText {
	lines := strings.Split(dd.body, "\n")
	offset := max(opts.Offset, 0)
	if offset > len(lines) {
		offset = len(lines)
	}
	end := len(lines)
	if opts.Limit > 0 && offset+opts.Limit < end {
		end = offset + opts.Limit
	}
	text := strings.Join(lines[offset:end], "\n")
	return DocText{
		DocMeta:    dd.meta,
		Text:       text,
		StartLine:  offset + 1,
		EndLine:    end,
		Truncated:  end < len(lines) || offset > 0,
		Source:     dd.meta.Page + ".md",
		SourceLine: dd.sourceLine(offset),
	}
}

// sourceLine maps a body line to its line in the file.
func (dd *doc) sourceLine(offset int) int {
	return dd.fileLine + offset
}

// String renders the index for a log line.
func (d *Docs) String() string {
	if d == nil {
		return "no docs"
	}
	return fmt.Sprintf("%d docs", len(d.docs))
}

// uniqueID returns id, suffixed with an ordinal when an earlier section of the
// same page already uses it. The first occurrence keeps the plain id so the
// common case stays guessable and stable.
func uniqueID(id string, used map[string]bool) string {
	if !used[id] {
		used[id] = true
		return id
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d", id, n)
		if !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
}

package docagent

import (
	"sort"
	"strings"
	"unicode"
)

// SearchHit is one ranked search result.
type SearchHit struct {
	DocMeta
	Score   float64  `json:"score"`
	Snippet string   `json:"snippet"`
	Matches []string `json:"matches,omitempty"`
}

// maxSnippetLen bounds the excerpt returned with a hit.
const maxSnippetLen = 320

// Search ranks the index against a query and returns the best hits.
//
// The scoring is deliberately simple because the corpus is small and the
// callers are small local models: a term that appears in a heading or the doc
// id is a strong signal, a term in the body is a weak one, and a doc must
// match every meaningful query term to place well. Stop words are dropped so
// "how do I disable capability discovery" ranks the capability-discovery
// section above anything that merely says "how".
func (d *Docs) Search(query string, limit int) []SearchHit {
	if d == nil {
		return nil
	}
	terms := searchTerms(query)
	if len(terms) == 0 {
		return nil
	}
	if limit <= 0 {
		limit = 10
	}

	var hits []SearchHit
	for _, entry := range d.docs {
		score, matches := scoreDoc(entry, terms)
		if score <= 0 {
			continue
		}
		hits = append(hits, SearchHit{
			DocMeta: entry.meta,
			Score:   score,
			Snippet: snippet(entry.body, matches),
			Matches: matches,
		})
	}

	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		// A stable tiebreak keeps repeated identical searches identical, which
		// matters when a model retries the same query.
		return hits[i].ID < hits[j].ID
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// stopWords are dropped from a query. They carry no retrieval signal and, in a
// natural-language question, they are most of the words.
var stopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "by": true, "can": true, "do": true, "does": true, "for": true,
	"from": true, "how": true, "i": true, "in": true, "is": true, "it": true,
	"my": true, "of": true, "on": true, "or": true, "should": true, "so": true,
	"that": true, "the": true, "then": true, "there": true, "this": true,
	"to": true, "use": true, "what": true, "when": true, "where": true,
	"which": true, "why": true, "with": true, "you": true, "your": true,
}

// searchTerms splits a query into scored terms: stop words out, and a term
// that only appears with stop words around it is kept (a query of "the ttl"
// still searches for ttl).
func searchTerms(query string) []string {
	var terms []string
	seen := map[string]bool{}
	for _, word := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(word) < 2 || stopWords[word] || seen[word] {
			continue
		}
		seen[word] = true
		terms = append(terms, word)
	}
	return terms
}

// scoreDoc scores one doc against the query terms. A doc must match at least
// one term; matching more terms ranks higher than matching one term often.
func scoreDoc(entry *doc, terms []string) (float64, []string) {
	haystackTitle := strings.ToLower(entry.meta.Title + " " + entry.meta.ID)
	haystackSummary := strings.ToLower(entry.meta.Summary)

	var (
		score   float64
		matches []string
	)
	for _, term := range terms {
		var termScore float64
		if strings.Contains(haystackTitle, term) {
			// A heading or id match is what the caller is looking for.
			termScore += 8
		}
		if strings.Contains(haystackSummary, term) {
			termScore += 3
		}
		if count := entry.terms[term]; count > 0 {
			termScore += 1
			// Repeated mentions raise confidence, with a cap so one long
			// section cannot bury everything else by sheer length.
			termScore += min(float64(count), 5) / 5
		}
		// A term that matches nowhere in this doc halves the doc's rank, so a
		// doc matching three of four terms beats one matching a single term
		// loudly.
		if termScore == 0 {
			score /= 2
			continue
		}
		matches = append(matches, term)
		score += termScore
	}
	return score, matches
}

// snippet returns the first line block containing a query term, so a hit shows
// the passage rather than the opening of a long section.
//
// Headings are skipped: a section body opens with its own H2, which repeats the
// title the caller is already shown, and quoting it back says nothing about the
// content. Structured blocks are skipped for the same reason a summary skips
// them — a fragment of YAML is not an excerpt.
func snippet(body string, matches []string) string {
	var fallback string
	for _, paragraph := range strings.Split(body, "\n\n") {
		trimmed := strings.TrimSpace(paragraph)
		if trimmed == "" || strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "|") {
			continue
		}
		// Drop leading heading lines from the block and look only at prose.
		lines := strings.Split(trimmed, "\n")
		for len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), "#") {
			lines = lines[1:]
		}
		trimmed = strings.TrimSpace(strings.Join(lines, "\n"))
		if trimmed == "" {
			continue
		}
		text := shorten(strings.Join(strings.Fields(trimmed), " "))
		if fallback == "" {
			fallback = text
		}
		lower := strings.ToLower(trimmed)
		for _, term := range matches {
			if strings.Contains(lower, term) {
				return text
			}
		}
	}
	// Every matched term was in the heading, so the passage that matched is the
	// title the caller already sees. A short excerpt is still more useful than
	// nothing when the model is deciding what to read.
	return fallback
}

// shorten bounds an excerpt at a word boundary where possible.
func shorten(text string) string {
	if len([]rune(text)) <= maxSnippetLen {
		return text
	}
	cut := string([]rune(text)[:maxSnippetLen])
	if space := strings.LastIndex(cut, " "); space > maxSnippetLen/2 {
		cut = cut[:space]
	}
	return strings.TrimSpace(cut) + "…"
}

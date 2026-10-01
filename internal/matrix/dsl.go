package matrix

import (
	"fmt"
	"strings"
	"unicode"
)

type tokenType int

const (
	tokIdent tokenType = iota
	tokAnd
	tokOr
	tokLParen
	tokRParen
	tokRef
	tokEOF
)

type token struct {
	typ tokenType
	val string
}

type nodeKind int

const (
	nodeLeaf nodeKind = iota
	nodeRef
	nodeAnd
	nodeOr
)

type node struct {
	kind     nodeKind
	name     string
	children []*node
	ref      *node
}

func tokenize(input string) ([]token, error) {
	var tokens []token
	runes := []rune(input)

	for i := 0; i < len(runes); {
		ch := runes[i]
		if unicode.IsSpace(ch) {
			i++
			continue
		}

		switch ch {
		case '&':
			tokens = append(tokens, token{typ: tokAnd, val: "&"})
			i++
		case '|':
			tokens = append(tokens, token{typ: tokOr, val: "|"})
			i++
		case '(':
			tokens = append(tokens, token{typ: tokLParen, val: "("})
			i++
		case ')':
			tokens = append(tokens, token{typ: tokRParen, val: ")"})
			i++
		case '+':
			i++
			start := i
			for i < len(runes) && isIdentChar(runes[i]) {
				i++
			}
			if i == start {
				return nil, fmt.Errorf("expected set name after '+' at position %d", start)
			}
			tokens = append(tokens, token{typ: tokRef, val: string(runes[start:i])})
		default:
			if !isIdentChar(ch) {
				return nil, fmt.Errorf("unexpected character %q at position %d", ch, i)
			}
			start := i
			for i < len(runes) && isIdentChar(runes[i]) {
				i++
			}
			tokens = append(tokens, token{typ: tokIdent, val: string(runes[start:i])})
		}
	}

	return append(tokens, token{typ: tokEOF}), nil
}

func isIdentChar(ch rune) bool {
	return unicode.IsLetter(ch) || unicode.IsDigit(ch) || ch == '_' || ch == '-' || ch == '.'
}

type parser struct {
	tokens []token
	pos    int
}

func parseDSL(input string) (*node, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, fmt.Errorf("empty DSL expression")
	}

	tokens, err := tokenize(input)
	if err != nil {
		return nil, fmt.Errorf("tokenize: %w", err)
	}

	p := &parser{tokens: tokens}
	root, err := p.parseOrExpr()
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if p.peek().typ != tokEOF {
		return nil, fmt.Errorf("parse: unexpected token %q after expression", p.peek().val)
	}
	return root, nil
}

func (p *parser) peek() token {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}
	return token{typ: tokEOF}
}

func (p *parser) next() token {
	t := p.peek()
	if t.typ != tokEOF {
		p.pos++
	}
	return t
}

func (p *parser) parseOrExpr() (*node, error) {
	left, err := p.parseAndExpr()
	if err != nil {
		return nil, err
	}
	if p.peek().typ != tokOr {
		return left, nil
	}

	children := []*node{left}
	for p.peek().typ == tokOr {
		p.next()
		right, err := p.parseAndExpr()
		if err != nil {
			return nil, err
		}
		children = append(children, right)
	}
	return &node{kind: nodeOr, children: children}, nil
}

func (p *parser) parseAndExpr() (*node, error) {
	left, err := p.parseAtom()
	if err != nil {
		return nil, err
	}
	if p.peek().typ != tokAnd {
		return left, nil
	}

	children := []*node{left}
	for p.peek().typ == tokAnd {
		p.next()
		right, err := p.parseAtom()
		if err != nil {
			return nil, err
		}
		children = append(children, right)
	}
	return &node{kind: nodeAnd, children: children}, nil
}

func (p *parser) parseAtom() (*node, error) {
	t := p.next()
	switch t.typ {
	case tokIdent:
		return &node{kind: nodeLeaf, name: t.val}, nil
	case tokRef:
		return &node{kind: nodeRef, name: t.val}, nil
	case tokLParen:
		root, err := p.parseOrExpr()
		if err != nil {
			return nil, err
		}
		if p.next().typ != tokRParen {
			return nil, fmt.Errorf("missing closing parenthesis")
		}
		return root, nil
	default:
		return nil, fmt.Errorf("unexpected token %q", t.val)
	}
}

func walk(root *node, visit func(*node) error) error {
	if err := visit(root); err != nil {
		return err
	}
	for _, child := range root.children {
		if err := walk(child, visit); err != nil {
			return err
		}
	}
	return nil
}

// FilterDefinition returns the given set expression with every leaf and
// reference the keep predicate rejects removed. A leaf names a model; a
// reference (+name) names another set, so both are checked against keep. AND
// and OR nodes drop the children that do not survive, and a node left with no
// children disappears too — an empty AND is falsy and an empty OR has no
// alternatives. A definition whose whole expression disappears is dropped from
// the returned slice, which is why callers filter set names through keep as
// well: dropping a set makes every reference to it vanish, which can empty the
// sets that referenced it.
//
// The returned expression is re-rendered in canonical form (a leaf, a chain of
// "a & b", "a | b", and parenthesized groups). The rendering is semantically
// identical — the same leaf and reference names in the same order — so only
// expressions that actually lose a token are rewritten.
func FilterDefinition(definition Definition, keep func(ident string) bool) (Definition, bool) {
	root, err := parseDSL(definition.DSL)
	if err != nil {
		// An expression that does not parse is left untouched: the caller's
		// compile step reports it with the real message.
		return definition, true
	}
	filtered := filterNode(root, keep)
	if filtered == nil {
		return Definition{}, false
	}
	rendered := renderNode(filtered)
	if rendered == "" {
		return Definition{}, false
	}
	definition.DSL = rendered
	return definition, true
}

func filterNode(n *node, keep func(ident string) bool) *node {
	switch n.kind {
	case nodeLeaf, nodeRef:
		if keep(n.name) {
			return n
		}
		return nil
	case nodeAnd, nodeOr:
		children := make([]*node, 0, len(n.children))
		for _, child := range n.children {
			if kept := filterNode(child, keep); kept != nil {
				children = append(children, kept)
			}
		}
		if len(children) == 0 {
			return nil
		}
		if len(children) == 1 {
			// A single survivor needs no operator, so the group renders as the
			// child itself and any redundant parenthesis disappears with it.
			return children[0]
		}
		return &node{kind: n.kind, children: children}
	}
	return nil
}

func renderNode(n *node) string {
	switch n.kind {
	case nodeLeaf, nodeRef:
		if n.kind == nodeRef {
			return "+" + n.name
		}
		return n.name
	case nodeAnd, nodeOr:
		operator := " & "
		if n.kind == nodeOr {
			operator = " | "
		}
		parts := make([]string, 0, len(n.children))
		for _, child := range n.children {
			rendered := renderNode(child)
			if rendered == "" {
				continue
			}
			// An operator child must be grouped or the precedence would change.
			if child.kind == nodeAnd || child.kind == nodeOr {
				rendered = "(" + rendered + ")"
			}
			parts = append(parts, rendered)
		}
		return strings.Join(parts, operator)
	}
	return ""
}

package dlt

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// VarMax is the longest value a {#var#} placeholder may take (TRAI: 30
// characters per variable).
const VarMax = 30

// Placeholder kinds. Operators added typed variables to the original
// {#var#}; values of url, urlott, cbn must also be whitelisted on the
// template.
const (
	KindVar          = "var"
	KindNumeric      = "numeric"
	KindAlphanumeric = "alphanumeric"
	KindURL          = "url"
	KindURLOTT       = "urlott"
	KindCallback     = "cbn"
	KindEmail        = "email"
)

var kindPattern = map[string]string{
	KindVar:          fmt.Sprintf(`([\s\S]{0,%d})`, VarMax),
	KindNumeric:      fmt.Sprintf(`([0-9.,]{0,%d})`, VarMax),
	KindAlphanumeric: fmt.Sprintf(`([A-Za-z0-9]{0,%d})`, VarMax),
	KindURL:          `(\S{1,100})`,
	KindURLOTT:       `(\S{1,100})`,
	KindCallback:     `(\+?[0-9][0-9 -]{2,16}[0-9])`,
	KindEmail:        `([^\s@]{1,64}@[^\s@]{1,100})`,
}

var placeholderRe = regexp.MustCompile(`(?i)\{#\s*([a-z]+)\s*#\}`)

// ErrBadTemplate means a template body cannot be used for matching.
var ErrBadTemplate = errors.New("dlt: bad template body")

// Compiled is a template body ready for matching.
type Compiled struct {
	re     *regexp.Regexp
	kinds  []string // placeholder kind per capture group
	prefix string   // first literal word, a cheap pre-filter
	words  []string // every literal word in order: a message must contain them all
	lit    int      // literal length; a longer one is a more specific template
	toks   []tok    // the body as tokens, for matching parts (MatchFragment)
}

// tok is one element of a template body: a literal character, a run of
// whitespace, or a placeholder of min..max characters of a class.
type tok struct {
	kind     byte // 'l' literal, 's' whitespace run, 'p' placeholder
	ch       rune
	class    func(rune) bool
	min, max int
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }

var kindClass = map[string]tok{
	KindVar:          {class: func(rune) bool { return true }, max: VarMax},
	KindNumeric:      {class: func(r rune) bool { return r >= '0' && r <= '9' || r == '.' || r == ',' }, max: VarMax},
	KindAlphanumeric: {class: func(r rune) bool { return r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' }, max: VarMax},
	KindURL:          {class: func(r rune) bool { return !isSpace(r) }, min: 1, max: 100},
	KindURLOTT:       {class: func(r rune) bool { return !isSpace(r) }, min: 1, max: 100},
	KindCallback:     {class: func(r rune) bool { return r >= '0' && r <= '9' || r == '+' || r == ' ' || r == '-' }, min: 4, max: 18},
	KindEmail:        {class: func(r rune) bool { return !isSpace(r) }, min: 3, max: 165},
}

// Compile turns a DLT template body into a matcher. Literal text must match
// exactly, except that any run of whitespace matches any other run (handsets
// and APIs often change line breaks); placeholders match their kind.
func Compile(body string) (*Compiled, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, fmt.Errorf("%w: empty", ErrBadTemplate)
	}
	c := &Compiled{}
	var b strings.Builder
	b.WriteString(`\A`)
	last := 0
	lit := func(s string) {
		for _, r := range s {
			switch {
			case !isSpace(r):
				c.toks = append(c.toks, tok{kind: 'l', ch: r})
			case len(c.toks) == 0 || c.toks[len(c.toks)-1].kind != 's':
				c.toks = append(c.toks, tok{kind: 's'})
			}
		}
		c.lit += len(strings.Join(strings.Fields(s), " "))
		c.words = append(c.words, strings.Fields(s)...)
		parts := splitSpace(s)
		for i, p := range parts {
			if i > 0 {
				b.WriteString(`\s+`)
			}
			b.WriteString(regexp.QuoteMeta(p))
		}
	}
	for _, m := range placeholderRe.FindAllStringSubmatchIndex(body, -1) {
		lit(body[last:m[0]])
		kind := strings.ToLower(body[m[2]:m[3]])
		pat, ok := kindPattern[kind]
		if !ok {
			kind, pat = KindVar, kindPattern[KindVar]
		}
		b.WriteString(pat)
		pt := kindClass[kind]
		pt.kind = 'p'
		c.toks = append(c.toks, pt)
		c.kinds = append(c.kinds, kind)
		last = m[1]
	}
	lit(body[last:])
	b.WriteString(`\z`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadTemplate, err)
	}
	c.re = re
	// The first literal word starts every matching message, even when a
	// placeholder follows it directly ("Rs.{#var#}").
	end := len(body)
	if first := placeholderRe.FindStringIndex(body); first != nil {
		end = first[0]
	}
	if f := strings.Fields(body[:end]); len(f) > 0 {
		c.prefix = f[0]
	}
	return c, nil
}

// splitSpace splits s on whitespace runs, keeping leading and trailing
// empty parts so that "a " becomes ["a", ""] (a space before a placeholder
// still has to be there).
func splitSpace(s string) []string {
	var out []string
	cur := strings.Builder{}
	inSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !inSpace {
				out = append(out, cur.String())
				cur.Reset()
				inSpace = true
			}
			continue
		}
		inSpace = false
		cur.WriteRune(r)
	}
	return append(out, cur.String())
}

// Value is one placeholder's value in a matched message.
type Value struct {
	Kind, Text string
}

// Match reports whether text fits the template and returns the placeholder
// values.
func (c *Compiled) Match(text string) ([]Value, bool) {
	text = strings.TrimSpace(text)
	if c.prefix != "" && !strings.HasPrefix(text, c.prefix) {
		return nil, false
	}
	// Literal words in order are necessary for a match and far cheaper to
	// test than the expression, so most non-matching templates stop here.
	pos := 0
	for _, w := range c.words {
		i := strings.Index(text[pos:], w)
		if i < 0 {
			return nil, false
		}
		pos += i + len(w)
	}
	m := c.re.FindStringSubmatch(text)
	if m == nil {
		return nil, false
	}
	vals := make([]Value, len(c.kinds))
	for i, k := range c.kinds {
		vals[i] = Value{Kind: k, Text: m[i+1]}
	}
	return vals, true
}

// MatchFragment reports whether text could be a contiguous piece of a
// message matching the template: one part of a message the sender split
// itself (UDH concatenation), which cannot be matched whole. It runs the
// template as a non-deterministic automaton that may start and stop
// anywhere. Placeholders are approximated by their character class and
// maximum length.
func (c *Compiled) MatchFragment(text string) bool {
	type state struct{ i, n int } // at token i, n characters into it
	// closure adds a state and the states reachable without input: past a
	// placeholder that has its minimum, past a whitespace run already seen.
	var closure func(set map[state]bool, st state)
	closure = func(set map[state]bool, st state) {
		if set[st] {
			return
		}
		set[st] = true
		if st.i >= len(c.toks) {
			return
		}
		t := c.toks[st.i]
		if (t.kind == 'p' && st.n >= t.min) || (t.kind == 's' && st.n > 0) {
			closure(set, state{st.i + 1, 0})
		}
	}
	cur := map[state]bool{}
	for i, t := range c.toks {
		closure(cur, state{i, 0})
		if t.kind == 's' {
			closure(cur, state{i, 1}) // a part may begin inside a whitespace run
		}
	}
	for _, r := range text {
		next := map[state]bool{}
		for st := range cur {
			if st.i >= len(c.toks) {
				continue
			}
			t := c.toks[st.i]
			switch t.kind {
			case 'l':
				if r == t.ch {
					closure(next, state{st.i + 1, 0})
				}
			case 's':
				if isSpace(r) {
					closure(next, state{st.i, 1})
				}
			case 'p':
				if t.class(r) && st.n < t.max {
					closure(next, state{st.i, st.n + 1})
				}
			}
		}
		if len(next) == 0 {
			return false
		}
		cur = next
	}
	return true
}

// compiled caches matchers by body, shared by every account snapshot.
var compiled sync.Map // body -> *Compiled or error

func compileCached(body string) (*Compiled, error) {
	if v, ok := compiled.Load(body); ok {
		if c, ok := v.(*Compiled); ok {
			return c, nil
		}
		return nil, v.(error)
	}
	c, err := Compile(body)
	if err != nil {
		compiled.Store(body, err)
		return nil, err
	}
	compiled.Store(body, c)
	return c, nil
}

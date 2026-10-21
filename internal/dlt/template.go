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

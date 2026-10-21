package dlt

import (
	"regexp"
	"strings"
)

// Whitelist holds a template's allowed links, APK links and callback numbers.
type Whitelist struct {
	URLs      []string // normalised: host[/path], lower-case host, no scheme
	APKs      []string
	Callbacks []string // digits only, national (last 10 digits for mobiles)
}

// Add records one whitelist entry of kind url, apk or callback.
func (w *Whitelist) Add(kind, value string) {
	switch kind {
	case "url":
		w.URLs = append(w.URLs, normURL(value))
	case "apk":
		w.APKs = append(w.APKs, normURL(value))
	case "callback":
		w.Callbacks = append(w.Callbacks, normNumber(value))
	}
}

// Links in free text: with a scheme, starting www., or a bare domain on a
// common TLD (so "Rs.500" or "Mr.Rao" are not taken for links).
var linkRe = regexp.MustCompile(`(?i)\b(?:https?://\S+|www\.\S+|[a-z0-9][a-z0-9-]*(?:\.[a-z0-9-]+)*\.(?:com|in|co|net|org|io|ly|me|app|info|biz|xyz|gl|link|page|ai|to|cc|us|online|site|store|shop)(?:/\S*)?)`)

// Links returns the links found in s.
func Links(s string) []string {
	var out []string
	for _, l := range linkRe.FindAllString(s, -1) {
		out = append(out, strings.TrimRight(l, ".,;:!?)\"'"))
	}
	return out
}

func normURL(u string) string {
	u = strings.TrimSpace(u)
	lu := strings.ToLower(u)
	for _, p := range []string{"https://", "http://"} {
		if strings.HasPrefix(lu, p) {
			u, lu = u[len(p):], lu[len(p):]
		}
	}
	host, path, _ := strings.Cut(u, "/")
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	if path == "" {
		return host
	}
	return host + "/" + path
}

// allowed reports whether link is covered by an entry: the same host (no
// subdomains), and the entry's path, if it has one, is a prefix of the
// link's path ending at a path, query or fragment boundary.
func allowed(link string, entries []string) bool {
	n := normURL(link)
	for _, e := range entries {
		if e == "" || !strings.HasPrefix(n, e) {
			continue
		}
		if len(n) == len(e) || strings.HasSuffix(e, "/") || strings.ContainsRune("/?#", rune(n[len(e)])) {
			return true
		}
	}
	return false
}

func normNumber(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	// +91 / 0 prefixes on mobile numbers.
	if len(d) == 12 && strings.HasPrefix(d, "91") {
		d = d[2:]
	} else if len(d) == 11 && strings.HasPrefix(d, "0") {
		d = d[1:]
	}
	return d
}

// checkValues enforces the whitelist on a matched message's placeholder
// values: every link must be whitelisted (APK links as APKs), and every
// {#cbn#} value must be a whitelisted callback number.
func (w *Whitelist) checkValues(vals []Value) *Error {
	for _, v := range vals {
		for _, l := range Links(v.Text) {
			if strings.HasSuffix(strings.ToLower(strings.SplitN(normURL(l), "?", 2)[0]), ".apk") {
				if !allowed(l, w.APKs) {
					return fail(ReasonURL, "APK link %q is not whitelisted on the template", l)
				}
				continue
			}
			if !allowed(l, w.URLs) {
				return fail(ReasonURL, "link %q is not whitelisted on the template", l)
			}
		}
		if (v.Kind == KindURL || v.Kind == KindURLOTT) && len(Links(v.Text)) == 0 && strings.TrimSpace(v.Text) != "" {
			if !allowed(v.Text, w.URLs) {
				return fail(ReasonURL, "link %q is not whitelisted on the template", v.Text)
			}
		}
		if v.Kind == KindCallback {
			n := normNumber(v.Text)
			ok := false
			for _, c := range w.Callbacks {
				ok = ok || c == n
			}
			if !ok {
				return fail(ReasonCallback, "callback number %q is not whitelisted on the template", v.Text)
			}
		}
	}
	return nil
}

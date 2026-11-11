package client

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/browserutils/kooky"
	_ "github.com/browserutils/kooky/browser/all" // register Chrome/Firefox/Safari/Edge/Brave finders
)

// cookieDomain is the site whose cookies are lifted out of a browser's (large)
// store. It is used twice: as kooky's coarse traversal filter, and then again
// in isRenfeDomain, because kooky's filter is a substring test.
const cookieDomain = "renfe.com"

// isRenfeDomain reports whether a cookie's Domain really belongs to Renfe.
//
// kooky.DomainContains is strings.Contains, so it also matches a domain that
// merely embeds the string — "renfe.com.attacker.example", "notrenfe.com".
// A page on such a domain can set cookies named JSESSIONID and SSOInfo, and
// they would be lifted, pass the signed-in check, be stored, and then be sent
// to venta.renfe.com as the user's session: the CLI would operate inside a
// session someone else chose. Cookie domains are matched the way cookies are
// scoped — the domain itself or something under it — not by substring.
// isUnsupportedStore reports the failures that only mean "this browser is not
// here": its files do not exist, or kooky has no backend for it on this
// platform. Both are the normal state of most of the browsers it looks for, and
// counting them would put a number like 18 in front of a user whose real
// problem is something else entirely. A missing file is a proper sentinel; the
// other is several unexported errors all spelled "not implemented", so there
// the text is all there is to go on.
func isUnsupportedStore(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		strings.Contains(strings.ToLower(err.Error()), "not implemented")
}

func isRenfeDomain(domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	d = strings.TrimPrefix(d, ".") // browsers store a host-wide cookie as ".renfe.com"
	return d == cookieDomain || strings.HasSuffix(d, "."+cookieDomain)
}

// sessionCookie is the server-side session the whole booking site hangs off.
// It is HttpOnly, so page JavaScript cannot see it — reading the browser's
// decrypted store can.
const sessionCookie = "JSESSIONID"

// loginCookie marks a store whose session has actually signed in. JSESSIONID
// alone is handed to anonymous visitors too, so it is not evidence of a login.
const loginCookie = "SSOInfo"

type nameVal struct{ name, val string }

// parseCookieHeader splits a raw "n=v; n=v" Cookie header into its pairs,
// skipping anything malformed or valueless. Seeding the jar and checking for a
// signed-in session both read a header this way, and they have to agree on what
// counts as a cookie — a value-less "SSOInfo=" is not a login.
func parseCookieHeader(header string) []nameVal {
	var out []nameVal
	for _, part := range strings.Split(header, ";") {
		name, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || name == "" || val == "" {
			continue
		}
		out = append(out, nameVal{name, val})
	}
	return out
}

// BrowserSession is one browser's Renfe cookies, as lifted from its store.
type BrowserSession struct {
	Browser string // "chrome", "firefox"…; "" when the store did not say
	Cookie  string // the "n=v; n=v" header
}

// CookiesFromBrowser returns every browser holding what looks like a signed-in
// Renfe session, in traversal order — the easy path: you stay logged in in your
// everyday browser and the CLI lifts the session (HttpOnly cookies included,
// since this reads the decrypted store rather than page JS). browser filters to
// one of chrome|chromium|firefox|safari|edge|brave; "" reads every installed
// browser.
//
// All of them are returned, not just the first: a browser's cookie file keeps
// the last session it wrote long after the server has dropped it, and only the
// server can say which is still alive. The caller tries them in order.
func CookiesFromBrowser(browser string) ([]BrowserSession, error) {
	browser = strings.ToLower(strings.TrimSpace(browser))

	// Collect Renfe cookies grouped BY BROWSER. Cookies are kept per browser
	// rather than merged by name across all of them — a global last-write-wins
	// merge could splice a JSESSIONID from one browser with the SSOInfo of
	// another, producing an authed-looking but server-rejected session.
	type store struct {
		val   map[string]string
		order []string
	}
	stores := map[string]*store{}
	var storeOrder []string
	// A store that cannot be read must not abort the read of the one you use.
	// Most of those failures are uninteresting — kooky reports "not implemented"
	// for every browser backend that does not exist on this platform, and there
	// are many. The rest are worth repeating if nothing turns up at all, since
	// "log in first" is the wrong advice for a cookie store this process was
	// refused access to.
	var readErrs []error
	for c, err := range kooky.TraverseCookies(context.Background(), kooky.DomainContains(cookieDomain)) {
		if err != nil {
			if !isUnsupportedStore(err) {
				readErrs = append(readErrs, err)
			}
			continue
		}
		if c == nil || c.Name == "" || c.Value == "" || !isRenfeDomain(c.Domain) {
			continue
		}
		bname := ""
		if c.Browser != nil {
			bname = strings.ToLower(c.Browser.Browser())
		}
		if browser != "" && bname != browser {
			continue
		}
		s := stores[bname]
		if s == nil {
			s = &store{val: map[string]string{}}
			stores[bname] = s
			storeOrder = append(storeOrder, bname)
		}
		if _, ok := s.val[c.Name]; !ok {
			s.order = append(s.order, c.Name)
		}
		s.val[c.Name] = c.Value // last write wins within a single browser
	}

	var found []BrowserSession
	for _, bname := range storeOrder {
		s := stores[bname]
		pairs := make([]nameVal, 0, len(s.order))
		for _, n := range s.order {
			pairs = append(pairs, nameVal{n, s.val[n]})
		}
		if cookie := buildCookieHeader(pairs); CookieLooksAuthed(cookie) {
			found = append(found, BrowserSession{Browser: bname, Cookie: cookie})
		}
	}
	if len(found) > 0 {
		return found, nil
	}

	where := "your browser"
	if browser != "" {
		where = browser
	}
	if len(readErrs) > 0 {
		return nil, cookieStoreReadError{browser: where, causes: readErrs}
	}
	return nil, fmt.Errorf("no signed-in Renfe session found in %s — "+
		"log in at venta.renfe.com in that browser first "+
		"(or pass --from-browser <chrome|firefox|safari|edge|brave>)", where)
}

type cookieStoreReadError struct {
	browser string
	causes  []error
}

func (e cookieStoreReadError) Error() string {
	firstType := "unknown"
	if len(e.causes) > 0 {
		firstType = fmt.Sprintf("%T", e.causes[0])
	}
	return fmt.Sprintf("no signed-in Renfe session found in %s, and %d cookie store(s) could not be read (first error type: %s) — if this is the browser you use, check its read/keychain permissions; otherwise sign in at venta.renfe.com first", e.browser, len(e.causes), firstType)
}

func (e cookieStoreReadError) Unwrap() []error { return e.causes }

// CookieLooksAuthed reports whether a Cookie header carries both halves of a
// signed-in session. It is a cheap local check, not proof: only `renfe whoami`
// asks the server.
func CookieLooksAuthed(header string) bool {
	pairs := parseCookieHeader(header)
	has := func(name string) bool {
		for _, p := range pairs {
			if strings.EqualFold(p.name, name) {
				return true
			}
		}
		return false
	}
	return has(sessionCookie) && has(loginCookie)
}

// buildCookieHeader renders cookie name/value pairs as a "n1=v1; n2=v2" header.
// Pairs are emitted in a stable (sorted) order so the result is deterministic.
func buildCookieHeader(pairs []nameVal) string {
	sorted := append([]nameVal(nil), pairs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].name < sorted[j].name })
	parts := make([]string, 0, len(sorted))
	for _, p := range sorted {
		parts = append(parts, p.name+"="+p.val)
	}
	return strings.Join(parts, "; ")
}

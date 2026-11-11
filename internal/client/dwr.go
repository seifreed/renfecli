package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Renfe's booking backend is a Java app that exposes its service beans over DWR
// (Direct Web Remoting): the browser POSTs a line-oriented text body naming a
// bean + method, and the server answers with a JavaScript *program* that calls
// back into the page. This file speaks that protocol without a JS engine.

// dwrPath is the plaincall endpoint template: one path per bean.method pair.
const dwrPath = "/vol/dwr/call/plaincall/%s.%s.dwr"

// The scriptSessionId is DWR's CSRF token as much as its reverse-ajax routing
// key: the browser sends "<DWRSESSIONID cookie>/<page token>", and DWR rejects
// a call whose id does not carry the cookie as its prefix ("CSRF Security
// Error"). A session that has never touched the DWR endpoint has no such
// cookie, and DWR then waives the check — which is why the anonymous search
// path works with a made-up id.
const (
	// dwrSessionCookie is the cookie DWR's engine sets on first contact.
	dwrSessionCookie = "DWRSESSIONID"
	// dwrPageToken is our half of the id. DWR only ever echoes it back, so a
	// fixed value keeps requests reproducible.
	dwrPageToken = "renfecli"
	// dwrAnonScriptSessionID is used when no DWRSESSIONID cookie exists yet.
	dwrAnonScriptSessionID = "0123456789ABCDEF0123456789ABCDEF/" + dwrPageToken
)

// scriptSessionID builds the id for a call, binding it to the DWRSESSIONID
// cookie when the session has one.
func scriptSessionID(dwrCookie string) string {
	if dwrCookie == "" {
		return dwrAnonScriptSessionID
	}
	return dwrCookie + "/" + dwrPageToken
}

// dwrCall encodes one bean.method invocation. Renfe's beans take either no
// argument at all (the account lookups) or a single object; pass nil params for
// the former.
//
// DWR never inlines a value: each one is declared as its own `c0-eN` variable
// and referenced by name, including the members of a nested object or array.
// Variables are emitted in the order the browser emits them — children before
// the array that references them — so a request body is byte-for-byte
// reproducible against a captured one.
func dwrCall(bean, method, page, scriptSession string, params []DWRParam) string {
	var b strings.Builder
	b.WriteString("callCount=1\nwindowName=\n")
	fmt.Fprintf(&b, "c0-scriptName=%s\nc0-methodName=%s\nc0-id=0\n", bean, method)
	if len(params) > 0 {
		e := &dwrEnc{buf: &b}
		fmt.Fprintf(&b, "c0-param0=%s\n", e.object(params))
	}
	b.WriteString("batchId=0\ninstanceId=0\n")
	fmt.Fprintf(&b, "page=%s\n", url.QueryEscape(page))
	fmt.Fprintf(&b, "scriptSessionId=%s\n", scriptSession)
	return b.String()
}

// DWRParam is one field of a DWR object argument. Order matters (see dwrCall).
// Value holds a scalar, encoded with Type ("string" when Type is empty);
// Objects instead holds an array of objects, the only nested shape Renfe's
// beans use.
type DWRParam struct {
	Key     string
	Value   string
	Type    string
	Objects [][]DWRParam
}

// dwrStr is a plain string field, the overwhelmingly common case.
func dwrStr(key, value string) DWRParam { return DWRParam{Key: key, Value: value} }

// dwrBool is a field DWR must see as a boolean, not the string "true".
func dwrBool(key string, v bool) DWRParam {
	return DWRParam{Key: key, Value: boolStr(v), Type: "boolean"}
}

// dwrObjects is a field holding an array of objects.
func dwrObjects(key string, objs [][]DWRParam) DWRParam {
	return DWRParam{Key: key, Objects: objs}
}

// dwrEnc allocates the c0-eN variables and writes their declarations.
//
// It mirrors DWR's own allocation order: a composite value takes its variable
// name the moment it is reached, *before* its children are walked, but its
// declaration is written after theirs. That is why an array of one object
// numbers as e4=array, e5=object, e6…=the object's fields, and why the file
// lists e6…e15 first, then e5, then e4.
type dwrEnc struct {
	buf *strings.Builder
	n   int
}

// reserve claims the next variable name without writing anything.
func (e *dwrEnc) reserve() string {
	e.n++
	return fmt.Sprintf("c0-e%d", e.n)
}

// write emits one variable declaration.
func (e *dwrEnc) write(name, body string) { fmt.Fprintf(e.buf, "%s=%s\n", name, body) }

// scalar declares a scalar and returns its name.
func (e *dwrEnc) scalar(typ, val string) string {
	if typ == "" {
		typ = "string"
	}
	// Only strings are percent-encoded; a boolean is sent verbatim.
	if typ == "string" {
		val = dwrEscape(val)
	}
	name := e.reserve()
	e.write(name, typ+":"+val)
	return name
}

// object declares every member of params and returns the object literal that
// references them. It does not declare the object itself — the caller decides
// whether that is a variable or the c0-param0 argument.
func (e *dwrEnc) object(params []DWRParam) string {
	refs := make([]string, len(params))
	for i, p := range params {
		var ref string
		if p.Objects != nil {
			ref = e.arrayVar(p.Objects)
		} else {
			ref = e.scalar(p.Type, p.Value)
		}
		refs[i] = p.Key + ":reference:" + ref
	}
	return "Object_Object:{" + strings.Join(refs, ", ") + "}"
}

// arrayVar declares an array of objects — name reserved first, declaration
// written last — and returns its variable name.
func (e *dwrEnc) arrayVar(objs [][]DWRParam) string {
	name := e.reserve()
	refs := make([]string, len(objs))
	for i, o := range objs {
		elem := e.reserve()
		body := e.object(o) // declares the element's own fields
		e.write(elem, body)
		refs[i] = "reference:" + elem
	}
	// DWR joins array elements with a bare comma and object members with a comma
	// AND a space. The difference is invisible with a single element, and a
	// stray space here makes the server reject a multi-leg request with a
	// ConversionException — so the two separators are deliberately not shared.
	e.write(name, "array:["+strings.Join(refs, ",")+"]")
	return name
}

// dwrEscape percent-encodes a value the way the DWR client does: everything
// url.QueryEscape does, except that a space stays "%20" rather than becoming
// '+' (DWR's server-side decoder does not treat '+' as a space).
func dwrEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// dwrReplyMarker opens the payload in every successful reply:
//
//	r.handleCallback("0","0",{...});
//
// An exception instead arrives as r.handleException("0","0",{...}).
const (
	dwrReplyMarker = `r.handleCallback("0","0",`
	dwrErrMarker   = `r.handleException("0","0",`
)

// decodeDWR extracts the payload from a DWR reply script and unmarshals it into
// out. The payload is a JavaScript object literal, not JSON — its keys are bare
// identifiers — so it goes through jsObjectToJSON first.
func decodeDWR(script string, out any) error {
	if i := strings.Index(script, dwrErrMarker); i >= 0 {
		body, err := sliceCallArg(script, i+len(dwrErrMarker))
		if err != nil {
			return dwrExceptionError{}
		}
		return dwrExceptionError{sessionExpired: strings.Contains(body, "U014")}
	}
	i := strings.Index(script, dwrReplyMarker)
	if i < 0 {
		// Anything that is not a DWR script is an HTML page: a virtual waiting
		// room, an expired session, or a bot-check interstitial. They need very
		// different responses from the caller, so name the one we can recognise.
		if isQueuePage(script) {
			return ErrQueued
		}
		return errNotDWRReply
	}
	body, err := sliceCallArg(script, i+len(dwrReplyMarker))
	if err != nil {
		return err
	}
	j, err := jsObjectToJSON(body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(j), out); err != nil {
		return fmt.Errorf("decode dwr payload: %w", err)
	}
	return nil
}

type dwrExceptionError struct{ sessionExpired bool }

func (dwrExceptionError) Error() string { return "Renfe returned an application error" }

var errNotDWRReply = errors.New("renfe returned an unexpected response format")

// sliceCallArg returns the payload argument that starts at script[from]. Most
// beans answer with an object or array, but some return a bare value — the
// signed-in user's name arrives as a plain string literal — so all three shapes
// are handled. Brackets and parentheses inside string literals are skipped.
func sliceCallArg(script string, from int) (string, error) {
	if from >= len(script) {
		return "", fmt.Errorf("empty DWR payload")
	}
	switch script[from] {
	case '{', '[':
		return sliceBracketed(script, from)
	case '"':
		return sliceString(script, from)
	}
	// A bare literal (number, true/false/null) runs to the call's closing paren.
	if end := strings.IndexByte(script[from:], ')'); end > 0 {
		return strings.TrimSpace(script[from : from+end]), nil
	}
	return "", fmt.Errorf("truncated DWR payload")
}

// sliceBracketed returns the object or array literal starting at script[from].
func sliceBracketed(script string, from int) (string, error) {
	depth := 0
	inStr, esc := false, false
	for i := from; i < len(script); i++ {
		c := script[i]
		switch {
		case esc:
			esc = false
		case inStr && c == '\\':
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '{' || c == '[':
			depth++
		case c == '}' || c == ']':
			depth--
			if depth == 0 {
				return script[from : i+1], nil
			}
		}
	}
	return "", fmt.Errorf("truncated DWR payload")
}

// sliceString returns the string literal starting at script[from], quotes
// included, so the caller can hand it straight to encoding/json.
func sliceString(script string, from int) (string, error) {
	esc := false
	for i := from + 1; i < len(script); i++ {
		switch {
		case esc:
			esc = false
		case script[i] == '\\':
			esc = true
		case script[i] == '"':
			return script[from : i+1], nil
		}
	}
	return "", fmt.Errorf("unterminated string in DWR payload")
}

// jsObjectToJSON rewrites a DWR object literal into JSON: its only deviation
// from JSON is that object keys are bare identifiers (`{horaSalida:"06:27"}`),
// so quoting those is the whole job. The scan is string-aware, since a value
// like "AVE: Madrid-Barcelona" would otherwise look like a key, and it tracks
// the enclosing brackets so an array's commas are not mistaken for key
// separators.
//
// DWR emits `null` for absent values, never `undefined`, and dates come through
// as strings, so no other JS-only literal needs translating.
func jsObjectToJSON(s string) (string, error) {
	var b strings.Builder
	b.Grow(len(s) + len(s)/8)
	var stack []byte // open brackets, innermost last
	inStr, esc := false, false
	// keyPos is true where a bare identifier may legally appear: right after an
	// object's '{' or one of its ',' separators.
	keyPos := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			b.WriteByte(c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch {
		case c == '"':
			inStr = true
			keyPos = false
			b.WriteByte(c)
		case c == '{' || c == '[':
			stack = append(stack, c)
			keyPos = c == '{'
			b.WriteByte(c)
		case c == '}' || c == ']':
			if len(stack) == 0 {
				return "", fmt.Errorf("unbalanced %q in DWR payload", c)
			}
			stack = stack[:len(stack)-1]
			keyPos = false
			b.WriteByte(c)
		case c == ',':
			keyPos = len(stack) > 0 && stack[len(stack)-1] == '{'
			b.WriteByte(c)
		case c == ' ' || c == '\n' || c == '\t' || c == '\r':
			b.WriteByte(c)
		case keyPos && isIdentStart(c):
			j := i
			for j < len(s) && isIdentPart(s[j]) {
				j++
			}
			b.WriteByte('"')
			b.WriteString(s[i:j])
			b.WriteByte('"')
			i = j - 1
			keyPos = false
		default:
			keyPos = false
			b.WriteByte(c)
		}
	}
	if inStr {
		return "", fmt.Errorf("unterminated string in DWR payload")
	}
	if len(stack) != 0 {
		return "", fmt.Errorf("truncated DWR payload")
	}
	return b.String(), nil
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }

// ErrQueued reports that Renfe put the request in its virtual waiting room.
// Renfe fronts the booking site with Queue-it when demand spikes — a new
// timetable opening, a promotion — and everyone, browser or not, is held in a
// queue. Retrying immediately does not help and only lengthens the wait, so
// this is surfaced as its own error instead of being retried.
var ErrQueued = errors.New("renfe has put this request in its virtual waiting room (queue-it) — " +
	"the site is under heavy load; wait and try again, or join the queue at venta.renfe.com in a browser")

// queueMarkers are the fingerprints Queue-it leaves on an interstitial. The
// booking pages load its client script too, so a marker is only meaningful on
// a body that is not a DWR reply (see decodeDWR).
var queueMarkers = []string{"queue-it.net", "queueittoken", "Queue-it", "waitingroom"}

func isQueuePage(body string) bool {
	for _, m := range queueMarkers {
		if strings.Contains(body, m) {
			return true
		}
	}
	return false
}

// dwrPrice parses a DWR money string ("49,8", "1.234,50") into euros. Renfe
// sends prices comma-decimal; an empty or unparseable value yields 0.
func dwrPrice(s string) float64 {
	s = strings.TrimSpace(strings.ReplaceAll(s, ".", ""))
	s = strings.ReplaceAll(s, ",", ".")
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

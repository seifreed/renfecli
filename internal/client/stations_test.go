package client

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"
)

// A slice of the real catalogue, kept in the file's ISO-8859-1 spirit by using
// the accented names the API actually returns.
var testStations = []Station{
	{Code: "MADRI", Name: "MADRID (TODAS)", Flat: "MADRID (TODAS)", Priority: 1},
	{Code: "60000", Name: "MADRID-PUERTA DE ATOCHA-ALMUDENA GRANDES", Flat: "MADRID-PUERTA DE ATOCHA-ALMUDENA GRANDES", Priority: 2},
	{Code: "71801", Name: "BARCELONA-SANTS", Flat: "BARCELONA-SANTS", Priority: 4},
	{Code: "50500", Name: "CÓRDOBA", Flat: "CORDOBA", Priority: 9},
	{Code: "03216", Name: "VALÈNCIA-JOAQUÍN SOROLLA", Flat: "VALENCIA-JOAQUIN SOROLLA", Priority: 11},
	{Code: "35406", Name: "VALENCIA DE ALCÁNTARA", Flat: "VALENCIA DE ALCANTARA", Priority: 316},
}

func TestParseStations(t *testing.T) {
	// The catalogue is ISO-8859-1 and holds a second array we must not read.
	js := []byte("var estacionesEstatico=[" +
		"{\"cdgoEstacion\":\"50500\",\"nmroPrioridad\":9,\"desgEstacion\":\"C\xd3RDOBA\"," +
		"\"cdgoUic\":\"50500\",\"desgEstacionPlano\":\"CORDOBA\"}," +
		"{\"cdgoEstacion\":\"71801\",\"nmroPrioridad\":4,\"desgEstacion\":\"BARCELONA-SANTS\"," +
		"\"cdgoUic\":null,\"desgEstacionPlano\":\"BARCELONA-SANTS\"}" +
		"];\nvar estacionesDestacada=[{\"cdgoEstacion\":\"99999\",\"desgEstacion\":\"NOPE\"}];")
	got, err := parseStations(js)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d stations, want 2 (the second array must be ignored)", len(got))
	}
	if got[0].Name != "CÓRDOBA" {
		t.Errorf("latin-1 name decoded as %q, want CÓRDOBA", got[0].Name)
	}
	if got[0].Priority != 9 {
		t.Errorf("priority = %d, want 9", got[0].Priority)
	}
}

func TestParseStationsRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "var x = 1;", "var estacionesEstatico=[{oops"} {
		if _, err := parseStations([]byte(in)); err == nil {
			t.Errorf("parseStations(%q) = nil error, want failure", in)
		}
	}
}

func TestFindStationsRanking(t *testing.T) {
	tests := []struct {
		query, wantCode string
	}{
		// An exact code wins outright.
		{"71801", "71801"},
		// A whole-name match beats a longer name that merely contains it, so the
		// group station is what "madrid" resolves to.
		{"madrid", "MADRI"},
		// Accents are folded both ways.
		{"cordoba", "50500"},
		{"CÓRDOBA", "50500"},
		// Renfe's own priority breaks ties, keeping the main city station on
		// top instead of a like-named village halt.
		{"valencia", "03216"},
		{"barcelona-sants", "71801"},
	}
	for _, tc := range tests {
		got := FindStations(testStations, tc.query)
		if len(got) == 0 {
			t.Errorf("FindStations(%q) found nothing", tc.query)
			continue
		}
		if got[0].Code != tc.wantCode {
			t.Errorf("FindStations(%q)[0] = %s (%s), want %s", tc.query, got[0].Code, got[0].Name, tc.wantCode)
		}
	}
}

func TestFindStationsEmptyQuery(t *testing.T) {
	if got := FindStations(testStations, "   "); got != nil {
		t.Errorf("a blank query should match nothing, got %d", len(got))
	}
}

func TestResolveStation(t *testing.T) {
	s, err := ResolveStation(testStations, "barcelona")
	if err != nil {
		t.Fatal(err)
	}
	if s.Code != "71801" {
		t.Errorf("resolved to %s, want 71801", s.Code)
	}
	if _, err := ResolveStation(testStations, "atlantis"); err == nil {
		t.Error("an unknown station should be an error, not a silent miss")
	}
}

func TestCookieLooksAuthed(t *testing.T) {
	// JSESSIONID alone is handed to anonymous visitors, so it is not enough.
	for header, want := range map[string]bool{
		"JSESSIONID=abc; SSOInfo=xyz":  true,
		"ssoinfo=xyz; jsessionid=abc":  true,
		"JSESSIONID=abc":               false,
		"SSOInfo=xyz":                  false,
		"JSESSIONID=; SSOInfo=xyz":     false,
		"":                             false,
		"OptanonConsent=1; TS018d34ad": false,
	} {
		if got := CookieLooksAuthed(header); got != want {
			t.Errorf("CookieLooksAuthed(%q) = %v, want %v", header, got, want)
		}
	}
}

func TestBuildCookieHeaderIsDeterministic(t *testing.T) {
	pairs := []nameVal{{"z", "1"}, {"a", "2"}, {"m", "3"}}
	want := "a=2; m=3; z=1"
	if got := buildCookieHeader(pairs); got != want {
		t.Errorf("buildCookieHeader = %q, want %q", got, want)
	}
}

func TestCardLevel(t *testing.T) {
	for in, want := range map[string]string{"1": "Básica", "4": "Plata", "9": "9", "": ""} {
		if got := cardLevel(in); got != want {
			t.Errorf("cardLevel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFold(t *testing.T) {
	for in, want := range map[string]string{
		"MADRID-PUERTA DE ATOCHA": "madrid puerta de atocha",
		"  Córdoba  ":             "cordoba",
		"VALÈNCIA-JOAQUÍN":        "valencia joaquin",
		"A CORUÑA":                "a coruna",
	} {
		if got := fold(in); got != want {
			t.Errorf("fold(%q) = %q, want %q", in, got, want)
		}
	}
}

// Renfe lists its French and Portuguese destinations under their local names,
// so a Spanish speaker asking for "Marsella" would be told no such station
// exists. Only places actually served are aliased.
func TestExonyms(t *testing.T) {
	foreign := []Station{
		{Code: "87089", Name: "MARSEILLE ST CHARLES", Flat: "MARSEILLE ST CHARLES", Priority: 400},
		{Code: "87374", Name: "PERPIGNAN", Flat: "PERPIGNAN", Priority: 401},
		{Code: "87814", Name: "AVIGNON TGV", Flat: "AVIGNON TGV", Priority: 402},
		{Code: "94346", Name: "PORTO CAMPANHA - O PORTO CAMPAÑA", Flat: "PORTO CAMPANHA - O PORTO CAMPANA", Priority: 403},
	}
	for query, wantCode := range map[string]string{
		"marsella":  "87089",
		"Marsella":  "87089",
		"marseille": "87089", // the local spelling must keep working
		"perpiñán":  "87374",
		"aviñón":    "87814",
		"oporto":    "94346",
	} {
		got := FindStations(foreign, query)
		if len(got) == 0 || got[0].Code != wantCode {
			t.Errorf("FindStations(%q) = %v, want %s", query, got, wantCode)
		}
	}
	// A place Renfe does not serve must not be aliased onto something else.
	if got := FindStations(foreign, "lisboa"); len(got) != 0 {
		t.Errorf("lisboa is not in the catalogue, matched %v", got)
	}
}

func TestSearchRejectsSameOriginAndDestination(t *testing.T) {
	_, err := New().Search(Query{
		Origin: "MADRI", Destination: "MADRI", OriginName: "MADRID (TODAS)",
		Date: time.Now(),
	})
	if err == nil {
		t.Fatal("A→A should fail before any request")
	}
	if !strings.Contains(err.Error(), "same station") {
		t.Errorf("error should name the real cause, got %q", err)
	}
}

// kooky's DomainContains is strings.Contains, so the traversal filter alone
// also matches a domain that merely embeds "renfe.com". A page there can set
// cookies called JSESSIONID and SSOInfo; lifted and stored, the CLI would run
// inside a session someone else chose.
func TestOnlyRealRenfeDomainsAreLifted(t *testing.T) {
	for domain, want := range map[string]bool{
		"renfe.com":                   true,
		".renfe.com":                  true,
		"venta.renfe.com":             true,
		".venta.renfe.com":            true,
		"RENFE.COM":                   true,
		"renfe.com.attacker.example":  false, // the one that matters
		".renfe.com.attacker.example": false,
		"notrenfe.com":                false,
		"myrenfe.com":                 false,
		"renfe.com.co":                false,
		"renfe.company":               false,
		"evil.example":                false,
		"":                            false,
	} {
		if got := isRenfeDomain(domain); got != want {
			t.Errorf("isRenfeDomain(%q) = %v, want %v", domain, got, want)
		}
	}
}

// The lift walks every browser kooky knows, and most of them are not installed.
// Those failures must not be counted as something the user can act on, or the
// message that fires when a session is genuinely missing leads with a number
// like 18 and an error about Brave.
func TestUnsupportedStoresAreNotCountedAsFailures(t *testing.T) {
	benign := []error{
		errors.New("cookie store: not implemented"),
		errors.New("DPAPI method not implemented on this platform"),
		fmt.Errorf("cookie store: open /Users/x/Brave/Local State: %w", fs.ErrNotExist),
	}
	for _, err := range benign {
		if !isUnsupportedStore(err) {
			t.Errorf("counted a missing browser as a failure: %v", err)
		}
	}
	worthReporting := []error{
		errors.New("failed to decrypt: keychain access denied"),
		errors.New("cookie store: permission denied"),
	}
	for _, err := range worthReporting {
		if isUnsupportedStore(err) {
			t.Errorf("hid a failure worth reporting: %v", err)
		}
	}
}

func TestCookieStoreReadErrorRedactsPathAndUnwrapsCauses(t *testing.T) {
	cause := fmt.Errorf("cannot read /Users/alice/Library/Cookies: %w", fs.ErrPermission)
	err := cookieStoreReadError{browser: "chrome", causes: []error{cause}}
	if strings.Contains(err.Error(), "/Users/alice") || strings.Contains(err.Error(), "Cookies:") {
		t.Fatalf("cookie store error exposed local path: %v", err)
	}
	if !strings.Contains(err.Error(), "first error type: *fmt.wrapError") {
		t.Fatalf("cookie store error lacks a safe diagnostic type: %v", err)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("cookie store error did not preserve cause: %v", err)
	}
}

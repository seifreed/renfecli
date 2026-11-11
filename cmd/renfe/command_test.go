package main

import (
	"encoding/json"
	"fmt"
	"github.com/seifreed/renfecli/internal/store"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/seifreed/renfecli/internal/client"
	"github.com/seifreed/renfecli/internal/config"
)

// These exercise a command end to end — flag parsing, the station lookup, the
// two-request search sequence, filtering and the view — against a stand-in for
// venta.renfe.com replaying a captured reply. Everything between os.Args and
// stdout is real; only the network is not.

func TestSearchCommandEndToEnd(t *testing.T) {
	withRenfe(t, replaying(t, "trainslist.dwr"))

	out, err := capture(t, func() error {
		return cmdSearch([]string{"madrid", "barcelona", "--date", "+2", "--limit", "3"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "MADRID") || !strings.Contains(out, "BARCELONA") {
		t.Errorf("the route header is missing:\n%s", out)
	}
	if n := strings.Count(out, "→"); n < 2 {
		t.Errorf("--limit 3 should still print journeys, got:\n%s", out)
	}
	if !strings.Contains(out, "cheapest nearby:") {
		t.Errorf("the price-calendar line should ride along with a search:\n%s", out)
	}
}

func TestStationCacheHitIsObservableWithoutStationData(t *testing.T) {
	withRenfe(t, replaying(t, "trainslist.dwr"))
	t.Setenv("RENFE_LOG_FORMAT", "json")
	t.Setenv("RENFE_LOG_LEVEL", "debug")
	stderr := captureStderr(t, func() {
		if _, err := loadStations(newClient(config.Config{}), false); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(stderr, `"msg":"station catalogue cache hit"`) ||
		!strings.Contains(stderr, `"station_count":2`) || strings.Contains(stderr, "BARCELONA") {
		t.Fatalf("unexpected station cache event: %s", stderr)
	}
}

// --json is the contract agents depend on, so it must stay parseable and carry
// the fields the human view shows.
func TestSearchCommandJSON(t *testing.T) {
	withRenfe(t, replaying(t, "trainslist.dwr"))

	out, err := capture(t, func() error {
		return cmdSearch([]string{"madrid", "barcelona", "--json", "--limit", "2"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var res client.Results
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("--json emitted something unparseable: %v\n%s", err, out)
	}
	if len(res.Journeys) != 2 {
		t.Fatalf("--limit 2 should cap the list, got %d", len(res.Journeys))
	}
	if res.Journeys[0].Departure == "" || res.Journeys[0].Price == 0 {
		t.Errorf("a journey came through hollow: %+v", res.Journeys[0])
	}
	if res.Return != nil {
		t.Error("a one-way search must not carry a return leg")
	}
}

// A round trip arrives in one reply; both directions have to survive the CLI.
func TestSearchCommandRoundTrip(t *testing.T) {
	withRenfe(t, replaying(t, "roundtrip.dwr"))

	out, err := capture(t, func() error {
		return cmdSearch([]string{"madrid", "barcelona", "--return", "+3", "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var res client.Results
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.Return == nil || len(res.Return.Journeys) == 0 {
		t.Fatal("the return leg did not reach the caller")
	}
}

func TestCalendarCommand(t *testing.T) {
	withRenfe(t, replaying(t, "trainslist.dwr"))

	out, err := capture(t, func() error {
		return cmdCalendar([]string{"madrid", "barcelona", "--cheapest"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "← cheapest") {
		t.Errorf("--cheapest should print the one best day:\n%s", out)
	}
	if lines := strings.Count(strings.TrimSpace(out), "\n"); lines > 2 {
		t.Errorf("--cheapest printed the whole strip:\n%s", out)
	}
}

func TestStopsCommand(t *testing.T) {
	// `stops` searches first and then asks about one of the results, so the two
	// beans answer from two different captures.
	trains, recorrido := fixture(t, "trainslist.dwr"), fixture(t, "recorrido.dwr")
	withRenfe(t, renfeServer(t, func(path string) []byte {
		if strings.Contains(path, "getRecorrido") {
			return recorrido
		}
		return trains
	}))

	out, err := capture(t, func() error {
		return cmdStops([]string{"madrid", "barcelona"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(salida)") || !strings.Contains(out, "(llegada)") {
		t.Errorf("the itinerary did not print:\n%s", out)
	}
}

func TestStationsCommandUsesTheCache(t *testing.T) {
	withRenfe(t, replaying(t, "trainslist.dwr"))

	out, err := capture(t, func() error { return cmdStations([]string{"barcelona"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "71801") {
		t.Errorf("the station lookup missed the cached entry:\n%s", out)
	}
	if _, err := capture(t, func() error { return cmdStations([]string{"nowhere-at-all"}) }); err == nil {
		t.Error("no match must exit non-zero, as grep does")
	}
}

// --json is a contract with scripts and agents. A Go nil slice marshals to
// null, so a journey with no fare buckets — every sold-out train — used to
// emit "fares": null and break any caller that iterated it. Every array in
// the payload must be an array.
func TestJSONNeverEmitsANullArray(t *testing.T) {
	for _, fx := range []string{"trainslist.dwr", "roundtrip.dwr"} {
		t.Run(fx, func(t *testing.T) {
			withRenfe(t, replaying(t, fx))
			out, err := capture(t, func() error {
				return cmdSearch([]string{"madrid", "barcelona", "--json", "--fares"})
			})
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(out), &payload); err != nil {
				t.Fatal(err)
			}
			for _, path := range nullPaths(payload, "") {
				t.Errorf("null at %s — a caller iterating it would fail", path)
			}
		})
	}
}

// nullPaths walks a decoded payload and reports where a JSON null sits.
func nullPaths(v any, path string) []string {
	var found []string
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			if vv == nil {
				found = append(found, path+"/"+k)
				continue
			}
			found = append(found, nullPaths(vv, path+"/"+k)...)
		}
	case []any:
		for i, vv := range x {
			found = append(found, nullPaths(vv, fmt.Sprintf("%s[%d]", path, i))...)
		}
	}
	return found
}

// Renfe sends a price strip whose days can all be sold out. cheapestDay then
// reports the zero PriceDay, and --cheapest printed it as a day: an empty date
// at 0 €, which in --json is a record an agent would happily book against.
func TestCalendarCheapestWithNothingOnSale(t *testing.T) {
	reply := map[string]any{"listadoTrenes": []any{map[string]any{
		"viajeIda":                   true,
		"descripcionEstacionOrigen":  "MADRID",
		"descripcionEstacionDestino": "BARCELONA",
		"fechaOrigen":                "04/10/2026",
		"listviajeViewEnlaceBean": []any{map[string]any{
			"horaSalida": "06:00", "horaLlegada": "09:00", "completo": true,
		}},
		"resultPriceCalendar": map[string]any{"journeysPriceCalendar": []any{
			map[string]any{"date": "2026-10-01", "minPrice": 0, "minPriceAvailable": false},
			map[string]any{"date": "2026-10-02", "minPrice": 0, "minPriceAvailable": false},
		}},
	}}}
	body := dwrPayload(t, reply)
	withRenfe(t, renfeServer(t, func(string) []byte { return body }))

	out, err := capture(t, func() error {
		return cmdCalendar([]string{"madrid", "barcelona", "--cheapest"})
	})
	if err == nil {
		t.Fatalf("a sold-out strip must be reported, not printed as a day; got:\n%s", out)
	}
	if !strings.Contains(err.Error(), "on sale") {
		t.Errorf("unhelpful message: %v", err)
	}

	// Without --cheapest the strip is still worth showing: it says which days
	// exist, even though none of them can be bought.
	out, err = capture(t, func() error {
		return cmdCalendar([]string{"madrid", "barcelona"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2026-10-01") || strings.Contains(out, "← cheapest") {
		t.Errorf("the full strip should list the days and mark none:\n%s", out)
	}
}

// `session keep` accepted --json and --toon and then wrote nothing machine-
// readable at all: the flag only silenced the human ticker, and the "done" it
// printed on the way out landed on the same stdout a caller was parsing.
func TestSessionKeepEmitsARecordPerRefresh(t *testing.T) {
	t.Setenv("RENFE_CONFIG_DIR", t.TempDir())
	t.Setenv("RENFE_BASE_URL", sessionServer(t, "MARC RIVERO", 25*time.Minute).URL)
	if err := store.Save(sessionFile, session{Cookie: "JSESSIONID=x; SSOInfo=y"}); err != nil {
		t.Fatal(err)
	}

	out, err := capture(t, func() error {
		return sessionKeep([]string{"--json", "--for", "60ms"})
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if strings.Contains(out, "done") || strings.Contains(out, "stopped") {
		t.Errorf("human chatter reached the structured stream:\n%s", out)
	}
	var rec struct {
		SignedIn bool   `json:"signed_in"`
		Seconds  int    `json:"seconds_remaining"`
		Name     string `json:"name"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("no parseable record on stdout: %v\n%s", err, out)
	}
	if !rec.SignedIn || rec.Name != "MARC RIVERO" || rec.Seconds <= 0 {
		t.Errorf("hollow record: %+v", rec)
	}
}

// sessionServer answers the two beans a keep-alive touches.
func sessionServer(t *testing.T, name string, left time.Duration) *httptest.Server {
	t.Helper()
	who := dwrPayload(t, name)
	ms := dwrPayload(t, left.Milliseconds())
	return renfeServer(t, func(path string) []byte {
		if strings.Contains(path, "checkSession") {
			return ms
		}
		return who
	})
}

// `session status` downgraded a failed login lookup to a warning and then
// printed the "NOT signed in … the login has expired" branch, which sends the
// user off to log in again over what may be a network blip. Not knowing and
// knowing the login is gone are different answers.
func TestSessionStatusDoesNotBlameTheLoginForAFailedLookup(t *testing.T) {
	t.Setenv("RENFE_CONFIG_DIR", t.TempDir())
	ms := dwrPayload(t, (25 * time.Minute).Milliseconds())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "checkSession") {
			_, _ = w.Write(ms)
			return
		}
		w.WriteHeader(http.StatusInternalServerError) // the login lookup falls over
	}))
	t.Cleanup(srv.Close)
	t.Setenv("RENFE_BASE_URL", srv.URL)
	if err := store.Save(sessionFile, session{Cookie: "JSESSIONID=x; SSOInfo=y"}); err != nil {
		t.Fatal(err)
	}

	out, err := capture(t, func() error { return sessionStatus(nil) })
	if err == nil {
		t.Fatalf("a failed lookup must not read as success:\n%s", out)
	}
	if strings.Contains(err.Error(), "expired") || strings.Contains(out, "expired") {
		t.Errorf("blamed the login for a server error: %v\n%s", err, out)
	}
	if !strings.Contains(err.Error(), "could not be checked") {
		t.Errorf("unclear message: %v", err)
	}
}

// Everything printed about a journey is text the server chose. A terminal
// executes some of those bytes: CSI repaints the screen, OSC sets the window
// title and on some terminals writes the clipboard, CR overwrites the line the
// user just read, and a newline forges a whole row in a listing an agent is
// parsing line by line. None of it may reach the terminal intact.
func TestServerTextCannotDriveTheTerminal(t *testing.T) {
	const esc = "\x1b[2J\x1b[31mCLEARED\x1b[0m"
	reply := map[string]any{"listadoTrenes": []any{map[string]any{
		"viajeIda":                   true,
		"descripcionEstacionOrigen":  "MADRID" + esc,
		"descripcionEstacionDestino": "BARCELONA",
		"fechaOrigen":                "04/10/2026",
		"listviajeViewEnlaceBean": []any{map[string]any{
			"horaSalida": "06:00", "horaLlegada": "09:00", "tarifaMinima": "50,00",
			"tipoTrenUno": "AVE", "directo": false,
			"msgCambioEstacionTransbordo": "cambio\x1b]0;title\x07 de estación",
			"trayectos":                   []any{map[string]any{"cdgoTren": "03311"}},
			"tarifasDisponibles": []any{map[string]any{
				"titulo": "Bás\x07ico\r\n06:00 → 09:00  FORGED ROW", "precioTarifa": "50,00", "cdgoClase": "T"}},
		}},
	}}}
	body := dwrPayload(t, reply)

	for _, flags := range [][]string{{"--fares"}, {"--toon"}} {
		withRenfe(t, renfeServer(t, func(string) []byte { return body }))
		out, err := capture(t, func() error {
			return cmdSearch(append([]string{"madrid", "barcelona"}, flags...))
		})
		if err != nil {
			t.Fatalf("%v: %v", flags, err)
		}
		for name, seq := range map[string]string{
			"ESC": "\x1b", "BEL": "\x07", "CR": "\r",
		} {
			if strings.Contains(out, seq) {
				t.Errorf("%v: a %s from the server reached the terminal: %q", flags, name, out)
			}
		}
		if strings.Contains(out, "FORGED ROW\n") && !strings.Contains(out, "FORGED ROW    ") {
			t.Errorf("%v: a fare title broke onto its own line: %q", flags, out)
		}
		// The legible part of the text must survive — this strips control
		// characters, not accents or anything else printable.
		if !strings.Contains(out, "MADRID") || !strings.Contains(out, "estación") {
			t.Errorf("%v: sanitising ate real text: %q", flags, out)
		}
	}
}

// The error path prints the server's words too — an HTTP error body is remote
// text — while the CLI's own messages legitimately span lines.
func TestErrorTextIsStrippedButKeepsItsLines(t *testing.T) {
	// The introducer is what matters: without ESC (or the 8-bit C1 CSI at
	// 0x9b) a terminal never enters escape parsing, so the "[2J" left behind is
	// inert text. Stripping the introducer is provable; parsing sequences to
	// remove the rest is the kind of filter that gets bypassed.
	got := safeText("renfe said\x1b[2J this\r\nand that\u009bK")
	if strings.ContainsAny(got, "\x1b\r\u009b") {
		t.Errorf("safeText left an escape introducer: %q", got)
	}
	if !strings.Contains(got, "\n") {
		t.Errorf("safeText ate the line break a CLI message needs: %q", got)
	}
	if got := safeField("MADRID\nBARCELONA\x07"); got != "MADRIDBARCELONA" {
		t.Errorf("safeField = %q", got)
	}
	if got := safeField("CÓRDOBA-CENTRAL (TODAS)"); got != "CÓRDOBA-CENTRAL (TODAS)" {
		t.Errorf("safeField mangled ordinary text: %q", got)
	}
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/seifreed/renfecli/internal/store"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestCorruptStationCacheIsWarnedWithoutExposingContents(t *testing.T) {
	t.Setenv("RENFE_CONFIG_DIR", t.TempDir())
	t.Setenv("RENFE_LOG_FORMAT", "json")
	t.Setenv("RENFE_LOG_LEVEL", "debug")
	const sentinel = "private-station-cache-value"
	if err := os.WriteFile(store.Path(stationsCache), []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer srv.Close()
	cl := client.New()
	cl.BaseURL = srv.URL
	cl.HTTP = &http.Client{Timeout: time.Second}
	stderr := captureStderr(t, func() {
		_, _ = loadStations(cl, false)
	})
	if !strings.Contains(stderr, `"msg":"station catalogue cache could not be read"`) || strings.Contains(stderr, sentinel) {
		t.Fatalf("corrupt cache was not reported safely: %s", stderr)
	}
}

func TestStationCacheWriteFailureIsWarnedAndDoesNotFailLoad(t *testing.T) {
	dir := t.TempDir()
	configPath := dir + "/not-a-directory"
	if err := os.WriteFile(configPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RENFE_CONFIG_DIR", configPath)
	t.Setenv("RENFE_LOG_FORMAT", "json")
	t.Setenv("RENFE_LOG_LEVEL", "debug")
	cl := client.New()
	cl.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `var estacionesEstatico=[{"cdgoEstacion":"60000","desgEstacion":"MADRID","desgEstacionPlano":"MADRID"}];`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	var stations []client.Station
	stderr := captureStderr(t, func() {
		var err error
		stations, err = loadStations(cl, false, "correlation-test")
		if err != nil {
			t.Errorf("cache failure made fresh catalogue unusable: %v", err)
		}
	})
	if len(stations) != 1 || stations[0].Code != "60000" {
		t.Fatalf("fresh catalogue was lost after cache write failure: %+v", stations)
	}
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid structured event: %v\n%s", err, line)
		}
		if event["event_name"] == "station_catalogue_cache_write_failed" {
			found = true
			if event["level"] != "WARN" || event["error_type"] == nil || event["error_code"] != "STORE_WRITE_FAILED" || event["correlation_id"] != "correlation-test" {
				t.Errorf("cache write warning lacks safe diagnostic fields: %#v", event)
			}
		}
	}
	if !found {
		t.Fatalf("cache write failure was not observable: %s", stderr)
	}
}

func TestStationFetchFailureIsLeftToCommandErrorBoundary(t *testing.T) {
	t.Setenv("RENFE_CONFIG_DIR", t.TempDir())
	t.Setenv("RENFE_LOG_FORMAT", "json")
	t.Setenv("RENFE_LOG_LEVEL", "debug")
	cl := client.New()
	cl.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("private upstream response")), Request: req}, nil
	})}
	var loadErr error
	stderr := captureStderr(t, func() {
		_, loadErr = loadStations(cl, false, "correlation-test")
	})
	if loadErr == nil {
		t.Fatal("station fetch unexpectedly succeeded")
	}
	if status, ok := client.HTTPStatus(loadErr); !ok || status != http.StatusForbidden {
		t.Fatalf("HTTP status was lost: %v", loadErr)
	}
	if stderr != "" {
		t.Fatalf("terminal failure logged here as well as at the command boundary: %s", stderr)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

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

func TestRunCorrelationIDJoinsCommandAndHTTPEvents(t *testing.T) {
	withRenfe(t, replaying(t, "trainslist.dwr"))
	t.Setenv("RENFE_LOG_FORMAT", "json")
	t.Setenv("RENFE_LOG_LEVEL", "debug")
	var stdout string
	var runErr error
	stderr := captureStderr(t, func() {
		stdout, runErr = capture(t, func() error {
			if code := run([]string{"search", "madrid", "barcelona", "--limit", "1", "--json"}); code != 0 {
				return fmt.Errorf("run returned %d", code)
			}
			return nil
		})
	})
	if runErr != nil {
		t.Fatalf("command failed: %v\n%s", runErr, stdout)
	}
	var commandID string
	var httpEvents int
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid structured event: %v\n%s", err, line)
		}
		id, _ := event["correlation_id"].(string)
		switch event["event_name"] {
		case "command_started", "command_completed":
			if id == "" || (commandID != "" && commandID != id) {
				t.Fatalf("command events have missing or inconsistent correlation IDs: %#v", event)
			}
			commandID = id
		case "http_request_completed":
			httpEvents++
			if id == "" || (commandID != "" && id != commandID) {
				t.Fatalf("HTTP event not correlated to command: %#v", event)
			}
			commandID = id
		case "station_catalogue_cache_hit":
			if id == "" || (commandID != "" && id != commandID) {
				t.Fatalf("storage event not correlated to command: %#v", event)
			}
			commandID = id
		}
	}
	if commandID == "" || httpEvents == 0 {
		t.Fatalf("expected command and HTTP events with a shared ID; stderr=%s", stderr)
	}
}

func TestRunHTTPFailureLogsStatusAndCorrelationWithoutRemoteBody(t *testing.T) {
	const privateBody = "private upstream response"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(privateBody))
	}))
	defer srv.Close()
	withRenfe(t, srv)
	t.Setenv("RENFE_LOG_FORMAT", "json")
	t.Setenv("RENFE_LOG_LEVEL", "info")
	var code int
	stderr := captureStderr(t, func() { code = run([]string{"search", "madrid", "barcelona"}) })
	if code != 1 {
		t.Fatalf("run returned %d, want failed operation", code)
	}
	var startID string
	var failed map[string]any
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue // the command also prints a safe human error to stderr
		}
		switch event["event_name"] {
		case "command_started":
			startID, _ = event["correlation_id"].(string)
		case "command_failed":
			failed = event
		}
	}
	if failed == nil {
		t.Fatalf("failed command event missing: %s", stderr)
	}
	if failed["http_status"] != float64(http.StatusForbidden) || failed["error_code"] != "HTTP_403" {
		t.Errorf("failure event lacks HTTP classification: %#v", failed)
	}
	if startID == "" || failed["correlation_id"] != startID {
		t.Errorf("failure event correlation mismatch: start=%q failed=%#v", startID, failed)
	}
	if strings.Contains(stderr, privateBody) {
		t.Errorf("remote response body leaked into diagnostics: %s", stderr)
	}
}

func TestRunMissingSessionUsesStableAuthenticationErrorCode(t *testing.T) {
	t.Setenv("RENFE_CONFIG_DIR", t.TempDir())
	t.Setenv("RENFE_LOG_FORMAT", "json")
	t.Setenv("RENFE_LOG_LEVEL", "info")
	var code int
	stderr := captureStderr(t, func() { code = run([]string{"trips"}) })
	if code != 1 {
		t.Fatalf("run returned %d, want authentication failure", code)
	}
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) == nil && event["event_name"] == "command_failed" {
			if event["error_code"] != "AUTH_REQUIRED" || event["operation"] != "trips" || event["correlation_id"] == "" {
				t.Fatalf("authentication failure lacks stable diagnostics: %#v", event)
			}
			return
		}
	}
	t.Fatalf("authentication failure event missing: %s", stderr)
}

func TestRunCorrelatesSafeStoredSessionWarning(t *testing.T) {
	t.Setenv("RENFE_CONFIG_DIR", t.TempDir())
	t.Setenv("RENFE_LOG_FORMAT", "json")
	t.Setenv("RENFE_LOG_LEVEL", "info")
	const secret = "private-stored-cookie"
	if err := os.WriteFile(store.Path(sessionFile), []byte(`{"cookie":"`+secret+`",broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	var code int
	stderr := captureStderr(t, func() { code = run([]string{"trips"}) })
	if code != 1 || strings.Contains(stderr, secret) {
		t.Fatalf("corrupt session result was unsafe or successful: code=%d stderr=%s", code, stderr)
	}
	var commandID string
	var warningID string
	var errorCode string
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		switch event["event_name"] {
		case "command_started":
			commandID, _ = event["correlation_id"].(string)
		case "stored_session_could_not_be_read":
			warningID, _ = event["correlation_id"].(string)
		case "command_failed":
			errorCode, _ = event["error_code"].(string)
		}
	}
	if commandID == "" || warningID != commandID || errorCode != "AUTH_REQUIRED" {
		t.Fatalf("stored session flow lacks correlated diagnostics: %s", stderr)
	}
}

func TestRunInvalidFlagEmitsCorrelatedFailure(t *testing.T) {
	t.Setenv("RENFE_LOG_FORMAT", "json")
	t.Setenv("RENFE_LOG_LEVEL", "info")
	var code int
	stderr := captureStderr(t, func() { code = run([]string{"search", "--unknown"}) })
	if code != 1 {
		t.Fatalf("run returned %d, want invalid command failure", code)
	}
	var startedID string
	var failed map[string]any
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		switch event["event_name"] {
		case "command_started":
			startedID, _ = event["correlation_id"].(string)
		case "command_failed":
			failed = event
		}
	}
	if startedID == "" || failed == nil || failed["correlation_id"] != startedID || failed["error_type"] == nil || failed["error_code"] != "INVALID_FLAGS" {
		t.Fatalf("invalid flag failure lacked correlated diagnostics: %s", stderr)
	}
	if !strings.Contains(stderr, "flag provided but not defined") {
		t.Fatalf("actionable usage error missing: %s", stderr)
	}
}

func TestCommandErrorCodesClassifyOperationalFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"invalid flags", errInvalidFlags, "INVALID_FLAGS"},
		{"authentication", errAuthenticationRequired, "AUTH_REQUIRED"},
		{"store write", errStoreWriteFailed, "STORE_WRITE_FAILED"},
		{"graceful cancellation", errGracefulCancellation, "CANCELLED"},
		{"session expiry", fmt.Errorf("wrapped: %w", client.ErrSessionExpired), "SESSION_EXPIRED"},
		{"timeout", context.DeadlineExceeded, "TIMEOUT"},
		{"cancellation", context.Canceled, "CANCELLED"},
		{"http status", &client.APIError{Status: 403}, "HTTP_403"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandErrorCode(tc.err); got != tc.want {
				t.Errorf("commandErrorCode(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestRunUnknownCommandIsRejectedWithWarning(t *testing.T) {
	t.Setenv("RENFE_LOG_FORMAT", "json")
	t.Setenv("RENFE_LOG_LEVEL", "info")
	var code int
	stderr := captureStderr(t, func() { code = run([]string{"not-a-command"}) })
	if code != 2 {
		t.Fatalf("run returned %d, want usage error code 2", code)
	}
	if !strings.Contains(stderr, `"event_name":"command_rejected"`) || !strings.Contains(stderr, `"error_code":"UNKNOWN_COMMAND"`) {
		t.Fatalf("unknown command lacked a structured warning: %s", stderr)
	}
}

func TestRunWithoutCommandIsRejectedWithWarning(t *testing.T) {
	t.Setenv("RENFE_LOG_FORMAT", "json")
	t.Setenv("RENFE_LOG_LEVEL", "info")
	var code int
	stderr := captureStderr(t, func() { code = run(nil) })
	if code != 2 {
		t.Fatalf("run returned %d, want usage error code 2", code)
	}
	if !strings.Contains(stderr, `"event_name":"command_rejected"`) || !strings.Contains(stderr, `"error_code":"MISSING_COMMAND"`) {
		t.Fatalf("missing command lacked a structured warning: %s", stderr)
	}
	if !strings.Contains(stderr, `"correlation_id":"`) {
		t.Fatalf("missing command warning lacked correlation: %s", stderr)
	}
}

func panicWithSensitivePayload() int { panic(errors.New("sensitive panic payload")) }

func TestUnhandledCommandPanicIsCriticalAndCorrelated(t *testing.T) {
	t.Setenv("RENFE_LOG_FORMAT", "json")
	t.Setenv("RENFE_LOG_LEVEL", "info")
	const correlationID = "0123456789abcdef0123456789abcdef"
	var code int
	stderr := captureStderr(t, func() {
		code = runWithPanicRecovery(correlationID, "test", time.Now(), func() int {
			return panicWithSensitivePayload()
		})
	})
	if code != 2 {
		t.Fatalf("panic exit code = %d, want 2", code)
	}
	for _, want := range []string{`"level":"CRITICAL"`, `"event_name":"command_panicked"`, `"error_type":"*errors.errorString"`, `"stack_trace":`, `"correlation_id":"` + correlationID + `"`} {
		if !strings.Contains(stderr, want) {
			t.Errorf("panic event missing %q: %s", want, stderr)
		}
	}
	if strings.Contains(stderr, "sensitive panic payload") {
		t.Fatalf("panic payload leaked into logs: %s", stderr)
	}
}

func TestRunCommandHelpCompletesSuccessfully(t *testing.T) {
	t.Setenv("RENFE_LOG_FORMAT", "json")
	t.Setenv("RENFE_LOG_LEVEL", "info")
	var code int
	stderr := captureStderr(t, func() { code = run([]string{"search", "--help"}) })
	if code != 0 || !strings.Contains(stderr, "USAGE") && !strings.Contains(stderr, "Usage of search") {
		t.Fatalf("command help was not successful or visible: code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, `"event_name":"command_completed"`) || strings.Contains(stderr, `"event_name":"command_failed"`) {
		t.Fatalf("help did not complete cleanly in logs: %s", stderr)
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
	t.Setenv("RENFE_LOG_FORMAT", "json")
	t.Setenv("RENFE_LOG_LEVEL", "debug")
	if err := store.Save(sessionFile, session{Cookie: "JSESSIONID=x; SSOInfo=y"}); err != nil {
		t.Fatal(err)
	}

	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = capture(t, func() error {
			return sessionKeep([]string{"--json", "--for", "60ms"})
		})
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
	if !strings.Contains(stderr, `"msg":"session keepalive verified"`) || strings.Contains(stderr, "MARC RIVERO") {
		t.Errorf("keepalive diagnostic missing or leaked identity: %s", stderr)
	}
}

func TestRunSessionKeepSignalCompletesAsCancelled(t *testing.T) {
	t.Setenv("RENFE_CONFIG_DIR", t.TempDir())
	t.Setenv("RENFE_LOG_FORMAT", "json")
	t.Setenv("RENFE_LOG_LEVEL", "info")
	dwrReady := make(chan struct{})
	dwrCount := 0
	who := dwrPayload(t, "MARC RIVERO")
	ms := dwrPayload(t, (25 * time.Minute).Milliseconds())
	srv := renfeServer(t, func(path string) []byte {
		dwrCount++
		if dwrCount == 2 {
			close(dwrReady)
		}
		if strings.Contains(path, "checkSession") {
			return ms
		}
		return who
	})
	t.Setenv("RENFE_BASE_URL", srv.URL)
	if err := store.Save(sessionFile, session{Cookie: "JSESSIONID=x; SSOInfo=y"}); err != nil {
		t.Fatal(err)
	}
	go func() {
		<-dwrReady
		time.Sleep(25 * time.Millisecond)
		process, _ := os.FindProcess(os.Getpid())
		_ = process.Signal(os.Interrupt)
	}()
	var code int
	stderr := captureStderr(t, func() { code = run([]string{"session", "keep", "--quiet", "--every", "1h"}) })
	if code != 0 {
		t.Fatalf("graceful signal returned code %d: %s", code, stderr)
	}
	var startedID string
	var cancelled bool
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid lifecycle event: %v\n%s", err, line)
		}
		if event["event_name"] == "command_started" {
			startedID, _ = event["correlation_id"].(string)
		}
		if event["event_name"] == "command_completed" {
			cancelled = event["outcome"] == "cancelled" && event["error_code"] == "CANCELLED" && event["correlation_id"] == startedID
		}
	}
	if startedID == "" || !cancelled {
		t.Fatalf("signal termination was not correlated as cancellation: %s", stderr)
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

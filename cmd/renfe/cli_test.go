package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/seifreed/renfecli/internal/store"
	"log/slog"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/seifreed/renfecli/internal/client"
)

func TestReorderArgs(t *testing.T) {
	newFS := func() *flag.FlagSet {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.String("date", "", "")
		fs.Int("limit", 0, "")
		fs.Bool("json", false, "")
		return fs
	}
	tests := []struct {
		name string
		args []string
		date string
		lim  int
		js   bool
		pos  []string
	}{
		{"flags first", []string{"--date", "tomorrow", "madrid", "sevilla"}, "tomorrow", 0, false, []string{"madrid", "sevilla"}},
		// The whole point: a flag after the positionals must still be seen.
		{"flags last", []string{"madrid", "sevilla", "--date", "tomorrow", "--json"}, "tomorrow", 0, true, []string{"madrid", "sevilla"}},
		{"flags interleaved", []string{"madrid", "--limit", "3", "sevilla", "--json"}, "", 3, true, []string{"madrid", "sevilla"}},
		{"equals form", []string{"madrid", "--date=2026-01-02"}, "2026-01-02", 0, false, []string{"madrid"}},
		{"double dash", []string{"--json", "--", "--weird-station"}, "", 0, true, []string{"--weird-station"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFS()
			parseFlags(fs, tc.args)
			if got := fs.Lookup("date").Value.String(); got != tc.date {
				t.Errorf("--date = %q, want %q", got, tc.date)
			}
			if got := fs.Lookup("limit").Value.String(); got != strconv.Itoa(tc.lim) {
				t.Errorf("--limit = %q, want %d", got, tc.lim)
			}
			if got := fs.Lookup("json").Value.String() == "true"; got != tc.js {
				t.Errorf("--json = %v, want %v", got, tc.js)
			}
			if !reflect.DeepEqual(fs.Args(), tc.pos) {
				t.Errorf("positionals = %q, want %q", fs.Args(), tc.pos)
			}
		})
	}
}

func TestFilterJourneys(t *testing.T) {
	js := []client.Journey{
		{Departure: "07:00", Price: 80, Available: true},
		{Departure: "08:00", Price: 0, SoldOut: true},
		{Departure: "09:00", Price: 30, Available: true},
		{Departure: "10:00", Price: 50, Available: true},
	}
	t.Run("available only", func(t *testing.T) {
		got := filterJourneys(append([]client.Journey(nil), js...), true, false, 0, timeWindow{})
		if len(got) != 3 {
			t.Fatalf("got %d journeys, want 3", len(got))
		}
	})
	t.Run("by price keeps sold-out last", func(t *testing.T) {
		got := filterJourneys(append([]client.Journey(nil), js...), false, true, 0, timeWindow{})
		want := []string{"09:00", "10:00", "07:00", "08:00"}
		for i, w := range want {
			if got[i].Departure != w {
				t.Fatalf("order = %v, want %v", departures(got), want)
			}
		}
	})
	t.Run("limit counts what is shown", func(t *testing.T) {
		got := filterJourneys(append([]client.Journey(nil), js...), true, true, 2, timeWindow{})
		if len(got) != 2 || got[0].Departure != "09:00" || got[1].Departure != "10:00" {
			t.Errorf("got %v, want the two cheapest available", departures(got))
		}
	})
	t.Run("does not alias the caller's slice", func(t *testing.T) {
		src := append([]client.Journey(nil), js...)
		_ = filterJourneys(src, true, false, 0, timeWindow{})
		if src[1].Departure != "08:00" {
			t.Error("filtering overwrote the input slice")
		}
	})
}

func TestTimeWindow(t *testing.T) {
	js := []client.Journey{
		{Departure: "07:00", Available: true},
		{Departure: "13:30", Available: true},
		{Departure: "19:45", Available: true},
	}
	cases := []struct {
		after, before string
		want          []string
	}{
		{"12:00", "", []string{"13:30", "19:45"}},
		{"", "14:00", []string{"07:00", "13:30"}},
		{"12:00", "14:00", []string{"13:30"}},
		// Bounds are inclusive: a train leaving exactly at the limit counts.
		{"13:30", "13:30", []string{"13:30"}},
		{"", "", []string{"07:00", "13:30", "19:45"}},
	}
	for _, c := range cases {
		w, err := parseTimeWindow(c.after, c.before)
		if err != nil {
			t.Fatalf("parseTimeWindow(%q,%q): %v", c.after, c.before, err)
		}
		got := departures(filterJourneys(append([]client.Journey(nil), js...), false, false, 0, w))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("after=%q before=%q -> %v, want %v", c.after, c.before, got, c.want)
		}
	}
}

func TestParseTimeWindowRejectsNonsense(t *testing.T) {
	for _, c := range [][2]string{{"7pm", ""}, {"", "25:00"}, {"nope", ""}, {"18:00", "09:00"}} {
		if _, err := parseTimeWindow(c[0], c[1]); err == nil {
			t.Errorf("parseTimeWindow(%q,%q) = nil error, want failure", c[0], c[1])
		}
	}
	// A single-digit hour is a normal thing to type and must be accepted.
	if w, err := parseTimeWindow("9:05", ""); err != nil || w.after != "09:05" {
		t.Errorf("parseTimeWindow(\"9:05\") = %+v, %v", w, err)
	}
}

func departures(js []client.Journey) []string {
	out := make([]string, len(js))
	for i, j := range js {
		out[i] = j.Departure
	}
	return out
}

func TestDurationFormat(t *testing.T) {
	for in, want := range map[int]string{0: "", 45: "45m", 60: "1h 00m", 203: "3h 23m", 125: "2h 05m"} {
		if got := duration(in); got != want {
			t.Errorf("duration(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestCheapestDaySkipsUnavailable(t *testing.T) {
	days := []client.PriceDay{
		{Date: "2026-09-20", MinPrice: 76, Available: true},
		{Date: "2026-09-21", MinPrice: 9, Available: false},
		{Date: "2026-09-22", MinPrice: 15, Available: true},
	}
	if got := cheapestDay(days); got.Date != "2026-09-22" {
		t.Errorf("cheapestDay = %+v, want the 22nd (the 21st is not on sale)", got)
	}
	if got := cheapestDay(days[1:2]); got.Date != "" {
		t.Errorf("an all-unavailable strip should yield no day, got %+v", got)
	}
}

func TestUnknownCommandExitsTwo(t *testing.T) {
	if code := run([]string{"nope"}); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if code := run(nil); code != 2 {
		t.Errorf("no args exit code = %d, want 2", code)
	}
	if code := run([]string{"version"}); code != 0 {
		t.Errorf("version exit code = %d, want 0", code)
	}
}

func TestToonEncodeMatchesJSONShape(t *testing.T) {
	s, err := toonEncode(client.PriceDay{Date: "2026-09-22", MinPrice: 15, Available: true})
	if err != nil {
		t.Fatal(err)
	}
	// TOON must carry the json tag names, not the Go field names.
	for _, want := range []string{"date", "min_price", "available"} {
		if !strings.Contains(s, want) {
			t.Errorf("TOON output missing %q:\n%s", want, s)
		}
	}
}

// Renfe quotes per passenger, so a party total is the fare times this. Getting
// it wrong understates a family's trip by a factor of however many they are.
// The per-person price multiplies by the travellers who occupy a seat. An
// infant under 4 rides on a lap — Renfe carries them in their own form field
// for that reason — so counting one as a fare turned "multiply by 3" into
// "multiply by 4" and overstated the trip by a whole passenger.
func TestSeatedExcludesLapInfants(t *testing.T) {
	for _, tc := range []struct {
		q    client.Query
		want int
	}{
		{client.Query{Adults: 1}, 1},
		{client.Query{Adults: 2}, 2},
		{client.Query{Adults: 2, Children: 1}, 3},
		{client.Query{Adults: 2, Children: 1, Infants: 1}, 3}, // the infant has no seat
		{client.Query{Adults: 1, Infants: 2}, 1},
		{client.Query{}, 1}, // an unset query still prices for somebody
	} {
		if got := tc.q.Seated(); got != tc.want {
			t.Errorf("Seated(%+v) = %d, want %d", tc.q, got, tc.want)
		}
	}
}

// …and the notice has to say so, or a reader wonders why the party of four
// multiplies by three.
func TestPriceNoticeNamesTheLapInfants(t *testing.T) {
	res := &client.Results{From: "A", To: "B", Date: "2026-10-04",
		Journeys: []client.Journey{{Departure: "06:00", Arrival: "09:00", Price: 50}}}

	solo := render(t, res, client.Query{Adults: 1})
	if strings.Contains(solo, "PER PERSON") {
		t.Errorf("one traveller needs no multiplication:\n%s", solo)
	}
	party := render(t, res, client.Query{Adults: 2, Children: 1})
	if !strings.Contains(party, "multiply by 3 for the party") {
		t.Errorf("a party of three seats:\n%s", party)
	}
	withInfant := render(t, res, client.Query{Adults: 2, Children: 1, Infants: 1})
	if !strings.Contains(withInfant, "multiply by 3") || !strings.Contains(withInfant, "lap") {
		t.Errorf("the infant must be excluded and explained:\n%s", withInfant)
	}
	if strings.Contains(withInfant, "multiply by 4") {
		t.Errorf("the lap infant was priced as a passenger:\n%s", withInfant)
	}
	// A lone adult with a baby still needs the sentence, or the infant looks
	// priced — and it has to read correctly for one as well as for several.
	loneParent := render(t, res, client.Query{Adults: 1, Infants: 1})
	if !strings.Contains(loneParent, "lap") {
		t.Errorf("the infant went unmentioned:\n%s", loneParent)
	}
	if strings.Contains(loneParent, "multiply by 1") {
		t.Errorf("multiplying by one helps nobody:\n%s", loneParent)
	}
}

// A round trip takes two windows: --after/--before bound the outbound, and the
// return gets its own only when asked. Without per-leg flags a user narrowing
// "afternoon" threw away every morning outbound as well.
func TestRoundTripTimeWindowsAreIndependent(t *testing.T) {
	out := []client.Journey{{Departure: "07:00"}, {Departure: "15:00"}}
	ret := []client.Journey{{Departure: "09:00"}, {Departure: "19:00"}}

	outWin, err := parseTimeWindow("", "10:00")
	if err != nil {
		t.Fatal(err)
	}
	retWin, err := parseTimeWindow("18:00", "")
	if err != nil {
		t.Fatal(err)
	}

	gotOut := departures(filterJourneys(append([]client.Journey(nil), out...), false, false, 0, outWin))
	gotRet := departures(filterJourneys(append([]client.Journey(nil), ret...), false, false, 0, retWin))
	if len(gotOut) != 1 || gotOut[0] != "07:00" {
		t.Errorf("outbound = %v, want the morning train", gotOut)
	}
	if len(gotRet) != 1 || gotRet[0] != "19:00" {
		t.Errorf("return = %v, want the evening train", gotRet)
	}

	// The outbound window must still carry over when no return window is given,
	// so the one-way behaviour is unchanged.
	carried := departures(filterJourneys(append([]client.Journey(nil), ret...), false, false, 0, outWin))
	if len(carried) != 1 || carried[0] != "09:00" {
		t.Errorf("carried-over window = %v, want the 09:00", carried)
	}
}

// A same-day round trip lists both legs independently, so returns that leave
// before the outbound can arrive look like ordinary options. On a long route
// that is most of them.
func TestSameDayNote(t *testing.T) {
	day := func(out, ret []client.Journey, retDate string) *client.Results {
		return &client.Results{
			Date: "23/09/2026", Journeys: out,
			Return: &client.Results{Date: retDate, Journeys: ret},
		}
	}
	outbound := []client.Journey{{Arrival: "12:35"}, {Arrival: "13:44"}}

	t.Run("some catchable", func(t *testing.T) {
		got := sameDayNote(day(outbound,
			[]client.Journey{{Departure: "07:00"}, {Departure: "14:59"}, {Departure: "17:41"}}, "23/09/2026"))
		for _, want := range []string{"12:35", "only 2 of the 3"} {
			if !strings.Contains(got, want) {
				t.Errorf("note %q missing %q", got, want)
			}
		}
	})
	t.Run("none catchable", func(t *testing.T) {
		got := sameDayNote(day(outbound, []client.Journey{{Departure: "07:00"}, {Departure: "09:30"}}, "23/09/2026"))
		if !strings.Contains(got, "NOT possible") {
			t.Errorf("note = %q, want it to say the trip cannot be made", got)
		}
	})
	t.Run("all catchable stays quiet", func(t *testing.T) {
		if got := sameDayNote(day(outbound, []client.Journey{{Departure: "18:00"}}, "23/09/2026")); got != "" {
			t.Errorf("nothing to warn about, got %q", got)
		}
	})
	t.Run("overnight trip is not a same-day question", func(t *testing.T) {
		if got := sameDayNote(day(outbound, []client.Journey{{Departure: "07:00"}}, "25/09/2026")); got != "" {
			t.Errorf("a different return date should not warn, got %q", got)
		}
	})
	// A journey that lands after midnight reports an arrival lexically smaller
	// than every return's departure, so comparing the strings makes the latest
	// arrival look like the earliest and the warning vanishes — in the one case
	// where a same-day return is flatly impossible.
	t.Run("arrival after midnight", func(t *testing.T) {
		overnight := []client.Journey{{Departure: "22:05", Arrival: "01:30"}}
		got := sameDayNote(day(overnight,
			[]client.Journey{{Departure: "07:00"}, {Departure: "19:30"}}, "23/09/2026"))
		if !strings.Contains(got, "NOT possible") {
			t.Errorf("an outbound landing at 01:30 the next day makes every same-day "+
				"return uncatchable; note = %q", got)
		}
	})
	t.Run("one-way", func(t *testing.T) {
		if got := sameDayNote(&client.Results{Journeys: outbound}); got != "" {
			t.Errorf("no return leg, got %q", got)
		}
	})
}

// usage() is hand-written prose, so nothing but a test stops it drifting from
// the commands run() actually dispatches. A command that exists but is not
// documented is invisible; one that is documented but does not exist is a lie.
func TestEveryCommandIsDocumented(t *testing.T) {
	help := captureStderr(t, usage)
	for _, cmd := range dispatchedCommands(t) {
		if !strings.Contains(help, "  "+cmd) {
			t.Errorf("command %q is dispatched but not listed in usage()", cmd)
		}
	}
}

// dispatchedCommands reads the case labels of run()'s switch out of the source,
// so adding a command to it is what makes the test cover the command.
func dispatchedCommands(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	inSwitch := false
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "switch args[0]"):
			inSwitch = true
		case inSwitch && strings.HasPrefix(line, "default:"):
			inSwitch = false
		case inSwitch && strings.HasPrefix(line, "case "):
			for _, lit := range strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "case "), ":"), ",") {
				name := strings.Trim(strings.TrimSpace(lit), `"`)
				if name != "" && !strings.HasPrefix(name, "-") {
					out = append(out, name)
				}
			}
		}
	}
	if len(out) < 5 {
		t.Fatalf("could not read run()'s dispatch table, found %v", out)
	}
	return out
}

// The two failures that actually happen — bot detection refusing the client,
// and the site falling over — must not reach the user as a status code and a
// slab of HTML.
func TestExplainTurnsAStatusIntoAdvice(t *testing.T) {
	blob := "<html><head><title>Access Denied</title></head><body>…</body></html>"
	forbidden := fmt.Errorf("search: %w", &client.APIError{Status: 403, Body: blob})
	got := explain(forbidden)
	if !strings.Contains(got, "bot detection") || strings.Contains(got, "<html>") {
		t.Errorf("a 403 should explain itself, got: %s", got)
	}
	if got := explain(&client.APIError{Status: 502}); !strings.Contains(got, "try again later") {
		t.Errorf("a 5xx should say what to do, got: %s", got)
	}
	// Everything else is already a written sentence; explain must not reword it.
	plain := errors.New("no trains for MADRID → BARCELONA on 2026-10-04")
	if got := explain(plain); got != plain.Error() {
		t.Errorf("explain rewrote an ordinary error: %s", got)
	}
	if got := explain(&client.APIError{Status: 404, Body: "nope"}); !strings.Contains(got, "404") {
		t.Errorf("an unmapped status should still surface, got: %s", got)
	}
}

// Renfe answers a date that has gone by with the same empty list it uses for
// one not yet on sale, and the hint for that tells the user to wait — so a
// typo in the year sent them off to wait for a train that left months ago.
func TestSearchRejectsADateThatHasPassed(t *testing.T) {
	fixClock(t) // 2026-09-22
	withRenfe(t, replaying(t, "trainslist.dwr"))

	_, err := capture(t, func() error {
		return cmdSearch([]string{"madrid", "barcelona", "--date", "2026-01-05"})
	})
	if err == nil {
		t.Fatal("a date in the past must be refused, not searched")
	}
	if !strings.Contains(err.Error(), "already gone by") {
		t.Errorf("unhelpful message: %v", err)
	}
	// Today is still a valid day to travel on.
	if _, err := capture(t, func() error {
		return cmdSearch([]string{"madrid", "barcelona", "--date", "today", "--limit", "1"})
	}); err != nil {
		t.Errorf("today must still be searchable: %v", err)
	}
}

// Renfe accepts a return date before the outbound and answers with two
// unrelated lists; the CLI then prints them as one trip nobody can make.
func TestSearchRejectsAReturnBeforeTheOutbound(t *testing.T) {
	fixClock(t)
	withRenfe(t, replaying(t, "roundtrip.dwr"))

	_, err := capture(t, func() error {
		return cmdSearch([]string{"madrid", "barcelona", "--date", "+14", "--return", "+7"})
	})
	if err == nil || !strings.Contains(err.Error(), "before the outbound") {
		t.Fatalf("an inverted round trip must be refused, got %v", err)
	}
	// Out and back on the same day is a normal booking, not an inversion.
	if _, err := capture(t, func() error {
		return cmdSearch([]string{"madrid", "barcelona", "--date", "+14", "--return", "+14", "--limit", "1"})
	}); err != nil {
		t.Errorf("a same-day return must still be allowed: %v", err)
	}
}

// Renfe prints departures zero-padded, so a bare string compare meant --at 8:27
// could never match the 08:27 train the user was reading off the search.
func TestPickJourneyNormalisesTheClock(t *testing.T) {
	js := []client.Journey{{Departure: "06:27"}, {Departure: "08:27"}}
	for _, at := range []string{"8:27", "08:27", " 8:27 "} {
		got, err := pickJourney(js, "", at)
		if err != nil {
			t.Errorf("--at %q: %v", at, err)
			continue
		}
		if got.Departure != "08:27" {
			t.Errorf("--at %q picked %s", at, got.Departure)
		}
	}
	if _, err := pickJourney(js, "", "half past eight"); err == nil {
		t.Error("a value that is not a time must be reported, not silently missed")
	}
}

// Hoisting the flags ahead of a "--" terminator means a flag left without a
// value gets the terminator as its value, so `renfe search madrid --date` used
// to fail with `bad date "--"` — about a "--" the user never typed.
func TestParseFlagsReportsAFlagLeftWithoutAValue(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("date", "", "")
	fs.Bool("json", false, "")
	var errOut strings.Builder
	fs.SetOutput(&errOut)

	parseFlags(fs, []string{"madrid", "barcelona", "--date"})

	if got := fs.Lookup("date").Value.String(); got != "" {
		t.Errorf("--date swallowed %q as its value", got)
	}
	if !strings.Contains(errOut.String(), "needs an argument") {
		t.Errorf("the missing value was not reported: %q", errOut.String())
	}
	// A bool flag at the end is complete on its own and must still parse.
	fs2 := flag.NewFlagSet("t", flag.ContinueOnError)
	fs2.Bool("json", false, "")
	parseFlags(fs2, []string{"madrid", "--json"})
	if fs2.Lookup("json").Value.String() != "true" {
		t.Error("a trailing bool flag was dropped")
	}
}

// A negative count was searched as one passenger and printed as itself, so the
// party shown contradicted the party priced.
func TestSearchRejectsNegativePassengerCounts(t *testing.T) {
	fixClock(t)
	for _, flagName := range []string{"--adults", "--children", "--infants"} {
		withRenfe(t, replaying(t, "trainslist.dwr"))
		_, err := capture(t, func() error {
			return cmdSearch([]string{"madrid", "barcelona", flagName, "-2"})
		})
		if err == nil || !strings.Contains(err.Error(), "cannot be negative") {
			t.Errorf("%s -2 was accepted, got %v", flagName, err)
		}
	}
}

// A negative --limit fell through the "limit > 0" guard and silently meant
// "no cap", so a typo printed everything instead of being reported.
func TestNegativeLimitIsRejected(t *testing.T) {
	fixClock(t)
	withRenfe(t, replaying(t, "trainslist.dwr"))
	for _, run := range []func() error{
		func() error { return cmdSearch([]string{"madrid", "barcelona", "--limit", "-1"}) },
		func() error { return cmdStations([]string{"madrid", "--limit", "-5"}) },
	} {
		if _, err := capture(t, run); err == nil || !strings.Contains(err.Error(), "cannot be negative") {
			t.Errorf("a negative --limit was accepted, got %v", err)
		}
	}
}

// RENFE_BASE_URL is a debugging knob, and it redirects the credential along
// with the requests: any https host it names was handed the user's Renfe
// session by a command as ordinary as `renfe trips`. Cookies are stored Secure
// so a plain-http host never received them (the jar excepts loopback, which is
// this machine), but an https one did.
func TestStoredSessionOnlyGoesToRenfeOrThisMachine(t *testing.T) {
	allowed := []string{
		"https://venta.renfe.com",
		"https://venta.renfe.com/",
		"https://sub.venta.renfe.com",
		"http://127.0.0.1:8080", // a local proxy or mock is the user's own process
		"http://[::1]:8080",
		"http://localhost:1234",
	}
	for _, base := range allowed {
		if err := sessionAllowedOn(base); err != nil {
			t.Errorf("sessionAllowedOn(%q) = %v, want it allowed", base, err)
		}
	}
	blocked := []string{
		"https://evil.example",
		"https://venta.renfe.com.evil.example", // the substring trap
		"https://renfe.com.attacker.test",
		"https://www.renfe.com", // the catalogue host, not the session's
		"http://evil.example",   // Secure already stops it; say so anyway
		"nonsense",
	}
	for _, base := range blocked {
		err := sessionAllowedOn(base)
		if err == nil {
			t.Errorf("sessionAllowedOn(%q) allowed a host that is not Renfe", base)
			continue
		}
		var fh foreignHost
		if base != "nonsense" && !errors.As(err, &fh) {
			t.Errorf("sessionAllowedOn(%q) = %v, want it marked as a foreign host", base, err)
		}
	}

	// The escape hatch stays, because a debugging proxy on another machine is a
	// real use — it just has to be asked for.
	t.Setenv(allowSessionEnv, "1")
	if err := sessionAllowedOn("https://proxy.internal"); err != nil {
		t.Errorf("the opt-in did not work: %v", err)
	}
}

// The account commands must fail closed: refusing to send the credential is
// not a reason to carry on anonymously and report an empty account.
func TestAccountCommandsFailClosedOnAForeignHost(t *testing.T) {
	t.Setenv("RENFE_CONFIG_DIR", t.TempDir())
	t.Setenv("RENFE_BASE_URL", "https://evil.example")
	if err := store.Save(sessionFile, session{Cookie: "JSESSIONID=x; SSOInfo=y"}); err != nil {
		t.Fatal(err)
	}
	if _, err := authClient(); err == nil {
		t.Fatal("authClient handed back a client for a host that must not have the session")
	} else if !strings.Contains(err.Error(), "refusing to send") {
		t.Errorf("unclear message: %v", err)
	}
}

func TestRunEmitsStructuredLifecycleWithoutArguments(t *testing.T) {
	t.Setenv("RENFE_LOG_FORMAT", "json")
	stderr := captureStderr(t, func() {
		if code := run([]string{"version", "sensitive-argument"}); code != 0 {
			t.Fatalf("run returned %d", code)
		}
	})
	if !strings.Contains(stderr, `"operation":"version"`) || !strings.Contains(stderr, `"event_name":"command_started"`) || !strings.Contains(stderr, `"component":"cli"`) || !strings.Contains(stderr, `"duration_ms":`) || strings.Contains(stderr, "sensitive-argument") {
		t.Fatalf("unexpected lifecycle logs: %s", stderr)
	}
}

func TestLogLevelFiltersEvents(t *testing.T) {
	t.Setenv("RENFE_LOG_LEVEL", "warn")
	stderr := captureStderr(t, func() {
		stderrEvent(slog.LevelInfo, "routine event")
		stderrEvent(slog.LevelWarn, "recoverable issue", "detail", "retrying")
	})
	if strings.Contains(stderr, "routine event") || !strings.Contains(stderr, "recoverable issue") {
		t.Fatalf("WARN threshold did not filter events: %s", stderr)
	}
}

func TestJSONLogEscapesUntrustedTextAsOneRecord(t *testing.T) {
	t.Setenv("RENFE_LOG_FORMAT", "json")
	t.Setenv("RENFE_LOG_LEVEL", "debug")
	input := "remote\nforged event\x1b"
	stderr := captureStderr(t, func() { stderrEvent(slog.LevelWarn, "remote warning", "detail", input) })
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if len(lines) != 1 {
		t.Fatalf("untrusted text injected additional log records: %q", stderr)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("invalid JSON log record: %v", err)
	}
	if record["detail"] != "remote\nforged event" {
		t.Fatalf("control characters were not stripped from attribute: %#v", record["detail"])
	}
}

func TestConcurrentStructuredLogsRemainValidRecords(t *testing.T) {
	t.Setenv("RENFE_LOG_FORMAT", "json")
	t.Setenv("RENFE_LOG_LEVEL", "debug")
	const records = 48
	stderr := captureStderr(t, func() {
		var wg sync.WaitGroup
		wg.Add(records)
		for i := 0; i < records; i++ {
			go func(id int) {
				defer wg.Done()
				stderrEvent(slog.LevelDebug, "concurrent diagnostic", "worker", id)
			}(i)
		}
		wg.Wait()
	})
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if len(lines) != records {
		t.Fatalf("got %d records, want %d", len(lines), records)
	}
	seen := map[float64]bool{}
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("concurrent log line is invalid JSON: %v", err)
		}
		id, ok := record["worker"].(float64)
		if !ok || seen[id] {
			t.Fatalf("missing or duplicated worker field: %#v", record)
		}
		seen[id] = true
	}
}

func TestLogDestinationFailureDoesNotPanic(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	_ = writer.Close()
	original := os.Stderr
	os.Stderr = writer
	t.Cleanup(func() { os.Stderr = original })
	stderrEvent(slog.LevelError, "diagnostic destination unavailable", "component", "test")
}

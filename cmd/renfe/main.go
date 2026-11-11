// Command renfe is an unofficial, agent-friendly CLI for venta.renfe.com.
//
// It covers the anonymous read core — journey search with live fares, the price
// calendar behind Renfe's "cheapest day" strip, and the station catalogue — over
// the same endpoints the booking site uses (a session bootstrap, the search form
// POST, then DWR calls), presenting Chrome's TLS fingerprint (uTLS) so the
// bot-detection layer keeps it on the no-challenge path. Every command supports
// --json (data to stdout, logs to stderr) for programmatic use by agents.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/seifreed/renfecli/internal/client"
	"github.com/seifreed/renfecli/internal/observability"
)

// Build metadata, injected at release time via -ldflags.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func main() { os.Exit(run(os.Args[1:])) }

// run dispatches a command and returns the process exit code. Split from main
// so it is testable without os.Exit.
func run(args []string) int {
	if len(args) < 1 {
		correlationID := observability.NewCorrelationID()
		logger().Warn("command missing", append([]any{"event_name", "command_rejected", "component", "cli", "operation", "dispatch", "error_code", "MISSING_COMMAND"}, correlationAttrs(correlationID)...)...)
		usage()
		return 2
	}
	started := time.Now()
	operation := safeField(args[0])
	correlationID := observability.NewCorrelationID()
	return runWithPanicRecovery(correlationID, operation, started, func() int {
		return runCommand(args, correlationID, operation, started)
	})
}

func runWithPanicRecovery(correlationID, operation string, started time.Time, runCommand func() int) (exitCode int) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logger().Log(context.Background(), criticalLevel, "unhandled command panic",
				"event_name", "command_panicked", "component", "cli", "operation", operation,
				"error_type", fmt.Sprintf("%T", recovered), "stack_trace", safeText(string(debug.Stack())),
				"duration_ms", time.Since(started).Milliseconds(), "correlation_id", correlationID)
			exitCode = 2
		}
	}()
	return runCommand()
}

func runCommand(args []string, correlationID, operation string, started time.Time) int {
	logger().Info("command started", append([]any{"event_name", "command_started", "component", "cli", "operation", operation}, correlationAttrs(correlationID)...)...)
	var err error
	switch args[0] {
	case "search":
		err = cmdSearch(args[1:], correlationID)
	case "calendar":
		err = cmdCalendar(args[1:], correlationID)
	case "stops":
		err = cmdStops(args[1:], correlationID)
	case "stations":
		err = cmdStations(args[1:], correlationID)
	case "login":
		err = cmdLogin(args[1:], correlationID)
	case "whoami":
		err = cmdWhoami(args[1:], correlationID)
	case "trips":
		err = cmdTrips(args[1:], correlationID)
	case "session":
		err = cmdSession(args[1:], correlationID)
	case "version", "--version", "-v":
		fmt.Println(versionString())
	case "help", "-h", "--help":
		usage()
	default:
		logger().Warn("unknown command", append([]any{"event_name", "command_rejected", "component", "cli", "operation", operation, "error_code", "UNKNOWN_COMMAND"}, correlationAttrs(correlationID)...)...)
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", safeField(args[0]))
		usage()
		return 2
	}
	if errors.Is(err, flag.ErrHelp) {
		logger().Info("command completed", append([]any{"event_name", "command_completed", "component", "cli", "operation", operation, "outcome", "success", "duration_ms", time.Since(started).Milliseconds()}, correlationAttrs(correlationID)...)...)
		return 0
	}
	if errors.Is(err, errGracefulCancellation) {
		logger().Info("command completed", append([]any{"event_name", "command_completed", "component", "cli", "operation", operation, "outcome", "cancelled", "error_code", "CANCELLED", "duration_ms", time.Since(started).Milliseconds()}, correlationAttrs(correlationID)...)...)
		return 0
	}
	if err != nil {
		attrs := []any{"event_name", "command_failed", "component", "cli", "operation", operation, "error_type", fmt.Sprintf("%T", err), "duration_ms", time.Since(started).Milliseconds()}
		if code := commandErrorCode(err); code != "" {
			attrs = append(attrs, "error_code", code)
		}
		if status, ok := client.HTTPStatus(err); ok {
			attrs = append(attrs, "http_status", status)
		}
		logger().Error("command failed", append(attrs, correlationAttrs(correlationID)...)...)
		fmt.Fprintln(os.Stderr, "error:", safeText(explain(err)))
		return 1
	}
	logger().Info("command completed", append([]any{"event_name", "command_completed", "component", "cli", "operation", operation, "outcome", "success", "duration_ms", time.Since(started).Milliseconds()}, correlationAttrs(correlationID)...)...)
	return 0
}

func commandErrorCode(err error) string {
	switch {
	case errors.Is(err, errInvalidFlags):
		return "INVALID_FLAGS"
	case errors.Is(err, errAuthenticationRequired):
		return "AUTH_REQUIRED"
	case errors.Is(err, errStoreWriteFailed):
		return "STORE_WRITE_FAILED"
	case errors.Is(err, errGracefulCancellation):
		return "CANCELLED"
	case errors.Is(err, client.ErrSessionExpired):
		return "SESSION_EXPIRED"
	case errors.Is(err, context.DeadlineExceeded):
		return "TIMEOUT"
	case errors.Is(err, context.Canceled):
		return "CANCELLED"
	}
	if status, ok := client.HTTPStatus(err); ok {
		return fmt.Sprintf("HTTP_%d", status)
	}
	return ""
}

// explain turns the statuses Renfe answers with into something a person can act
// on. A bare *APIError prints the status and 300 characters of whatever HTML
// came back, which for the two failures that actually happen — the bot-detection
// layer refusing the client, and the site falling over — says nothing about what
// to do next. This is what client.HTTPStatus is for.
func explain(err error) string {
	status, ok := client.HTTPStatus(err)
	if !ok {
		return err.Error()
	}
	switch {
	case status == http.StatusForbidden:
		return "renfe refused the request (HTTP 403) — its bot detection did not accept this client. " +
			"Wait a few minutes and try again; if it persists, look for the uTLS warning on stderr, " +
			"since falling back to Go's own TLS fingerprint is an easy tell."
	case status >= 500:
		return fmt.Sprintf("renfe's site is failing (HTTP %d) — a throttle is already retried "+
			"several times before you see this, so try again later.", status)
	}
	return err.Error()
}

func versionString() string {
	// An untagged build describes itself from git, so its version already IS
	// the commit ("1ebcd8f", "1ebcd8f-dirty") and printing both says it twice.
	parts := []string{}
	if commit != "" && !strings.HasPrefix(version, commit) {
		parts = append(parts, commit)
	}
	if date != "" {
		parts = append(parts, date)
	}
	if len(parts) == 0 {
		return version
	}
	return fmt.Sprintf("%s (%s)", version, strings.Join(parts, ", "))
}

func usage() {
	fmt.Fprint(os.Stderr, `renfe — unofficial CLI for venta.renfe.com

USAGE:
  renfe <command> [flags]

COMMANDS:
  search <origin> <destination>   journeys with live fares for a date
  calendar <origin> <destination> cheapest price per day around a date
  stops <origin> <destination>    a train's intermediate calls and onboard services
                                  --train N   which train (default: the first)
                                  --at HH:MM  or pick it by departure time
  stations [query]                look up station names and codes

ACCOUNT COMMANDS (bring your own browser session):
  login                           lift the session from any installed browser's
                                  cookie store (this is the usual form)
  login --from-browser chrome     …or name one: chrome|chromium|firefox|safari|
                                  edge|brave. The flag needs a value; omit it
                                  entirely to search every browser.
  login --stdin                   read a raw Cookie header from stdin instead
  whoami                          check the session and show the +Renfe card
  trips                           upcoming journeys on the account
  session [status]                who is signed in, and how long the session lasts
  session keep                    hold the session open (Renfe drops an idle one
                                  after ~30 min); --every D, --for D, --quiet

  Origin and destination are matched against the station catalogue, so
  "madrid", "Barcelona-Sants" and the raw code "71801" all work. Run
  'renfe stations <name>' when a name is ambiguous.

SEARCH FLAGS:
  --date <d>        YYYY-MM-DD, DD/MM/YYYY, today, tomorrow, a weekday
                    name, or +N days (default today)
  --return <d>      round trip: prices the outbound under round-trip
                    rules and prints the return journeys too
  --adults N        adult passengers (default 1). NOTE: all prices are quoted
                    PER PASSENGER, not per booking
  --children N      children aged 4-13        --infants N   under 4, no seat
  --after HH:MM     only outbound trains departing at or after this time
  --before HH:MM    only outbound trains departing at or before this time
  --return-after HH:MM    the same, for the return leg of a round trip
  --return-before HH:MM   (omit both and the outbound window carries over)
  --direct          exclude journeys with a connection
  --pet             travelling with a pet     --bike        with a bicycle
  --wheelchair      request a wheelchair (H) space
  --cheapest        sort by price instead of departure time
  --available       hide sold-out and unpriced journeys
  --fares           list every fare bucket, not just the cheapest
  --limit N         cap the number of journeys shown

CALENDAR FLAGS:
  --date <d>        the date to centre the strip on (see above)
  --cheapest        show only the cheapest day

STATIONS FLAGS:
  --update          re-download the catalogue, ignoring the 7-day cache
  --limit N         cap the number of matches (default 20; 0 = no cap)

COMMON FLAGS (may go anywhere after the command):
  --json            emit JSON (data→stdout, logs→stderr)
  --toon            emit TOON instead of JSON (fewer tokens; for LLM/agents)

CONFIG (~/.renfe/config.toml):
  [defaults]
  origin = "Madrid"        # used when <origin> is omitted
  destination = "Sevilla"  # used when <destination> is omitted
  adults = 2

ENV:
  RENFE_CONFIG_DIR  override ~/.renfe
  RENFE_BASE_URL    override the API host (debugging proxy, mock, replay).
                    The stored session is NOT sent to a host that is neither
                    venta.renfe.com nor this machine; the anonymous commands
                    still work there.
  RENFE_ALLOW_SESSION_ON_CUSTOM_HOST=1
                    opt that host back in — it hands your Renfe session to
                    whatever RENFE_BASE_URL names

OTHER COMMANDS:
  version                         print the build version
  help                            print this text
`)
}

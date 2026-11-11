package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/seifreed/renfecli/internal/client"
)

// keepAliveMargin is how early to refresh before the session would lapse. Renfe
// warns its own web UI 60s ahead; two minutes leaves room for a slow request
// without waking up more often than necessary.
const keepAliveMargin = 2 * time.Minute

func cmdSession(args []string, correlationIDs ...string) error {
	sub := ""
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "", "status":
		return sessionStatus(args, correlationIDs...)
	case "keep":
		return sessionKeep(args, correlationIDs...)
	default:
		return fmt.Errorf("unknown session command %q — use: renfe session [status|keep]", sub)
	}
}

// sessionState is what `session status` reports and what each refresh of
// `session keep` emits, so a caller parses the two the same way.
type sessionState struct {
	Name      string `json:"name,omitempty"`
	SignedIn  bool   `json:"signed_in"`
	Seconds   int    `json:"seconds_remaining"`
	Remaining string `json:"remaining"`
}

func newSessionState(name string, left time.Duration) sessionState {
	return sessionState{
		Name: name, SignedIn: name != "",
		Seconds: int(left.Seconds()), Remaining: client.FormatRemaining(left),
	}
}

func sessionStatus(args []string, correlationIDs ...string) error {
	fs, cf := newCommonFlags("session status")
	parseFlags(fs, args)

	cl, err := authClient(correlationIDs...)
	if err != nil {
		return err
	}
	left, err := cl.SessionRemaining()
	if err != nil {
		return err
	}
	// A failed lookup is not the same answer as "nobody is signed in": reporting
	// the first as the second sends the user off to log in again over a network
	// blip. The remaining time is still worth printing, so say which half failed.
	name, err := cl.Whoami()
	if err != nil {
		return fmt.Errorf("the HTTP session is alive (%s left) but the login could not be checked: %w",
			client.FormatRemaining(left), err)
	}
	if emitted, err := emitStructured(cf, newSessionState(name, left)); emitted {
		return err
	}
	if name == "" {
		// Renfe keeps two clocks: the servlet session, and the login on top of
		// it. The first can be healthy while the second is gone, and only the
		// second is what the account commands need.
		fmt.Printf("NOT signed in — the HTTP session is alive (%s left) but the login has expired\n",
			client.FormatRemaining(left))
		fmt.Println("sign in again in your browser, then: renfe login --from-browser chrome")
		return nil
	}
	fmt.Printf("%s — session expires in %s\n", name, client.FormatRemaining(left))
	return nil
}

// sessionKeep holds the session open. Renfe expires an idle session in about
// half an hour; any request resets that timer, and checkSession both resets it
// and reports what is left, so this sleeps until shortly before each lapse
// rather than polling on a fixed schedule.
//
// Scope, because Renfe keeps two clocks: this refreshes the servlet session,
// which is measurably held open (checkSession reports a full window again 75s
// later with no other traffic). The *login* layered on it has its own lifetime
// that a refresh does not obviously extend, so the loop watches for the login
// dropping and stops rather than pretending it is still useful.
//
// It deliberately never calls sesionManager.extendSession: that runs
// regenerarSesion server-side, fails with U014 outside the expiry warning
// window, and the login was gone immediately afterwards during testing.
func sessionKeep(args []string, correlationIDs ...string) error {
	fs, cf := newCommonFlags("session keep")
	every := fs.Duration("every", 0, "fixed refresh interval; default is adaptive, just before each lapse")
	forDur := fs.Duration("for", 0, "stop after this long (default: until interrupted)")
	quiet := fs.Bool("quiet", false, "only report problems")
	parseFlags(fs, args)

	cl, err := authClient(correlationIDs...)
	if err != nil {
		return err
	}

	// Ctrl-C should end the loop cleanly, not leave the user wondering whether
	// the session was left in a half-refreshed state.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)

	var deadline <-chan time.Time
	if *forDur > 0 {
		t := time.NewTimer(*forDur)
		defer t.Stop()
		deadline = t.C
	}

	for {
		left, err := cl.SessionRemaining()
		verified := false
		if err != nil {
			if errors.Is(err, client.ErrSessionExpired) {
				return err
			}
			// A transient network blip should not end a long-running keep-alive.
			stderrEventFor(firstCorrelationID(correlationIDs), slog.LevelWarn, "session refresh failed; retrying", "component", "session", "error_type", fmt.Sprintf("%T", err), "retry_after_ms", time.Minute.Milliseconds())
			left = 3 * time.Minute
		} else {
			// The servlet session outlives the login, so a keep-alive that only
			// watched the timer would happily "succeed" for half an hour after
			// the account commands stopped working.
			name, werr := cl.Whoami()
			switch {
			case werr != nil:
				// Could not confirm either way; say so and try again next tick
				// rather than reporting a session we did not verify.
				stderrEventFor(firstCorrelationID(correlationIDs), slog.LevelWarn, "session login check failed; retrying", "component", "session", "error_type", fmt.Sprintf("%T", werr))
			case name == "":
				return fmt.Errorf("the login expired (the HTTP session is still alive) — " +
					"sign in again in your browser, then: renfe login --from-browser chrome")
			case name != "":
				verified = true
				if cf.structured() {
					if eerr := emitStream(cf, newSessionState(name, left)); eerr != nil {
						return eerr
					}
				} else if !*quiet {
					fmt.Printf("%s  session ok, expires in %s\n", time.Now().Format("15:04:05"), client.FormatRemaining(left))
				}
			}
		}

		wait := client.KeepAliveInterval(left, keepAliveMargin)
		if *every > 0 {
			wait = *every
		}
		if verified {
			stderrEventFor(firstCorrelationID(correlationIDs), slog.LevelDebug, "session keepalive verified", "component", "session", "remaining_ms", left.Milliseconds(), "next_check_ms", wait.Milliseconds())
		}
		timer := time.NewTimer(wait)
		select {
		case <-stop:
			timer.Stop()
			if !*quiet && !cf.structured() {
				fmt.Println("stopped")
			}
			return nil
		case <-deadline:
			timer.Stop()
			if !*quiet && !cf.structured() {
				fmt.Println("done")
			}
			return nil
		case <-timer.C:
		}
	}
}

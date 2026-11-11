package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"

	"github.com/seifreed/renfecli/internal/client"
	"github.com/seifreed/renfecli/internal/config"
	"github.com/seifreed/renfecli/internal/store"
)

// sessionFile is the machine-managed session cache, kept apart from the
// user-authored config.toml so rewriting it never touches hand-edited settings.
const sessionFile = "session.json"

// session is what `login` stores and the account commands read back.
type session struct {
	Cookie string `json:"cookie"`
}

// loadSession returns the stored Cookie header, preferring an explicit
// [auth] cookie in config.toml over the cached one from `login`.
func loadSession(cfg config.Config, correlationIDs ...string) string {
	if cfg.Auth.Cookie != "" {
		warnIfOthersCanRead(config.Path(), correlationIDs...)
		return cfg.Auth.Cookie
	}
	var s session
	if err := store.Load(sessionFile, &s); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			stderrEventFor(firstCorrelationID(correlationIDs), slog.LevelWarn, "stored session could not be read", "component", "store", "operation", "load_session", "error_type", fmt.Sprintf("%T", err))
		}
		return ""
	}
	warnIfOthersCanRead(store.Path(sessionFile), correlationIDs...)
	return s.Cookie
}

// warnIfOthersCanRead reports a credential file that other users on the machine
// can read. `login` writes 0600, but config.toml is hand-written — a default
// umask makes it 0644 — and a file restored from a backup or copied between
// machines arrives with whatever mode it was given. Warn, never fail: the
// session still works, and refusing to use it would help nobody.
//
// Windows is exempt: access there is an ACL, not a mode. Go's os.Chmod only
// toggles the read-only flag and Stat reports 0666, so this would fire on every
// run for every Windows user while saying nothing true about who can read it.
func warnIfOthersCanRead(path string, correlationIDs ...string) {
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		stderrEventFor(firstCorrelationID(correlationIDs), slog.LevelWarn, "credential file permissions are too broad", "component", "auth", "mode", fmt.Sprintf("%04o", perm), "recommended_mode", "0600")
	}
}

// authClient builds a client carrying the stored session, failing with an
// actionable message when there is none. newClient already installs an [auth]
// cookie from config.toml; this only has to add the one `login` cached.
func authClient(correlationIDs ...string) (*client.Client, error) {
	cfg := loadConfig(correlationIDs...)
	cookie := loadSession(cfg, correlationIDs...)
	if cookie == "" {
		return nil, fmt.Errorf("not signed in — run: renfe login --from-browser chrome")
	}
	cl := newClient(cfg, correlationIDs...)
	if cookie == cfg.Auth.Cookie {
		// newClient installed it (or reported why it would not); either way the
		// gate has already run.
		return cl, nil
	}
	if err := installSession(cl, cookie, correlationIDs...); err != nil {
		var fh foreignHost
		if errors.As(err, &fh) {
			return nil, err
		}
		return nil, fmt.Errorf("stored session is unusable (%w) — run renfe login again", err)
	}
	return cl, nil
}

func cmdLogin(args []string, correlationIDs ...string) error {
	fs, cf := newCommonFlags("login")
	fromBrowser := fs.String("from-browser", "", "name one browser's cookie store: chrome|chromium|firefox|safari|edge|brave (omit the flag to search every installed browser)")
	fromStdin := fs.Bool("stdin", false, "read a raw Cookie header from stdin instead")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	var candidates []client.BrowserSession
	switch {
	case *fromStdin:
		raw, err := io.ReadAll(bufio.NewReader(os.Stdin))
		if err != nil {
			return err
		}
		cookie := strings.TrimSpace(string(raw))
		if cookie == "" {
			return fmt.Errorf("nothing on stdin — pipe the Cookie header from your browser's devtools")
		}
		if !client.CookieLooksAuthed(cookie) {
			stderrEventFor(firstCorrelationID(correlationIDs), slog.LevelWarn, "input cookie header does not look authenticated; storing for verification", "component", "auth", "source", "stdin")
		}
		candidates = []client.BrowserSession{{Browser: "stdin", Cookie: cookie}}
	default:
		// Omitting the flag leaves this "", which searches every installed
		// browser — the usual form. A bare `--from-browser` with no value is a
		// usage error the flag package reports, not a way to say "any".
		var err error
		if candidates, err = client.CookiesFromBrowser(*fromBrowser); err != nil {
			return err
		}
	}

	// Verify BEFORE storing, and try every candidate. A browser's cookie file
	// keeps the last session it wrote even after that session has died
	// server-side — JSESSIONID is a session cookie the browser holds in memory,
	// so the copy on disk routinely outlives it. Saving first would let one
	// stale lift destroy a session that was still working, and stopping at the
	// first stale one would miss the browser you are actually logged in on.
	live, name, err := firstLiveSession(newClient(loadConfig(correlationIDs...), correlationIDs...), candidates)
	if err != nil {
		return err
	}
	if name == "" {
		tried := make([]string, 0, len(candidates))
		for _, c := range candidates {
			tried = append(tried, c.Browser)
		}
		return fmt.Errorf("those cookies are stale (tried: %s): the browser still has them "+
			"on disk, but Renfe no longer accepts them, so nothing was stored.\n"+
			"Open venta.renfe.com in your browser, sign in (or reload to confirm you still are), "+
			"then run this again", strings.Join(tried, ", "))
	}
	if err := store.Save(sessionFile, session{Cookie: live.Cookie}); err != nil {
		return fmt.Errorf("could not store the session: %w", err)
	}
	stderrEventFor(firstCorrelationID(correlationIDs), slog.LevelInfo, "browser session stored", "component", "store", "outcome", "success")
	if emitted, err := emitStructured(cf, struct {
		Name string `json:"name"`
	}{name}); emitted {
		return err
	}
	fmt.Printf("signed in as %s\n", name)
	return nil
}

// firstLiveSession asks Renfe about each candidate in turn and returns the
// first it still accepts, with the name it belongs to. An empty name means
// every candidate was stale — which is not an error, just an answer.
func firstLiveSession(cl *client.Client, candidates []client.BrowserSession) (client.BrowserSession, string, error) {
	for _, cand := range candidates {
		if err := cl.SetCookies(cand.Cookie); err != nil {
			return cand, "", err
		}
		name, err := cl.Whoami()
		if err != nil {
			return cand, "", fmt.Errorf("renfe rejected that session, so nothing was stored: %w", err)
		}
		if name != "" {
			return cand, name, nil
		}
	}
	return client.BrowserSession{}, "", nil
}

func cmdWhoami(args []string, correlationIDs ...string) error {
	fs, cf := newCommonFlags("whoami")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	cl, err := authClient(correlationIDs...)
	if err != nil {
		return err
	}
	name, err := cl.Whoami()
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("the stored session is no longer signed in — run: renfe login --from-browser chrome")
	}
	card, err := cl.Card()
	if err != nil {
		// The name is the answer to "who am I"; the loyalty card is a bonus and
		// not every account has one.
		stderrEventFor(firstCorrelationID(correlationIDs), slog.LevelWarn, "Renfe card could not be read", "component", "auth", "error_type", fmt.Sprintf("%T", err))
		card = &client.Card{}
	}
	if emitted, err := emitStructured(cf, struct {
		Name string       `json:"name"`
		Card *client.Card `json:"card,omitempty"`
	}{name, card}); emitted {
		return err
	}
	fmt.Println(name)
	if card.Number != "" {
		fmt.Printf("+Renfe %s  nº %s  %s points\n", card.Level, card.Number, card.Points)
	}
	return nil
}

func cmdTrips(args []string, correlationIDs ...string) error {
	fs, cf := newCommonFlags("trips")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	cl, err := authClient(correlationIDs...)
	if err != nil {
		return err
	}
	trips, err := cl.Trips()
	if err != nil {
		return err
	}
	if emitted, err := emitStructured(cf, trips); emitted {
		return err
	}
	return printTrips(os.Stdout, trips)
}

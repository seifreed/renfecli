package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/seifreed/renfecli/internal/client"
	"github.com/seifreed/renfecli/internal/config"
)

func stderrEvent(level slog.Level, message string, args ...any) {
	clean := make([]any, len(args))
	for i, arg := range args {
		if s, ok := arg.(string); ok {
			clean[i] = safeText(s)
		} else {
			clean[i] = arg
		}
	}
	logger().Log(context.Background(), level, safeText(message), clean...)
}

func logger() *slog.Logger {
	return newLogger()
}

func newLogger() *slog.Logger {
	level := new(slog.LevelVar)
	switch strings.ToLower(os.Getenv("RENFE_LOG_LEVEL")) {
	case "debug":
		level.Set(slog.LevelDebug)
	case "warn", "warning":
		level.Set(slog.LevelWarn)
	case "error":
		level.Set(slog.LevelError)
	default:
		level.Set(slog.LevelInfo)
	}
	opts := &slog.HandlerOptions{Level: level}
	handler := slog.Handler(slog.NewTextHandler(os.Stderr, opts))
	if strings.EqualFold(os.Getenv("RENFE_LOG_FORMAT"), "json") {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(handler.WithAttrs([]slog.Attr{slog.String("service", "renfecli")}))
}

// common flags shared by every subcommand.
//
// There is deliberately no --lang: the search bean answers in Spanish whatever
// idiomaBusqueda the form carried (verified against ES/CA/EN — byte-identical
// replies), so the flag could only ever have lied about what it did.
type common struct {
	jsonOut bool
	toon    bool
}

// structured reports whether the caller asked for machine-readable output, in
// either format. Anything that suppresses human chatter has to ask this rather
// than test jsonOut, or --toon silently behaves like neither.
func (c *common) structured() bool { return c.toon || c.jsonOut }

// newCommonFlags builds a flag set carrying the common flags, the starting
// point for every command that takes them.
func newCommonFlags(name string) (*flag.FlagSet, *common) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	return fs, addCommon(fs)
}

func addCommon(fs *flag.FlagSet) *common {
	c := &common{}
	fs.BoolVar(&c.jsonOut, "json", false, "emit JSON to stdout")
	fs.BoolVar(&c.toon, "toon", false, "emit TOON (token-oriented, fewer tokens than JSON) to stdout")
	return c
}

// allowSessionEnv opts a non-Renfe host back into receiving the stored session
// (see installSession). It is named after what it does, at length, because
// setting it hands a credential to whatever RENFE_BASE_URL points at.
const allowSessionEnv = "RENFE_ALLOW_SESSION_ON_CUSTOM_HOST"

// newClient builds a client carrying any stored session.
func newClient(cfg config.Config) *client.Client {
	cl := client.New()
	cl.LogEvent = stderrEvent
	// RENFE_BASE_URL points the client at an alternate host (a debugging proxy,
	// a mock, a replay server) instead of production.
	if u := os.Getenv("RENFE_BASE_URL"); u != "" {
		cl.BaseURL = u
	}
	// The read commands all work anonymously, so a config cookie that cannot be
	// installed is a warning rather than the end of the run.
	if cfg.Auth.Cookie != "" {
		if err := installSession(cl, cfg.Auth.Cookie); err != nil {
			stderrEvent(slog.LevelWarn, "configured session cookie ignored", "component", "auth", "error_type", fmt.Sprintf("%T", err))
		}
	}
	return cl
}

// installSession puts a stored Renfe session in the client's jar — but only if
// the client is actually pointed at Renfe.
//
// RENFE_BASE_URL is a debugging knob, and it redirects the credential along
// with the requests: set it to any https host and that host is handed the
// user's session cookie, silently, by a command as ordinary as `renfe trips`.
// Cookies are stored Secure, which already keeps them off a plain-http host
// (the jar makes an exception for loopback, which is this machine), but an
// https one receives them. So a host that is neither Renfe nor this machine
// does not get the session unless it is explicitly asked for.
func installSession(cl *client.Client, cookie string) error {
	if err := sessionAllowedOn(cl.BaseURL); err != nil {
		return err
	}
	return cl.SetCookies(cookie)
}

// sessionAllowedOn reports whether a stored credential may be sent to base.
func sessionAllowedOn(base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s is not a usable URL (%q)", "RENFE_BASE_URL", base)
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == renfeHost || strings.HasSuffix(host, "."+renfeHost):
		return nil
	case host == "localhost" || net.ParseIP(host).IsLoopback():
		// A proxy or mock on this machine is the user's own process.
		return nil
	case os.Getenv(allowSessionEnv) == "1":
		stderrEvent(slog.LevelWarn, "stored session sent to custom host by explicit opt-in", "component", "auth", "host", safeField(u.Host), "override", allowSessionEnv)
		return nil
	}
	return foreignHost{fmt.Errorf("refusing to send the stored Renfe session to %s: RENFE_BASE_URL "+
		"points away from %s. The anonymous commands (search, calendar, stops, stations) still "+
		"work; set %s=1 if that host really should receive your session",
		u.Host, renfeHost, allowSessionEnv)}
}

// foreignHost marks the refusal above, so a caller can tell "this host must not
// have your session" from "this credential is malformed" and not answer the
// first by telling the user to log in again.
type foreignHost struct{ err error }

func (e foreignHost) Error() string { return e.err.Error() }
func (e foreignHost) Unwrap() error { return e.err }

// renfeHost is the host the session belongs to, taken from the client's own
// default so the two cannot drift apart.
var renfeHost = mustHost(client.BaseURL)

func mustHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		panic("client.BaseURL is not a URL: " + raw)
	}
	return strings.ToLower(u.Hostname())
}

// loadConfig reads config.toml, downgrading a read failure to a warning: every
// command works without a config, and none of them is destructive enough that a
// broken file should abort the run.
func loadConfig() config.Config {
	cfg, err := config.Load()
	if err != nil {
		stderrEvent(slog.LevelWarn, "configuration could not be read; using defaults", "component", "config", "error_type", fmt.Sprintf("%T", err))
	}
	return cfg
}

// parseFlags parses args into fs, letting flags appear among positionals (see
// reorderArgs). Centralised so every command parses identically and none forgets
// the reorder step — omitting it silently drops flags placed after positionals.
func parseFlags(fs *flag.FlagSet, args []string) {
	reordered, dangling := reorderArgs(fs, args)
	if dangling != "" {
		// The stdlib reports a missing value only when the flag is the last
		// token, which the reorder guarantees it never is — the "--" terminator
		// would be parsed as the value instead, and the user would be told
		// `bad date "--"` about a "--" they never typed. Hand the flag over on
		// its own so the stdlib produces its own message and exit code.
		_ = fs.Parse([]string{dangling})
		return
	}
	_ = fs.Parse(reordered)
}

// reorderArgs lets flags appear anywhere among positional args. The stdlib flag
// parser stops at the first positional, so `renfe search madrid barcelona --json`
// would silently drop --json; this hoists flags (and their values) ahead of a
// `--` terminator so a normal fs.Parse sees them all. Honours bool flags (no
// value), `--flag=value`, and an explicit `--`.
//
// dangling names a known non-bool flag that ended the command line with no
// value; the caller reports it, since after the reorder the stdlib cannot.
func reorderArgs(fs *flag.FlagSet, args []string) (reordered []string, dangling string) {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		// A leading '-' followed by a digit or '.' is a negative number, not a
		// flag — no flag here starts with a digit. Keep it positional so it
		// reaches the command as a value, not a parse error.
		if len(a) > 1 && a[0] == '-' && (a[1] < '0' || a[1] > '9') && a[1] != '.' {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if strings.IndexByte(name, '=') >= 0 {
				continue // --flag=value: value is in the same token
			}
			if f := fs.Lookup(name); f != nil && !isBoolFlag(f) {
				if i+1 >= len(args) {
					return nil, a
				}
				flags = append(flags, args[i+1]) // consume this flag's value
				i++
			}
			continue
		}
		positional = append(positional, a)
	}
	out := make([]string, 0, len(flags)+1+len(positional))
	out = append(out, flags...)
	out = append(out, "--") // keep positionals positional even if they start with '-'
	return append(out, positional...), ""
}

// nonNegative rejects a count flag given a negative value. Zero keeps whatever
// meaning the flag documents (for --limit, no cap), but a negative one is a
// typo, and reading it as "no cap" or as one passenger hides the mistake.
func nonNegative(name string, n int) error {
	if n < 0 {
		return fmt.Errorf("%s cannot be negative (got %d)", name, n)
	}
	return nil
}

func isBoolFlag(f *flag.Flag) bool {
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && bf.IsBoolFlag()
}

// Package client is a thin Go client for the (unofficial) venta.renfe.com
// booking API. It speaks the same HTTP shapes the web app does — a session
// bootstrap, the search form POST, then DWR calls for the data — and presents
// Chrome's TLS fingerprint (uTLS) so the bot-detection layer keeps it on the
// no-challenge path.
package client

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/seifreed/renfecli/internal/observability"
)

const (
	// BaseURL is the booking host. Every endpoint lives under /vol/ on it.
	BaseURL = "https://venta.renfe.com"
	// acceptLanguage is fixed: Renfe's beans reply in Spanish whatever this (or
	// the form's idiomaBusqueda) says — verified against ES/CA/EN, byte-identical
	// replies — so a language setting could only ever have lied about what it did.
	acceptLanguage = "es-ES,es;q=0.9,en;q=0.8"
	// DefaultUA mirrors a current desktop Chrome so requests look like the web app.
	DefaultUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"
)

// Client is the API entrypoint. The zero value is not usable — use New.
type Client struct {
	HTTP      *http.Client
	BaseURL   string
	UserAgent string
	// correlationID ties HTTP diagnostics emitted by this client instance together.
	correlationID string

	// Logf, when set, receives human-readable diagnostics about otherwise-silent
	// resilience events (throttle retries, transport fallback). Diagnostics belong
	// on stderr; nil disables them. The library never writes to os.Stderr itself —
	// the caller routes the output, keeping the client free of process I/O.
	Logf func(format string, args ...any)
	// LogEvent receives structured diagnostics when the caller supports levels.
	// Logf remains as a compatibility fallback for existing embedders.
	LogEvent func(level slog.Level, message string, args ...any)

	// transportErr records why the uTLS Chrome transport could not initialise (nil
	// when it did). New cannot log it — Logf is set by the caller afterwards — so
	// it is surfaced lazily on the first request.
	transportErr error
	warnOnce     sync.Once

	// bootstrapped guards the one-time GET that seeds the JSESSIONID every later
	// call rides on.
	bootstrapOnce sync.Once
	bootstrapErr  error
}

// New returns a Client with web-app-like defaults (anonymous, Spanish). Its HTTP
// client presents Chrome's TLS (JA3) fingerprint via uTLS; if that fails to
// initialise it falls back to the stdlib transport. A cookie jar is always
// installed: the whole API is session-stateful — the search form POST stores the
// route on the server and the DWR calls read it back out.
func New() *Client {
	return NewWithCorrelationID(observability.NewCorrelationID())
}

// NewWithCorrelationID constructs a Client whose diagnostics join a caller operation.
func NewWithCorrelationID(correlationID string) *Client {
	jar, _ := cookiejar.New(nil) // the nil-options constructor cannot fail
	hc := &http.Client{Timeout: 45 * time.Second, Jar: jar}
	c := &Client{
		HTTP:          hc,
		BaseURL:       BaseURL,
		UserAgent:     DefaultUA,
		correlationID: correlationID,
	}
	if tr, err := newChromeTransport(); err == nil {
		hc.Transport = tr
	} else {
		c.transportErr = err
	}
	return c
}

// logEvent sends diagnostics to the structured hook, falling back for older callers.
func (c *Client) logEvent(level slog.Level, message string, attrs ...any) {
	if c.LogEvent != nil {
		if c.correlationID != "" {
			attrs = append([]any{"correlation_id", c.correlationID}, attrs...)
		}
		c.LogEvent(level, message, attrs...)
	} else if c.Logf != nil && level >= slog.LevelWarn {
		c.Logf("%s %v", message, attrs)
	}
}

// SetCookies seeds the jar with a raw "name=value; name=value" Cookie header,
// adopting a logged-in browser session (see the login command). Reads work
// anonymously; only the "my trips" endpoints need this.
func (c *Client) SetCookies(header string) error {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return err
	}
	pairs := parseCookieHeader(header)
	cs := make([]*http.Cookie, 0, len(pairs))
	for _, p := range pairs {
		// These flags describe how a *server* would have us store the cookie; we
		// are replaying one the browser already holds, and the jar only ever
		// sends it back to BaseURL over TLS.
		cs = append(cs, &http.Cookie{
			Name: p.name, Value: p.val, Path: "/",
			Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
	}
	if len(cs) == 0 {
		return errors.New("no cookies in header")
	}
	c.HTTP.Jar.SetCookies(u, cs)
	return nil
}

// APIError carries a non-2xx response so callers (and agents) can branch on it.
// RetryAfter is the parsed Retry-After header on a 429/503 (>=0 when present).
type APIError struct {
	Status     int
	Body       string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("renfe api: HTTP %d", e.Status)
}

// HTTPStatus reports the HTTP status carried by err when it is (or wraps) an
// *APIError. ok is false for any other error. Callers branch on a status (404,
// 401…) without reaching into the error type, and it unwraps wrapped errors.
func HTTPStatus(err error) (status int, ok bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status, true
	}
	return 0, false
}

// Automatic backoff on throttling (HTTP 429/503): retry up to maxRetries times,
// honouring Retry-After, else exponential backoff with jitter, each wait capped.
const (
	maxRetries       = 3
	defaultRetryBase = 500 * time.Millisecond
	maxRetryWait     = 10 * time.Second
	maxBodyBytes     = 32 << 20
)

func isRateLimited(err error) bool {
	status, ok := HTTPStatus(err)
	return ok && (status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable)
}

// parseRetryAfter reads the delta-seconds form of Retry-After; -1 when absent.
func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return -1
	}
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	return -1
}

func retryWait(err error, backoff time.Duration) time.Duration {
	var ae *APIError
	if errors.As(err, &ae) && ae.RetryAfter >= 0 {
		return capWait(ae.RetryAfter)
	}
	return capWait(backoff + time.Duration(rand.Int63n(int64(250*time.Millisecond))))
}

func capWait(d time.Duration) time.Duration {
	if d > maxRetryWait {
		return maxRetryWait
	}
	if d < 0 {
		return 0
	}
	return d
}

// newReq builds a request with the shared headers (accept, language, user-agent,
// origin/referer) plus any extra headers the endpoint needs. Pass extra=nil when
// none. Cookies come from the jar, so they are never set here.
func (c *Client) newReq(method, path string, body io.Reader, extra map[string]string) (*http.Request, error) {
	req, err := http.NewRequest(method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "*/*")
	req.Header.Set("accept-language", acceptLanguage)
	req.Header.Set("user-agent", c.UserAgent)
	req.Header.Set("origin", c.BaseURL)
	req.Header.Set("referer", c.BaseURL+"/vol/inicio.do")
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	return req, nil
}

// do sends req, reads the (capped) response body, and turns a non-2xx status
// into an *APIError carrying that body and any Retry-After. The single place
// requests are executed and HTTP errors are mapped.
func (c *Client) do(req *http.Request) ([]byte, error) {
	started := time.Now()
	c.warnOnce.Do(func() {
		if c.transportErr != nil {
			c.logEvent(slog.LevelWarn, "TLS transport fallback", "component", "client", "error_type", fmt.Sprintf("%T", c.transportErr))
		}
	})
	resp, err := c.HTTP.Do(req)
	if err != nil {
		c.logEvent(slog.LevelDebug, "HTTP request completed", "component", "client", "method", req.Method, "operation", req.URL.Path,
			"outcome", "transport_error", "error_type", fmt.Sprintf("%T", err), "duration_ms", time.Since(started).Milliseconds())
		return nil, requestError{method: req.Method, path: req.URL.Path, cause: err}
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.logEvent(slog.LevelDebug, "HTTP request completed", "component", "client", "method", req.Method, "operation", req.URL.Path,
			"status", resp.StatusCode, "outcome", "http_error", "duration_ms", time.Since(started).Milliseconds())
		// On a non-2xx, surface whatever body we managed to read as error context
		// (best effort) — the status is the actionable signal, not the read error.
		return nil, &APIError{
			Status:     resp.StatusCode,
			Body:       string(data),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	// A 2xx whose body read failed mid-stream must not pass as success: the DWR
	// payload would parse as "truncated" at best and silently short at worst.
	if readErr != nil {
		c.logEvent(slog.LevelDebug, "HTTP request completed", "component", "client", "method", req.Method, "operation", req.URL.Path,
			"status", resp.StatusCode, "outcome", "read_error", "error_type", fmt.Sprintf("%T", readErr),
			"duration_ms", time.Since(started).Milliseconds())
		return nil, requestError{method: req.Method, path: req.URL.Path, cause: readErr}
	}
	c.logEvent(slog.LevelDebug, "HTTP request completed", "component", "client", "method", req.Method, "operation", req.URL.Path,
		"status", resp.StatusCode, "outcome", "success", "duration_ms", time.Since(started).Milliseconds())
	return data, nil
}

type requestError struct {
	method string
	path   string
	cause  error
}

func (e requestError) Error() string {
	return fmt.Sprintf("HTTP %s %s failed (%T)", e.method, e.path, e.cause)
}

func (e requestError) Unwrap() error { return e.cause }

// doRetry runs fn, retrying transient throttling (429/503) so a burst of calls —
// a price calendar sweep, several searches in one session — degrades gracefully
// instead of failing. Non-throttle errors return immediately.
func (c *Client) doRetry(what string, fn func() ([]byte, error)) ([]byte, error) {
	backoff := defaultRetryBase
	for attempt := 0; ; attempt++ {
		data, err := fn()
		if !isRateLimited(err) || attempt >= maxRetries {
			return data, err
		}
		wait := retryWait(err, backoff)
		status, _ := HTTPStatus(err)
		c.logEvent(slog.LevelWarn, "HTTP request throttled; retrying", "component", "client", "status", status, "operation", what,
			"attempt", attempt+1, "max_attempts", maxRetries, "retry_after_ms", wait.Milliseconds())
		time.Sleep(wait)
		backoff *= 2
	}
}

// bootstrap performs the one-time GET that hands out the JSESSIONID every later
// call rides on. Renfe rejects a search POST that arrives without one.
func (c *Client) bootstrap() error {
	c.bootstrapOnce.Do(func() {
		_, c.bootstrapErr = c.doRetry("inicio.do", func() ([]byte, error) {
			req, err := c.newReq("GET", "/vol/inicio.do", nil, map[string]string{
				"accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
			})
			if err != nil {
				return nil, err
			}
			return c.do(req)
		})
	})
	return c.bootstrapErr
}

// postForm submits an application/x-www-form-urlencoded body and returns the
// response, following redirects (the search form answers with a 302).
func (c *Client) postForm(path string, form url.Values) ([]byte, error) {
	if err := c.bootstrap(); err != nil {
		return nil, fmt.Errorf("session bootstrap: %w", err)
	}
	return c.doRetry(path, func() ([]byte, error) {
		req, err := c.newReq("POST", path, strings.NewReader(form.Encode()), map[string]string{
			"content-type": "application/x-www-form-urlencoded",
			"accept":       "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		})
		if err != nil {
			return nil, err
		}
		return c.do(req)
	})
}

// callDWR invokes a DWR bean method whose single argument is the given ordered
// string map, decoding the reply into out.
func (c *Client) callDWR(bean, method, page string, params []DWRParam, out any) error {
	body := dwrCall(bean, method, page, scriptSessionID(c.cookie(dwrSessionCookie)), params)
	path := fmt.Sprintf(dwrPath, bean, method)
	data, err := c.doRetry(bean+"."+method, func() ([]byte, error) {
		req, err := c.newReq("POST", path, strings.NewReader(body), map[string]string{
			// DWR's plaincall endpoint expects text/plain, not a form type.
			"content-type": "text/plain",
			"referer":      c.BaseURL + page,
		})
		if err != nil {
			return nil, err
		}
		return c.do(req)
	})
	if err != nil {
		return err
	}
	return decodeDWR(string(data), out)
}

// cookie returns the named cookie's value from the jar for BaseURL, or "".
func (c *Client) cookie(name string) string {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return ""
	}
	for _, ck := range c.HTTP.Jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

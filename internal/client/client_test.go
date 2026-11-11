package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// The retry layer is the one piece of the client that decides on its own to
// wait and try again. It is worth pinning down precisely: retrying what should
// not be retried costs the user their rate-limit budget, and giving up on a
// 429 that carried a Retry-After throws away the one instruction the server
// sent.

func TestParseRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"", -1},
		{"5", 5 * time.Second},
		{" 12 ", 12 * time.Second},
		{"0", 0},
		{"-3", -1},                            // nonsense; treat as absent
		{"Wed, 21 Oct 2026 07:28:00 GMT", -1}, // the HTTP-date form, not handled
	} {
		if got := parseRetryAfter(tc.in); got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// Retry-After is an instruction, not a hint: when the server sends one it must
// win over the local backoff — but still be capped, so a hostile or mistaken
// "Retry-After: 86400" cannot park the CLI for a day.
func TestRetryWaitHonoursRetryAfterAndCapsIt(t *testing.T) {
	err := &APIError{Status: 429, RetryAfter: 3 * time.Second}
	if got := retryWait(err, time.Second); got != 3*time.Second {
		t.Errorf("retryWait = %v, want the server's 3s", got)
	}
	huge := &APIError{Status: 429, RetryAfter: 24 * time.Hour}
	if got := retryWait(huge, time.Second); got != maxRetryWait {
		t.Errorf("retryWait = %v, want it capped at %v", got, maxRetryWait)
	}
	// No Retry-After: the local backoff plus jitter, never longer than the cap.
	plain := &APIError{Status: 503, RetryAfter: -1}
	for i := 0; i < 20; i++ {
		got := retryWait(plain, defaultRetryBase)
		if got < defaultRetryBase || got > defaultRetryBase+250*time.Millisecond {
			t.Fatalf("jittered backoff out of range: %v", got)
		}
	}
}

func TestDoRetriesThrottlingAndNothingElse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		wantCall int32
	}{
		{"429 is retried", http.StatusTooManyRequests, maxRetries + 1},
		{"503 is retried", http.StatusServiceUnavailable, maxRetries + 1},
		{"500 is not", http.StatusInternalServerError, 1},
		{"404 is not", http.StatusNotFound, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.Header().Set("Retry-After", "0") // no sleeping in tests
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			c := newTestClient(srv)
			_, err := c.doRetry("t", func() ([]byte, error) {
				req, _ := c.newReq("GET", "/vol/inicio.do", nil, nil)
				return c.do(req)
			})
			if status, ok := HTTPStatus(err); !ok || status != tc.status {
				t.Fatalf("HTTPStatus(%v) = %d, %v; want %d", err, status, ok, tc.status)
			}
			if got := atomic.LoadInt32(&calls); got != tc.wantCall {
				t.Errorf("server saw %d calls, want %d", got, tc.wantCall)
			}
		})
	}
}

func TestDoRetryGivesUpAndReportsTheLastError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "0")
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := newTestClient(srv)
	var logged []string
	c.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	data, err := c.doRetry("t", func() ([]byte, error) {
		req, _ := c.newReq("GET", "/vol/inicio.do", nil, nil)
		return c.do(req)
	})
	if err != nil || string(data) != "ok" {
		t.Fatalf("a throttle that clears should succeed: %q, %v", data, err)
	}
	if len(logged) != 2 {
		t.Errorf("each retry should be reported once, got %v", logged)
	}
}

// HTTPStatus is the documented way to branch on a status, so it has to see
// through a wrapped error and say no to anything that is not an APIError.
func TestHTTPStatus(t *testing.T) {
	wrapped := fmt.Errorf("search: %w", &APIError{Status: 404})
	if status, ok := HTTPStatus(wrapped); !ok || status != 404 {
		t.Errorf("HTTPStatus(wrapped) = %d, %v", status, ok)
	}
	if _, ok := HTTPStatus(errors.New("plain")); ok {
		t.Error("a plain error has no HTTP status")
	}
	if _, ok := HTTPStatus(nil); ok {
		t.Error("nil has no HTTP status")
	}
}

func TestAPIErrorDoesNotExposeRemoteBody(t *testing.T) {
	e := &APIError{Status: 403, Body: "private-account-detail"}
	msg := e.Error()
	if !strings.Contains(msg, "HTTP 403") || strings.Contains(msg, "private-account-detail") {
		t.Errorf("unexpected message: %q", msg)
	}
}

// bootstrap must happen exactly once per client, however many searches follow:
// it is what seeds the JSESSIONID, and re-running it would reset the server's
// search context mid-session.
func TestBootstrapRunsOnce(t *testing.T) {
	var inicio int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "inicio.do") {
			atomic.AddInt32(&inicio, 1)
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := newTestClient(srv)
	for i := 0; i < 3; i++ {
		if _, err := c.postForm("/vol/buscarTren.do", nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(&inicio); got != 1 {
		t.Errorf("inicio.do was fetched %d times, want 1", got)
	}
}

func TestSetCookies(t *testing.T) {
	c := New()
	if err := c.SetCookies("JSESSIONID=abc; SSOInfo=xyz"); err != nil {
		t.Fatal(err)
	}
	if got := c.cookie("JSESSIONID"); got != "abc" {
		t.Errorf("JSESSIONID = %q", got)
	}
	if got := c.cookie(dwrSessionCookie); got != "" {
		t.Errorf("an absent cookie should read as empty, got %q", got)
	}
	// What counts as a cookie is parseCookieHeader's rule (see
	// TestCookieLooksAuthed); SetCookies' own job is to refuse a header that
	// leaves the jar empty rather than store a session that cannot work.
	for _, header := range []string{"   ", "malformed", "=novalue", "JSESSIONID="} {
		if err := c.SetCookies(header); err == nil {
			t.Errorf("SetCookies(%q) should fail, not silently store nothing", header)
		}
	}
}

// Every request has to look like the web app's. The TLS fingerprint is the
// hard half; these headers are the easy half to drop by accident, and losing
// one is exactly how a working client starts getting challenged.
func TestNewReqCarriesTheWebAppHeaders(t *testing.T) {
	c := New()
	c.BaseURL = "https://venta.renfe.com"
	req, err := c.newReq("POST", "/vol/buscarTren.do", nil, map[string]string{"content-type": "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	for header, want := range map[string]string{
		"accept-language": acceptLanguage,
		"user-agent":      DefaultUA,
		"origin":          "https://venta.renfe.com",
		"referer":         "https://venta.renfe.com/vol/inicio.do",
		"content-type":    "text/plain", // the per-endpoint extras win
	} {
		if got := req.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if len(req.Cookies()) > 0 {
		t.Error("cookies must come from the jar, never be set on the request")
	}
}

// A 2xx whose body dies mid-read must not pass as success: the DWR decoder
// would see a truncated script and report it as a protocol error.
func TestDoRejectsATruncatedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short"))
	}))
	defer srv.Close()

	c := newTestClient(srv)
	req, _ := c.newReq("GET", "/vol/inicio.do", nil, nil)
	if _, err := c.do(req); err == nil {
		t.Fatal("a short read on a 2xx must be an error")
	}
}

func newTestClient(srv *httptest.Server) *Client {
	c := New()
	c.HTTP = srv.Client()
	c.HTTP.Jar = New().HTTP.Jar
	c.BaseURL = srv.URL
	return c
}

// If the Chrome-fingerprint transport stopped verifying certificates, every
// other protection here would be decoration: the session cookie and the whole
// conversation would be readable by anyone on the path. The disguise is about
// what the handshake looks like, never about whether it is checked.
func TestChromeTransportVerifiesCertificates(t *testing.T) {
	// httptest's TLS server presents a self-signed certificate no root trusts.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshake is the point
	defer srv.Close()

	conn, err := chromeDial(context.Background(), "tcp", strings.TrimPrefix(srv.URL, "https://"),
		chromeSpecHTTP1, func(host string) *utls.Config { return &utls.Config{ServerName: host} })
	if err == nil {
		conn.Close()
		t.Fatal("the uTLS dial accepted an untrusted certificate")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Errorf("dial failed for the wrong reason: %v", err)
	}
}

package main

import (
	"encoding/json"
	"github.com/seifreed/renfecli/internal/client"
	"github.com/seifreed/renfecli/internal/config"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/seifreed/renfecli/internal/store"
)

// runLogin feeds a Cookie header on stdin and runs `renfe login --stdin`.
func runLogin(t *testing.T, cookie string, args ...string) error {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = orig })
	go func() {
		_, _ = w.WriteString(cookie)
		w.Close()
	}()
	return cmdLogin(append([]string{"--stdin"}, args...))
}

func TestLoadSessionLogsCorruptStoreButNotMissingFile(t *testing.T) {
	t.Setenv("RENFE_CONFIG_DIR", t.TempDir())
	t.Setenv("RENFE_LOG_FORMAT", "json")
	var cfg config.Config
	missing := captureStderr(t, func() {
		if got := loadSession(cfg); got != "" {
			t.Fatalf("missing session = %q", got)
		}
	})
	if missing != "" {
		t.Fatalf("missing session should be a normal state: %s", missing)
	}
	if err := os.WriteFile(store.Path(sessionFile), []byte(`{"cookie":"secret-cookie", broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	corrupt := captureStderr(t, func() {
		if got := loadSession(cfg); got != "" {
			t.Fatalf("corrupt session = %q", got)
		}
	})
	if !strings.Contains(corrupt, `"operation":"load_session"`) || !strings.Contains(corrupt, `"level":"WARN"`) || strings.Contains(corrupt, "secret-cookie") {
		t.Fatalf("corrupt store warning is missing or unsafe: %s", corrupt)
	}
}

// A browser keeps a dead session's cookies on disk, so `login` regularly reads
// cookies the server no longer accepts. It must verify before it stores, or one
// stale lift silently destroys a session that was still working.
func TestLoginDoesNotClobberAGoodSessionWhenTheNewOneIsStale(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RENFE_CONFIG_DIR", dir)

	good := session{Cookie: "JSESSIONID=good; SSOInfo=good"}
	if err := store.Save(sessionFile, good); err != nil {
		t.Fatal(err)
	}

	srv := signedInAs(t, "") // the server does not consider the caller signed in
	t.Setenv("RENFE_BASE_URL", srv.URL)

	err := runLogin(t, "JSESSIONID=stale; SSOInfo=stale")
	if err == nil {
		t.Fatal("a stale session should fail")
	}
	if !strings.Contains(err.Error(), "stale") || !strings.Contains(err.Error(), "nothing was stored") {
		t.Errorf("the error should explain what happened, got %q", err)
	}

	var after session
	if err := store.Load(sessionFile, &after); err != nil {
		t.Fatalf("the stored session went missing: %v", err)
	}
	if after.Cookie != good.Cookie {
		t.Errorf("the working session was overwritten: got %q", after.Cookie)
	}
}

func TestLoginStoresAnAcceptedSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RENFE_CONFIG_DIR", dir)
	srv := signedInAs(t, "MARC RIVERO")
	t.Setenv("RENFE_BASE_URL", srv.URL)

	if err := runLogin(t, "JSESSIONID=fresh; SSOInfo=fresh"); err != nil {
		t.Fatalf("login: %v", err)
	}
	var got session
	if err := store.Load(sessionFile, &got); err != nil {
		t.Fatal(err)
	}
	if got.Cookie != "JSESSIONID=fresh; SSOInfo=fresh" {
		t.Errorf("stored %q", got.Cookie)
	}
}

func TestRunLoginStoreFailureLogsSafeClassification(t *testing.T) {
	blockedDir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RENFE_CONFIG_DIR", blockedDir)
	t.Setenv("RENFE_LOG_FORMAT", "json")
	t.Setenv("RENFE_LOG_LEVEL", "info")
	t.Setenv("RENFE_BASE_URL", signedInAs(t, "MARC RIVERO").URL)
	const cookie = "JSESSIONID=private; SSOInfo=private"
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStdin := os.Stdin
	os.Stdin = reader
	t.Cleanup(func() { os.Stdin = originalStdin })
	go func() {
		_, _ = writer.WriteString(cookie)
		_ = writer.Close()
	}()
	var code int
	stderr := captureStderr(t, func() { code = run([]string{"login", "--stdin"}) })
	if code != 1 {
		t.Fatalf("run returned %d, want storage failure", code)
	}
	if strings.Contains(stderr, cookie) {
		t.Fatalf("session save failure exposed cookie: %s", stderr)
	}
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) == nil && event["event_name"] == "command_failed" {
			if event["error_code"] != "STORE_WRITE_FAILED" || event["correlation_id"] == "" || event["operation"] != "login" {
				t.Fatalf("session save failure lacks safe structured fields: %#v", event)
			}
			return
		}
	}
	t.Fatalf("session save failure event missing: %s", stderr)
}

func TestLoginRejectsEmptyStdin(t *testing.T) {
	t.Setenv("RENFE_CONFIG_DIR", t.TempDir())
	if err := runLogin(t, "   \n"); err == nil {
		t.Error("empty stdin should fail")
	}
}

// The package doc promises every command emits --json; login registered the
// flag and threw it away, so a script had no way to read back who it signed in
// as without a second call to whoami.
func TestLoginEmitsJSON(t *testing.T) {
	t.Setenv("RENFE_CONFIG_DIR", t.TempDir())
	t.Setenv("RENFE_BASE_URL", signedInAs(t, "MARC RIVERO").URL)

	out, err := capture(t, func() error {
		return runLogin(t, "JSESSIONID=fresh; SSOInfo=fresh", "--json")
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("login --json emitted something unparseable: %v\n%s", err, out)
	}
	if got.Name != "MARC RIVERO" {
		t.Errorf("name = %q", got.Name)
	}
}

// A browser keeps a dead session's cookies on disk, so the first store read is
// routinely stale while another browser is genuinely logged in. Stopping at
// the first candidate told the user to go and sign in again on a machine where
// they already were.
func TestLoginTriesEveryBrowserBeforeGivingUp(t *testing.T) {
	t.Setenv("RENFE_CONFIG_DIR", t.TempDir())
	// The stand-in recognises one cookie and not the other.
	live := dwrPayload(t, "MARC RIVERO")
	dead := dwrPayload(t, "")
	srv := renfeServerForCookie(t, "JSESSIONID=live", live, dead)
	t.Setenv("RENFE_BASE_URL", srv.URL)

	cl := newClient(loadConfig())
	got, name, err := firstLiveSession(cl, []client.BrowserSession{
		{Browser: "chrome", Cookie: "JSESSIONID=dead; SSOInfo=dead"},
		{Browser: "firefox", Cookie: "JSESSIONID=live; SSOInfo=live"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if name != "MARC RIVERO" || got.Browser != "firefox" {
		t.Errorf("picked %q from %q, want MARC RIVERO from firefox", name, got.Browser)
	}

	// Every candidate stale is an answer, not an error.
	_, name, err = firstLiveSession(newClient(loadConfig()), []client.BrowserSession{
		{Browser: "chrome", Cookie: "JSESSIONID=dead; SSOInfo=dead"},
	})
	if err != nil || name != "" {
		t.Errorf(`all-stale = (%q, %v), want ("", nil)`, name, err)
	}
}

// renfeServerForCookie answers with hit when the request carries wantCookie,
// and with miss otherwise.
func renfeServerForCookie(t *testing.T, wantCookie string, hit, miss []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Cookie"), wantCookie) {
			_, _ = w.Write(hit)
			return
		}
		_, _ = w.Write(miss)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// `login` writes 0600, but config.toml is hand-written — a default umask makes
// it 0644 — and a file copied between machines arrives with whatever mode it
// was given. A credential other local users can read is worth a word.
func TestWarnsAboutACredentialFileOthersCanRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("access on Windows is an ACL, not a mode; the check is exempt there")
	}
	dir := t.TempDir()
	t.Setenv("RENFE_CONFIG_DIR", dir)
	if err := store.Save(sessionFile, session{Cookie: "JSESSIONID=x; SSOInfo=y"}); err != nil {
		t.Fatal(err)
	}
	path := store.Path(sessionFile)

	warned := captureStderr(t, func() { loadSession(config.Config{}) })
	if warned != "" {
		t.Errorf("0600 is correct and must be silent, got: %s", warned)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	warned = captureStderr(t, func() { loadSession(config.Config{}) })
	if !strings.Contains(warned, "mode=0644") || !strings.Contains(warned, "credential file permissions are too broad") {
		t.Errorf("a world-readable credential went unmentioned: %q", warned)
	}
	if strings.Contains(warned, path) || strings.Contains(warned, "JSESSIONID=x") {
		t.Errorf("credential warning exposed its path or contents: %q", warned)
	}
	// Warning, never failing: the session still works.
	if got := loadSession(config.Config{}); got == "" {
		t.Error("the warning stopped the session being used")
	}
}

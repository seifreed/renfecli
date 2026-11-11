package client

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrSessionExpired reports that Renfe no longer recognises the stored session.
// Only the user can fix it — by signing in again in their browser — so callers
// should stop rather than retry.
var ErrSessionExpired = errors.New("the Renfe session has expired — sign in again in your browser, then run: renfe login --from-browser chrome")

// SessionRemaining reports how long Renfe will keep the session alive without
// further activity, and touches it in the process: the server's idle timer is
// reset by the request itself, so polling this is what keeps a session from
// timing out.
//
// It returns ErrSessionExpired once the session is gone.
func (c *Client) SessionRemaining() (time.Duration, error) {
	var ms int64
	if err := c.callDWR("sesionManager", "checkSession", accountPage, nil, &ms); err != nil {
		if isSessionGone(err) {
			return 0, ErrSessionExpired
		}
		return 0, err
	}
	if ms <= 0 {
		return 0, ErrSessionExpired
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// sessionGoneCodes are the application error codes Renfe raises for a session
// that has timed out, as opposed to a request that was merely wrong.
var sessionGoneCodes = []string{"U014", "Debe volver a iniciar la sesi"}

func isSessionGone(err error) bool {
	var dwrErr dwrExceptionError
	if errors.As(err, &dwrErr) {
		return dwrErr.sessionExpired
	}
	msg := err.Error()
	for _, code := range sessionGoneCodes {
		if strings.Contains(msg, code) {
			return true
		}
	}
	return false
}

// KeepAliveInterval returns how long to wait before the next refresh, given how
// much of the session is left. It aims to touch the session once per window,
// shortly before it would lapse, rather than polling on a fixed timer — Renfe
// reports the remaining time on every check, so there is no need to guess.
//
// margin is how early to come back; the result is clamped so a nearly-expired
// session is refreshed promptly and a fresh one is not hammered.
func KeepAliveInterval(remaining, margin time.Duration) time.Duration {
	const (
		minWait = 30 * time.Second
		maxWait = 20 * time.Minute
	)
	d := remaining - margin
	if d < minWait {
		d = minWait
	}
	if d > maxWait {
		d = maxWait
	}
	return d
}

// FormatRemaining renders a session's remaining time for a human.
func FormatRemaining(d time.Duration) string {
	d = d.Round(time.Second)
	if d >= time.Minute {
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

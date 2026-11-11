package client

import (
	"errors"
	"testing"
	"time"
)

func TestKeepAliveInterval(t *testing.T) {
	const margin = 2 * time.Minute
	tests := []struct {
		name      string
		remaining time.Duration
		want      time.Duration
	}{
		// A full window is capped, so a long-running keep-alive still checks in
		// periodically instead of sleeping through a server-side change.
		{"full window", 29 * time.Minute, 20 * time.Minute},
		{"half window", 10 * time.Minute, 8 * time.Minute},
		// Close to expiry: come back promptly, but never busy-loop.
		{"inside the margin", 90 * time.Second, 30 * time.Second},
		{"already expired", 0, 30 * time.Second},
		{"negative", -time.Minute, 30 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := KeepAliveInterval(tc.remaining, margin); got != tc.want {
				t.Errorf("KeepAliveInterval(%v) = %v, want %v", tc.remaining, got, tc.want)
			}
		})
	}
}

// KeepAliveInterval must always return a wait shorter than what is left, or the
// loop would wake up after the session it is meant to preserve has gone.
func TestKeepAliveIntervalNeverOversleeps(t *testing.T) {
	for m := 3; m <= 30; m++ {
		remaining := time.Duration(m) * time.Minute
		if got := KeepAliveInterval(remaining, 2*time.Minute); got >= remaining {
			t.Errorf("with %v left the loop would sleep %v", remaining, got)
		}
	}
}

func TestIsSessionGone(t *testing.T) {
	gone := []error{
		dwrExceptionError{sessionExpired: true},
		errors.New("Debe volver a iniciar la sesion"),
	}
	for _, err := range gone {
		if !isSessionGone(err) {
			t.Errorf("should be recognised as an expired session: %v", err)
		}
	}
	// An ordinary failure must not be reported as an expiry, or the keep-alive
	// would give up on a blip instead of retrying.
	for _, err := range []error{
		errors.New("dial tcp: connection refused"),
		errors.New("renfe api: HTTP 503"),
	} {
		if isSessionGone(err) {
			t.Errorf("should NOT be an expiry: %v", err)
		}
	}
}

func TestFormatRemaining(t *testing.T) {
	for d, want := range map[time.Duration]string{
		29 * time.Minute: "29m 00s",
		90 * time.Second: "1m 30s",
		45 * time.Second: "45s",
		0:                "0s",
	} {
		if got := FormatRemaining(d); got != want {
			t.Errorf("FormatRemaining(%v) = %q, want %q", d, got, want)
		}
	}
}

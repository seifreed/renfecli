// Package observability contains small primitives shared by the CLI and client.
package observability

import (
	"crypto/rand"
	"encoding/hex"
)

// NewCorrelationID returns a random, process-local correlation token.
func NewCorrelationID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(id[:])
}

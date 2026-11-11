package observability

import (
	"encoding/hex"
	"testing"
)

func TestNewCorrelationIDReturnsUnique128BitHex(t *testing.T) {
	first, second := NewCorrelationID(), NewCorrelationID()
	for _, id := range []string{first, second} {
		decoded, err := hex.DecodeString(id)
		if err != nil || len(decoded) != 16 {
			t.Fatalf("correlation ID is not 128-bit hex: %q", id)
		}
	}
	if first == second {
		t.Fatal("consecutive correlation IDs must differ")
	}
}

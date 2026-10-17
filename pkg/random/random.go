package random

import (
	"crypto/rand"
	"encoding/hex"
)

// Hex returns n random hex digits; n must be even.
func Hex(n int) string {
	b := make([]byte, n/2)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

package interest

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// newID derives a stable id from a string.
func newID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h[:])[:16]
}

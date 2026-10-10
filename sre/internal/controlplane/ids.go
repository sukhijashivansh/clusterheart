package controlplane

import (
	"crypto/rand"
	"encoding/hex"
)

// newID returns a prefixed, URL-safe random identifier, e.g. "clu_9f3a…".
func newID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

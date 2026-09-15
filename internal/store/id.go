package store

import (
	"crypto/rand"
	"encoding/hex"
)

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	// mem_<random hex>: opaque, collision-safe for a single-user local DB.
	return "mem_" + hex.EncodeToString(b)
}

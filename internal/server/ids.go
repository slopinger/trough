package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"github.com/slopinger/trough/internal/ulid"
)

// numericPageBounds lets clients page with the numbers from ulid.Number.
func numericPageBounds(pq *pageQ) {
	conv := func(s *string, upper bool) {
		if n, err := strconv.ParseInt(*s, 10, 64); err == nil && n > 0 && len(*s) < 26 {
			*s = ulid.FromNumber(n, upper)
		}
	}
	conv(&pq.MaxID, false)
	conv(&pq.SinceID, true)
	conv(&pq.MinID, true)
}

func randomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

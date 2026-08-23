// Package keygen implements the "Keygen" box: generates license keys.
package keygen

import (
	"crypto/rand"
	"strings"
)

// Crockford-style alphabet: no 0/O/1/I ambiguity.
const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// New returns a key like "K7QX3-MPA9V-ZBT2H-D8NRW-4FGSL" (5 groups of 5).
func New() string {
	b := make([]byte, 25)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	var sb strings.Builder
	for i, c := range b {
		if i > 0 && i%5 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(alphabet[int(c)%len(alphabet)])
	}
	return sb.String()
}

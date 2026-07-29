// Package idgen generates identifiers.
//
// New identifiers are UUIDv7: they embed a millisecond timestamp in their high
// bits, so rows inserted in time order land in adjacent B-tree pages instead of
// scattering across the index the way UUIDv4 does. On a ledger table that only
// ever grows and is always queried by time this matters.
package idgen

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Generator produces identifiers. Injecting it keeps use cases deterministic
// under test.
type Generator interface {
	// NewID returns a fresh, time-ordered identifier.
	NewID() string
}

// UUIDv7 is the production Generator.
type UUIDv7 struct{}

// NewID returns a new UUIDv7 string. It falls back to UUIDv4 in the practically
// impossible case that the v7 source fails, because a use case must never be
// unable to name the entity it just created.
func (UUIDv7) NewID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}

// Sequence is a deterministic Generator for tests: id-1, id-2, ...
type Sequence struct {
	Prefix string
	n      int
}

// NewID returns the next identifier in the sequence.
func (s *Sequence) NewID() string {
	s.n++
	prefix := s.Prefix
	if prefix == "" {
		prefix = "id"
	}
	return fmt.Sprintf("%s-%d", prefix, s.n)
}

// IsValidUUID reports whether s parses as a UUID. Inbound adapters use this to
// reject junk before it reaches a repository.
func IsValidUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

// giftCardAlphabet is Crockford-style base32 without I, L, O and U so that a
// code read aloud or typed by hand cannot be confused or spell something rude.
const giftCardAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var giftCardEncoding = base32.NewEncoding(giftCardAlphabet).WithPadding(base32.NoPadding)

// NewGiftCardCode returns a cryptographically random, human-transcribable code
// formatted as four groups of four characters, e.g. "7K2M-9XQF-3B4T-VW8N".
//
// 16 characters of a 32-symbol alphabet is 80 bits of entropy, which makes
// guessing a valid code infeasible even before the abuse rate limiter kicks in.
func NewGiftCardCode() (string, error) {
	const groups, groupSize = 4, 4
	// base32 emits 8 characters per 5 bytes; 10 bytes gives exactly 16.
	buf := make([]byte, 10)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("idgen: read random bytes: %w", err)
	}
	encoded := giftCardEncoding.EncodeToString(buf)

	var sb strings.Builder
	sb.Grow(groups*groupSize + groups - 1)
	for i := 0; i < groups; i++ {
		if i > 0 {
			sb.WriteByte('-')
		}
		sb.WriteString(encoded[i*groupSize : (i+1)*groupSize])
	}
	return sb.String(), nil
}

// NormalizeCode canonicalises a user-supplied gift-card or discount code so that
// "7k2m 9xqf-3b4t vw8n" and "7K2M-9XQF-3B4T-VW8N" resolve to the same record.
// Lookups and uniqueness constraints always use the normalised form.
func NormalizeCode(code string) string {
	var sb strings.Builder
	sb.Grow(len(code))
	for _, r := range strings.ToUpper(code) {
		switch {
		case r >= '0' && r <= '9', r >= 'A' && r <= 'Z':
			sb.WriteRune(r)
		default:
			// Drop separators, spaces and anything else a user might paste.
		}
	}
	return sb.String()
}

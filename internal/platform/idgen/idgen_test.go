package idgen_test

import (
	"regexp"
	"testing"

	"github.com/MS-Arcadia/api-gateway/internal/platform/idgen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUUIDv7IsValidAndUnique(t *testing.T) {
	var gen idgen.UUIDv7
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := gen.NewID()
		require.True(t, idgen.IsValidUUID(id), "%q must parse as a UUID", id)
		_, dup := seen[id]
		require.False(t, dup, "generated a duplicate id %q", id)
		seen[id] = struct{}{}
	}
}

func TestUUIDv7IsTimeOrdered(t *testing.T) {
	var gen idgen.UUIDv7
	prev := gen.NewID()
	for i := 0; i < 50; i++ {
		next := gen.NewID()
		// v7 ids sort lexicographically in creation order.
		assert.LessOrEqual(t, prev, next)
		prev = next
	}
}

func TestSequenceIsDeterministic(t *testing.T) {
	gen := &idgen.Sequence{Prefix: "wallet"}
	assert.Equal(t, "wallet-1", gen.NewID())
	assert.Equal(t, "wallet-2", gen.NewID())

	unprefixed := &idgen.Sequence{}
	assert.Equal(t, "id-1", unprefixed.NewID())
}

func TestIsValidUUID(t *testing.T) {
	assert.True(t, idgen.IsValidUUID("0193b1b0-4b1e-7000-8000-000000000000"))
	assert.False(t, idgen.IsValidUUID("not-a-uuid"))
	assert.False(t, idgen.IsValidUUID(""))
}

func TestNewGiftCardCodeFormat(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}(-[0-9A-HJKMNP-TV-Z]{4}){3}$`)
	seen := make(map[string]struct{}, 500)
	for i := 0; i < 500; i++ {
		code, err := idgen.NewGiftCardCode()
		require.NoError(t, err)
		assert.Regexp(t, pattern, code)

		_, dup := seen[code]
		require.False(t, dup, "generated a duplicate gift card code %q", code)
		seen[code] = struct{}{}
	}
}

func TestGiftCardCodeAvoidsAmbiguousLetters(t *testing.T) {
	for i := 0; i < 200; i++ {
		code, err := idgen.NewGiftCardCode()
		require.NoError(t, err)
		assert.NotContains(t, code, "I")
		assert.NotContains(t, code, "L")
		assert.NotContains(t, code, "O")
		assert.NotContains(t, code, "U")
	}
}

func TestNormalizeCode(t *testing.T) {
	tests := map[string]string{
		"7K2M-9XQF-3B4T-VW8N": "7K2M9XQF3B4TVW8N",
		"7k2m 9xqf-3b4t vw8n": "7K2M9XQF3B4TVW8N",
		" 7k2m9xqf3b4tvw8n ":  "7K2M9XQF3B4TVW8N",
		"7K2M_9XQF.3B4T/VW8N": "7K2M9XQF3B4TVW8N",
		"":                    "",
		"---":                 "",
	}
	for input, want := range tests {
		assert.Equal(t, want, idgen.NormalizeCode(input), "input %q", input)
	}
}

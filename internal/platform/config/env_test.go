package config

import "testing"

// TestOneOfReturnsTheCanonicalSpelling covers the boot failure this function caused: it
// matched case-insensitively but returned the caller's spelling, so JWT_ALGORITHM=HS256
// came back as "hs256" and the token verifier rejected it as unsupported.
func TestOneOfReturnsTheCanonicalSpelling(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		allowed []string
		want    string
	}{
		{"exact match", "HS256", []string{"HS256", "RS256"}, "HS256"},
		{"lowercase input is accepted and canonicalised", "hs256", []string{"HS256", "RS256"}, "HS256"},
		{"mixed case too", "Rs256", []string{"HS256", "RS256"}, "RS256"},
		{"an all-lowercase enum is unaffected", "grpc", []string{"grpc", "http", "both"}, "grpc"},
		{"and is still case-insensitive", "BOTH", []string{"grpc", "http", "both"}, "both"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_ONE_OF", tc.env)
			l := NewLoader()
			got := l.OneOf("TEST_ONE_OF", tc.allowed[0], tc.allowed...)
			if err := l.Err(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("OneOf(%q) = %q, want %q", tc.env, got, tc.want)
			}
		})
	}
}

func TestOneOfUsesTheDefaultWhenUnset(t *testing.T) {
	l := NewLoader()
	if got := l.OneOf("TEST_ONE_OF_ABSENT", "HS256", "HS256", "RS256"); got != "HS256" {
		t.Fatalf("got %q", got)
	}
	if err := l.Err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOneOfRejectsAValueOutsideTheList(t *testing.T) {
	t.Setenv("TEST_ONE_OF", "ES256")
	l := NewLoader()
	l.OneOf("TEST_ONE_OF", "HS256", "HS256", "RS256")
	if l.Err() == nil {
		t.Fatal("expected an error for an unsupported algorithm")
	}
}

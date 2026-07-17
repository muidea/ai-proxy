package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeterministicEventIDStable(t *testing.T) {
	a := deterministicEventID("src", 2, []string{"a", "b"})
	b := deterministicEventID("src", 2, []string{"a", "b"})
	c := deterministicEventID("src", 3, []string{"a", "b"})
	if a != b || a == c {
		t.Fatalf("ids a=%s b=%s c=%s", a, b, c)
	}
	if _, err := os.Stat(filepath.Join(t.TempDir())); err != nil {
		t.Fatal(err)
	}
}

func TestStrictImportFieldParsing(t *testing.T) {
	if _, err := parseNonNegativeInt64("input_tokens", "not-a-number"); err == nil {
		t.Fatal("invalid token value must be rejected")
	}
	if _, err := parseHTTPStatus("99"); err == nil {
		t.Fatal("invalid HTTP status must be rejected")
	}
	if _, err := parseOptionalBool("stream", "sometimes"); err == nil {
		t.Fatal("invalid bool must be rejected")
	}
	if !apiKeyIDPattern.MatchString("team_a-1.prod") || apiKeyIDPattern.MatchString("-bad") || apiKeyIDPattern.MatchString("A") {
		t.Fatal("api key ID validation does not match configuration contract")
	}
}

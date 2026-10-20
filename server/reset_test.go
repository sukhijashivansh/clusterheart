package main

import "testing"

func TestResetTokenRoundTrip(t *testing.T) {
	a := &app{secret: []byte("test-secret-key-0123456789")}
	hash1 := "$2a$10$DummyBcryptHashValueForTestingXXXXXXXXXXXXXXXXXXXXXXXX"

	tok := a.mintReset("User@Example.com", hash1)
	email, fp, ok := a.parseReset(tok)
	if !ok {
		t.Fatal("valid token was rejected")
	}
	if email != "user@example.com" {
		t.Fatalf("email normalised wrong: %q", email)
	}
	if fp != resetFingerprint(hash1) {
		t.Fatal("fingerprint does not match the minting password hash")
	}

	// Single-use: once the password (hash) changes, the fingerprint differs, so
	// handleReset will reject a reused token.
	if resetFingerprint("a-different-hash") == fp {
		t.Fatal("fingerprint must change when the password hash changes")
	}

	// Tampered signature / wrong secret must not verify.
	b := &app{secret: []byte("a-completely-different-secret")}
	if _, _, ok := b.parseReset(tok); ok {
		t.Fatal("token verified under the wrong secret")
	}
	if _, _, ok := a.parseReset("not-a-valid-token"); ok {
		t.Fatal("garbage token was accepted")
	}
	if _, _, ok := a.parseReset(tok + "x"); ok {
		t.Fatal("token with altered MAC was accepted")
	}
}

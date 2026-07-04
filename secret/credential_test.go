package secret

import (
	"strings"
	"testing"
)

// allAlgos is the set of HashSpecs the round-trip tests exercise.
func allAlgos() []HashSpec {
	return []HashSpec{
		{Algo: AlgoSHA512_256},
		{Algo: AlgoSHA384},
		{Algo: "pbkdf2-sha256:1000:32"}, // small iters keeps the test fast
	}
}

func TestMintParseVerifyRoundTrip(t *testing.T) {
	for _, spec := range allAlgos() {
		spec := spec
		t.Run(spec.Algo, func(t *testing.T) {
			m := &CredentialMinter{Prefix: "ib", Spec: spec}
			token, stored, err := m.Mint()
			if err != nil {
				t.Fatalf("Mint: %v", err)
			}
			if stored.Spec.Algo != spec.Algo {
				t.Fatalf("stored spec = %q, want %q", stored.Spec.Algo, spec.Algo)
			}
			if stored.PublicID == "" || stored.Salt == "" || stored.Hash == "" {
				t.Fatalf("stored has empty field: %+v", stored)
			}

			prefix, publicID, secret, err := Parse(token)
			if err != nil {
				t.Fatalf("Parse(%q): %v", token, err)
			}
			if prefix != "ib" {
				t.Fatalf("prefix = %q, want ib", prefix)
			}
			if publicID != stored.PublicID {
				t.Fatalf("parsed public id %q != stored %q", publicID, stored.PublicID)
			}

			ok, err := Verify(secret, stored)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if !ok {
				t.Fatalf("Verify returned false for the correct secret")
			}
		})
	}
}

func TestVerifyWrongSecretFails(t *testing.T) {
	for _, spec := range allAlgos() {
		spec := spec
		t.Run(spec.Algo, func(t *testing.T) {
			m := &CredentialMinter{Spec: spec}
			_, stored, err := m.Mint()
			if err != nil {
				t.Fatalf("Mint: %v", err)
			}
			ok, err := Verify("not-the-secret", stored)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if ok {
				t.Fatalf("Verify accepted a wrong secret")
			}
		})
	}
}

func TestVerifyTamperedSaltOrHashFails(t *testing.T) {
	m := &CredentialMinter{}
	token, stored, err := m.Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	_, _, secret, err := Parse(token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Tampered salt: recomputed hash no longer matches → not ok (same length hash,
	// so the constant-time compare runs to completion and returns 0).
	badSalt := stored
	badSalt.Salt = flipBase64Char(t, stored.Salt)
	ok, err := Verify(secret, badSalt)
	if err != nil {
		t.Fatalf("Verify (bad salt): %v", err)
	}
	if ok {
		t.Fatalf("Verify accepted a tampered salt")
	}

	// Tampered hash: byte differs → not ok.
	badHash := stored
	badHash.Hash = flipBase64Char(t, stored.Hash)
	ok, err = Verify(secret, badHash)
	if err != nil {
		t.Fatalf("Verify (bad hash): %v", err)
	}
	if ok {
		t.Fatalf("Verify accepted a tampered hash")
	}
}

// flipBase64Char changes one character of a base64 string to a different valid
// base64 character, keeping the decoded length compatible so the constant-time
// compare path (equal-length inputs) is what rejects the value.
func flipBase64Char(t *testing.T, s string) string {
	t.Helper()
	if s == "" {
		t.Fatalf("cannot flip an empty string")
	}
	b := []byte(s)
	// Flip the first character to a guaranteed-different base64 letter.
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}

func TestPublicIDUniqueAcrossManyMints(t *testing.T) {
	m := &CredentialMinter{}
	seen := make(map[string]struct{}, 4096)
	const n = 4096
	for i := 0; i < n; i++ {
		_, stored, err := m.Mint()
		if err != nil {
			t.Fatalf("Mint #%d: %v", i, err)
		}
		if _, dup := seen[stored.PublicID]; dup {
			t.Fatalf("duplicate public id after %d mints: %q", i, stored.PublicID)
		}
		seen[stored.PublicID] = struct{}{}
	}
}

func TestTokenFormatAndBase62Charset(t *testing.T) {
	m := &CredentialMinter{Prefix: "ib"}
	token, stored, err := m.Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.HasPrefix(token, "ib_") {
		t.Fatalf("token %q lacks the ib_ prefix", token)
	}
	_, publicID, secret, err := Parse(token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !isBase62(publicID) {
		t.Fatalf("public id %q is not base62", publicID)
	}
	if !isBase62(secret) {
		t.Fatalf("secret %q is not base62", secret)
	}
	// The default secret is >=256 bits => >=43 base62 chars.
	if got := len(secret); got < charsForBits(DefaultSecretBits) {
		t.Fatalf("secret length %d < %d chars for %d bits", got, charsForBits(DefaultSecretBits), DefaultSecretBits)
	}
	// The public id carries >=128 bits => >=22 base62 chars.
	if got := len(stored.PublicID); got < charsForBits(publicIDBits) {
		t.Fatalf("public id length %d < %d chars for %d bits", got, charsForBits(publicIDBits), publicIDBits)
	}
}

func TestDefaultPrefixApplied(t *testing.T) {
	m := &CredentialMinter{} // empty Prefix
	token, _, err := m.Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	prefix, _, _, err := Parse(token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if prefix != DefaultPrefix {
		t.Fatalf("prefix = %q, want DefaultPrefix %q", prefix, DefaultPrefix)
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"noseparators",
		"ib_onlytwo",
		"ib__emptysecret",   // empty middle/secret segments
		"_pub_secret",       // empty prefix
		"ib_pub_",           // empty secret
		"ib_pub_secret!bad", // non-base62 secret
	}
	for _, c := range cases {
		if _, _, _, err := Parse(c); err == nil {
			t.Fatalf("Parse(%q) = nil error, want error", c)
		}
	}
}

func TestParseHandlesMultiUnderscorePrefix(t *testing.T) {
	m := &CredentialMinter{Prefix: "ib_live"}
	token, stored, err := m.Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	prefix, publicID, secret, err := Parse(token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if prefix != "ib_live" {
		t.Fatalf("prefix = %q, want ib_live", prefix)
	}
	if publicID != stored.PublicID {
		t.Fatalf("public id mismatch: %q vs %q", publicID, stored.PublicID)
	}
	ok, err := Verify(secret, stored)
	if err != nil || !ok {
		t.Fatalf("Verify: ok=%v err=%v", ok, err)
	}
}

func TestVerifyUnsupportedAlgoErrors(t *testing.T) {
	_, err := Verify("x", StoredCredential{Salt: "AAAA", Hash: "AAAA", Spec: HashSpec{Algo: "md5"}})
	if err == nil {
		t.Fatalf("Verify with unsupported algo returned nil error")
	}
}

package secret_test

import (
	"context"
	"testing"

	"github.com/infobloxopen/devedge-sdk/secret"
)

func testKey() []byte {
	key := make([]byte, 32)
	key[0] = 1
	return key
}

// TestDev_EncryptDecrypt_Roundtrip verifies that decrypting an encrypted value
// returns the original plaintext.
func TestDev_EncryptDecrypt_Roundtrip(t *testing.T) {
	ctx := context.Background()
	enc := secret.NewDev(testKey())

	plaintext := "hello world"
	ciphertext, err := enc.Encrypt(ctx, plaintext)
	if err != nil {
		t.Fatalf("Encrypt error: %v", err)
	}

	got, err := enc.Decrypt(ctx, ciphertext)
	if err != nil {
		t.Fatalf("Decrypt error: %v", err)
	}

	if got != plaintext {
		t.Errorf("roundtrip failed: got %q, want %q", got, plaintext)
	}
}

// TestDev_Hash_IsStable verifies that hashing the same input twice yields the
// same non-empty result.
func TestDev_Hash_IsStable(t *testing.T) {
	ctx := context.Background()
	enc := secret.NewDev(testKey())

	h1, err := enc.Hash(ctx, "abc")
	if err != nil {
		t.Fatalf("Hash (first call) error: %v", err)
	}
	h2, err := enc.Hash(ctx, "abc")
	if err != nil {
		t.Fatalf("Hash (second call) error: %v", err)
	}

	if h1 == "" {
		t.Error("Hash returned empty string")
	}
	if h1 != h2 {
		t.Errorf("Hash is not stable: first=%q second=%q", h1, h2)
	}
}

// TestDev_Hash_DiffersAcrossKeys verifies that hashing the same plaintext with
// different keys produces different results (HMAC-like keyed hash).
func TestDev_Hash_DiffersAcrossKeys(t *testing.T) {
	ctx := context.Background()

	key1 := make([]byte, 32)
	key1[0] = 1
	key2 := make([]byte, 32)
	key2[0] = 2

	enc1 := secret.NewDev(key1)
	enc2 := secret.NewDev(key2)

	h1, err := enc1.Hash(ctx, "abc")
	if err != nil {
		t.Fatalf("enc1.Hash error: %v", err)
	}
	h2, err := enc2.Hash(ctx, "abc")
	if err != nil {
		t.Fatalf("enc2.Hash error: %v", err)
	}

	if h1 == h2 {
		t.Errorf("expected different hashes for different keys, both got %q", h1)
	}
}

// TestDev_Encrypt_ProducesDifferentCiphertexts verifies that encrypting the same
// plaintext twice yields different ciphertexts (random nonce per call).
func TestDev_Encrypt_ProducesDifferentCiphertexts(t *testing.T) {
	ctx := context.Background()
	enc := secret.NewDev(testKey())

	c1, err := enc.Encrypt(ctx, "abc")
	if err != nil {
		t.Fatalf("Encrypt (first call) error: %v", err)
	}
	c2, err := enc.Encrypt(ctx, "abc")
	if err != nil {
		t.Fatalf("Encrypt (second call) error: %v", err)
	}

	if c1 == c2 {
		t.Errorf("expected different ciphertexts per call (random nonce), but both were %q", c1)
	}
}

// TestNewDev_ShortKey_Panics verifies that NewDev panics when given a key
// shorter than 32 bytes (AES-256 requires exactly 32 bytes).
func TestNewDev_ShortKey_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected NewDev to panic with short key, but it did not")
		}
	}()
	secret.NewDev(make([]byte, 16))
}

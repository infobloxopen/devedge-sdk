package secret_test

import (
	"context"
	"os"
	"testing"

	"github.com/infobloxopen/devedge-sdk/secret"
)

func vaultAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("VAULT_ADDR")
	if addr == "" {
		t.Skip("VAULT_ADDR not set; skipping Vault integration tests")
	}
	return addr
}

func TestVaultTransit_EncryptDecrypt(t *testing.T) {
	addr := vaultAddr(t)
	token := os.Getenv("VAULT_TOKEN")
	enc := secret.NewVaultTransit(addr, token, "test-key")
	ct, err := enc.Encrypt(context.Background(), "hello vault")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if ct == "" {
		t.Fatal("expected non-empty ciphertext")
	}
	plain, err := enc.Decrypt(context.Background(), ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if plain != "hello vault" {
		t.Fatalf("want 'hello vault', got %q", plain)
	}
}

func TestVaultTransit_Hash_IsStable(t *testing.T) {
	addr := vaultAddr(t)
	enc := secret.NewVaultTransit(addr, os.Getenv("VAULT_TOKEN"), "test-key")
	h1, _ := enc.Hash(context.Background(), "abc")
	h2, _ := enc.Hash(context.Background(), "abc")
	if h1 != h2 {
		t.Fatal("Hash not stable")
	}
	if h1 == "" {
		t.Fatal("Hash empty")
	}
}

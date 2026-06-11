package secret

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
)

// Encryptor provides encrypt, decrypt, and hash operations for secret fields.
type Encryptor interface {
	Encrypt(ctx context.Context, plaintext string) (ciphertext string, err error)
	Decrypt(ctx context.Context, ciphertext string) (plaintext string, err error)
	Hash(ctx context.Context, plaintext string) (hash string, err error)
}

// NewDev returns a dev-suitable Encryptor using AES-256-GCM (encrypt/decrypt)
// and HMAC-SHA256 (hash). Panics if len(key) < 32.
func NewDev(key []byte) Encryptor {
	if len(key) < 32 {
		panic(fmt.Sprintf("secret.NewDev: key must be at least 32 bytes, got %d", len(key)))
	}
	k := make([]byte, 32)
	copy(k, key)
	return &devEncryptor{key: k}
}

type devEncryptor struct{ key []byte }

func (d *devEncryptor) Encrypt(_ context.Context, plaintext string) (string, error) {
	block, err := aes.NewCipher(d.key)
	if err != nil {
		return "", fmt.Errorf("secret: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("secret: new gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("secret: rand nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (d *devEncryptor) Decrypt(_ context.Context, ciphertext string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("secret: base64 decode: %w", err)
	}
	block, err := aes.NewCipher(d.key)
	if err != nil {
		return "", fmt.Errorf("secret: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("secret: new gcm: %w", err)
	}
	ns := gcm.NonceSize()
	if len(data) < ns {
		return "", fmt.Errorf("secret: ciphertext too short")
	}
	nonce, ct := data[:ns], data[ns:]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("secret: decrypt: %w", err)
	}
	return string(plain), nil
}

func (d *devEncryptor) Hash(_ context.Context, plaintext string) (string, error) {
	mac := hmac.New(sha256.New, d.key)
	mac.Write([]byte(plaintext))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

package secret

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// VaultTransitEncryptor is an Encryptor backed by HashiCorp Vault's Transit
// Secrets Engine. Uses plain HTTP — no Vault SDK dependency.
type VaultTransitEncryptor struct {
	addr    string
	token   string
	keyName string
	client  *http.Client
	hmacKey []byte // sha256(token) — stable local HMAC key
}

// NewVaultTransit returns an Encryptor backed by Vault Transit.
// addr: Vault server address (e.g. "http://localhost:8200")
// token: Vault token with encrypt/decrypt policy on keyName
// keyName: Transit key name (must already exist in Vault)
func NewVaultTransit(addr, token, keyName string) *VaultTransitEncryptor {
	sum := sha256.Sum256([]byte(token))
	return &VaultTransitEncryptor{
		addr:    addr,
		token:   token,
		keyName: keyName,
		client:  &http.Client{},
		hmacKey: sum[:],
	}
}

func (v *VaultTransitEncryptor) Encrypt(ctx context.Context, plaintext string) (string, error) {
	body := map[string]string{
		"plaintext": base64.StdEncoding.EncodeToString([]byte(plaintext)),
	}
	var result struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	if err := v.post(ctx, fmt.Sprintf("/v1/transit/encrypt/%s", v.keyName), body, &result); err != nil {
		return "", err
	}
	return result.Data.Ciphertext, nil
}

func (v *VaultTransitEncryptor) Decrypt(ctx context.Context, ciphertext string) (string, error) {
	body := map[string]string{"ciphertext": ciphertext}
	var result struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := v.post(ctx, fmt.Sprintf("/v1/transit/decrypt/%s", v.keyName), body, &result); err != nil {
		return "", err
	}
	plain, err := base64.StdEncoding.DecodeString(result.Data.Plaintext)
	if err != nil {
		return "", fmt.Errorf("vault: decode plaintext: %w", err)
	}
	return string(plain), nil
}

func (v *VaultTransitEncryptor) Hash(_ context.Context, plaintext string) (string, error) {
	mac := hmac.New(sha256.New, v.hmacKey)
	mac.Write([]byte(plaintext))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// Rewrap re-encrypts ciphertext under the latest key version without revealing plaintext.
func (v *VaultTransitEncryptor) Rewrap(ctx context.Context, ciphertext string) (string, error) {
	body := map[string]string{"ciphertext": ciphertext}
	var result struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	if err := v.post(ctx, fmt.Sprintf("/v1/transit/rewrap/%s", v.keyName), body, &result); err != nil {
		return "", err
	}
	return result.Data.Ciphertext, nil
}

func (v *VaultTransitEncryptor) post(ctx context.Context, path string, body, result any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("vault: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.addr+path, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("vault: new request: %w", err)
	}
	req.Header.Set("X-Vault-Token", v.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("vault: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("vault: status %d: %s", resp.StatusCode, string(body))
	}
	return json.NewDecoder(resp.Body).Decode(result)
}

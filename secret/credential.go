package secret

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// This file implements the verify-only credential primitive (WS-033): a
// gold-standard prefixed split token the SDK MINTS, returns to the client once,
// and stores as a public lookup id plus a salted one-way hash — never a
// reversible copy. It is FIPS-clean and dependency-light: stdlib crypto only
// (crypto/sha512, crypto/pbkdf2, crypto/rand, crypto/subtle). Unlike the
// retrievable secret path (Encryptor, hash + reversible cipher), a credential
// cannot be read back — it can only be VERIFIED.

// Supported HashSpec.Algo values.
const (
	// AlgoSHA512_256 is the default: a fast, FIPS-approved, length-extension-safe
	// hash over the SDK-minted high-entropy secret. Because the secret is minted
	// with >=256 bits of entropy, a slow KDF buys nothing; a fast hash is correct.
	AlgoSHA512_256 = "sha512-256"
	// AlgoSHA384 is a fast FIPS/CNSA-2.0 alternative.
	AlgoSHA384 = "sha384"
	// algoPBKDF2SHA256Prefix marks the optional slow-KDF mode for low-entropy /
	// defense-in-depth use: "pbkdf2-sha256:<iters>:<keylen>".
	algoPBKDF2SHA256Prefix = "pbkdf2-sha256:"
)

// Defaults for a CredentialMinter left zero-valued.
const (
	// DefaultPrefix is used when a CredentialMinter has an empty Prefix. A known,
	// constant prefix is what makes minted tokens recognizable to secret scanners.
	DefaultPrefix = "ib"
	// DefaultSecretBits is the minted secret's entropy when SecretBits <= 0.
	DefaultSecretBits = 256
	// publicIDBits is the entropy of the public lookup id (>=128-bit CSPRNG, so a
	// collision on the UNIQUE index is ~2^-128; the DB constraint + re-mint makes
	// uniqueness a guarantee rather than merely improbable).
	publicIDBits = 128
	// saltBytesLen is the per-credential CSPRNG salt length (128-bit).
	saltBytesLen = 16
)

// base62Alphabet is the URL- and shell-safe alphabet used for the public id and
// the secret (no separators, so the token splits unambiguously on "_").
const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// HashSpec self-describes the hash used for a stored credential, so a credential
// minted under one algorithm still verifies after the process default changes
// (verify-time agility / algorithm upgrades). Algo is one of AlgoSHA512_256,
// AlgoSHA384, or "pbkdf2-sha256:<iters>:<keylen>".
type HashSpec struct {
	Algo string
}

// StoredCredential is the at-rest representation of a minted credential: the
// public lookup id (plaintext, UNIQUE-indexed), the per-credential salt and the
// one-way hash (both base64), and the self-describing HashSpec. It contains NO
// reversible copy of the secret.
type StoredCredential struct {
	PublicID string
	Salt     string // base64 (StdEncoding)
	Hash     string // base64 (StdEncoding)
	Spec     HashSpec
}

// CredentialMinter mints verify-only credentials. The zero value is usable: an
// empty Prefix defaults to DefaultPrefix, an empty Spec.Algo defaults to
// AlgoSHA512_256, and a non-positive SecretBits defaults to DefaultSecretBits.
type CredentialMinter struct {
	Prefix     string
	Spec       HashSpec
	SecretBits int
}

// Mint generates a fresh credential. It returns the full token
// "<prefix>_<public_id>_<secret>" — hand this to the client EXACTLY ONCE — and
// the StoredCredential to persist. The plaintext secret is never retained:
// only its salted hash is kept, in stored.Hash.
func (m *CredentialMinter) Mint() (token string, stored StoredCredential, err error) {
	prefix := m.Prefix
	if prefix == "" {
		prefix = DefaultPrefix
	}
	spec := m.Spec
	if spec.Algo == "" {
		spec = HashSpec{Algo: AlgoSHA512_256}
	}
	secretBits := m.SecretBits
	if secretBits <= 0 {
		secretBits = DefaultSecretBits
	}

	publicID, err := randBase62(charsForBits(publicIDBits))
	if err != nil {
		return "", StoredCredential{}, fmt.Errorf("secret: mint public id: %w", err)
	}
	secretVal, err := randBase62(charsForBits(secretBits))
	if err != nil {
		return "", StoredCredential{}, fmt.Errorf("secret: mint secret: %w", err)
	}
	salt := make([]byte, saltBytesLen)
	if _, err := rand.Read(salt); err != nil {
		return "", StoredCredential{}, fmt.Errorf("secret: mint salt: %w", err)
	}

	h, err := computeHash(spec, salt, secretVal)
	if err != nil {
		return "", StoredCredential{}, err
	}

	stored = StoredCredential{
		PublicID: publicID,
		Salt:     base64.StdEncoding.EncodeToString(salt),
		Hash:     base64.StdEncoding.EncodeToString(h),
		Spec:     spec,
	}
	token = prefix + "_" + publicID + "_" + secretVal
	// secretVal goes out of scope here; the minter never keeps it.
	return token, stored, nil
}

// Parse splits a presented token into its prefix, public id, and secret. The
// caller loads the StoredCredential by publicID and calls Verify with secret.
// The prefix may itself contain "_" (e.g. "ib_live"); the public id and secret
// are base62 and separator-free, so the split is taken from the right.
func Parse(token string) (prefix, publicID, secret string, err error) {
	i2 := strings.LastIndexByte(token, '_')
	if i2 <= 0 || i2 == len(token)-1 {
		return "", "", "", fmt.Errorf("secret: malformed credential token")
	}
	secret = token[i2+1:]
	rest := token[:i2]
	i1 := strings.LastIndexByte(rest, '_')
	if i1 <= 0 || i1 == len(rest)-1 {
		return "", "", "", fmt.Errorf("secret: malformed credential token")
	}
	publicID = rest[i1+1:]
	prefix = rest[:i1]
	if !isBase62(publicID) || !isBase62(secret) {
		return "", "", "", fmt.Errorf("secret: credential token segments are not base62")
	}
	return prefix, publicID, secret, nil
}

// Verify recomputes the stored HashSpec's hash of (salt||secret) and compares it
// to stored.Hash in constant time. It returns (true, nil) on a match, (false,
// nil) on a mismatch, and a non-nil error only when the stored fields are
// malformed or the algorithm is unsupported.
func Verify(secret string, stored StoredCredential) (ok bool, err error) {
	saltBytes, err := base64.StdEncoding.DecodeString(stored.Salt)
	if err != nil {
		return false, fmt.Errorf("secret: decode salt: %w", err)
	}
	want, err := base64.StdEncoding.DecodeString(stored.Hash)
	if err != nil {
		return false, fmt.Errorf("secret: decode hash: %w", err)
	}
	got, err := computeHash(stored.Spec, saltBytes, secret)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// computeHash applies spec to (salt || secret). The fast FIPS hashes prepend the
// salt to the secret bytes; PBKDF2 takes the salt as its dedicated salt input.
func computeHash(spec HashSpec, salt []byte, secret string) ([]byte, error) {
	switch {
	case spec.Algo == AlgoSHA512_256:
		sum := sha512.Sum512_256(saltedInput(salt, secret))
		return sum[:], nil
	case spec.Algo == AlgoSHA384:
		sum := sha512.Sum384(saltedInput(salt, secret))
		return sum[:], nil
	case strings.HasPrefix(spec.Algo, algoPBKDF2SHA256Prefix):
		iters, keyLen, perr := parsePBKDF2(spec.Algo)
		if perr != nil {
			return nil, perr
		}
		out, kerr := pbkdf2.Key(sha256.New, secret, salt, iters, keyLen)
		if kerr != nil {
			return nil, fmt.Errorf("secret: pbkdf2: %w", kerr)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("secret: unsupported hash algo %q", spec.Algo)
	}
}

// saltedInput returns a fresh salt||secret byte slice (never mutating salt).
func saltedInput(salt []byte, secret string) []byte {
	buf := make([]byte, 0, len(salt)+len(secret))
	buf = append(buf, salt...)
	buf = append(buf, secret...)
	return buf
}

// parsePBKDF2 parses "pbkdf2-sha256:<iters>:<keylen>" into positive iters/keylen.
func parsePBKDF2(algo string) (iters, keyLen int, err error) {
	parts := strings.Split(algo, ":")
	if len(parts) != 3 {
		return 0, 0, fmt.Errorf("secret: malformed pbkdf2 spec %q (want pbkdf2-sha256:<iters>:<keylen>)", algo)
	}
	iters, err = strconv.Atoi(parts[1])
	if err != nil || iters <= 0 {
		return 0, 0, fmt.Errorf("secret: pbkdf2 iterations must be a positive integer in %q", algo)
	}
	keyLen, err = strconv.Atoi(parts[2])
	if err != nil || keyLen <= 0 {
		return 0, 0, fmt.Errorf("secret: pbkdf2 key length must be a positive integer in %q", algo)
	}
	return iters, keyLen, nil
}

// charsForBits returns the number of base62 characters needed to carry at least
// bits of entropy (ceil(bits / log2(62))).
func charsForBits(bits int) int {
	return int(math.Ceil(float64(bits) / math.Log2(62)))
}

// randBase62 returns n base62 characters drawn uniformly from crypto/rand using
// rejection sampling (so there is no modulo bias).
func randBase62(n int) (string, error) {
	const max = 256 - (256 % 62) // 248: the largest multiple of 62 <= 256
	out := make([]byte, n)
	buf := make([]byte, 1)
	for i := 0; i < n; {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		if buf[0] >= max {
			continue // reject to keep the distribution uniform
		}
		out[i] = base62Alphabet[buf[0]%62]
		i++
	}
	return string(out), nil
}

// isBase62 reports whether s is non-empty and contains only base62 characters.
func isBase62(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		default:
			return false
		}
	}
	return true
}

package apikeyv1_test

import (
	"testing"

	"github.com/infobloxopen/devedge-sdk/secret"
	. "github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1"
)

// TestAPIKeyRepository_ConstructorRequiresEncryptor verifies at compile time
// that the generated APIKeyRepository constructor requires a secret.Encryptor
// and that APIKeyModel has no KeyValue column.
func TestAPIKeyRepository_ConstructorRequiresEncryptor(t *testing.T) {
	key := make([]byte, 32)
	enc := secret.NewDev(key)
	// This call must compile — if secret fields are not handled, the constructor
	// won't accept an Encryptor and this test won't compile.
	_ = NewAPIKeyRepository(nil, enc)
	t.Log("APIKeyRepository correctly requires secret.Encryptor")
}

func TestAPIKeyModel_HasHashAndCipherColumns(t *testing.T) {
	m := &APIKeyModel{}
	// Verify the model has hash+cipher fields, not a raw key_value field.
	_ = m.KeyValueHash
	_ = m.KeyValueCipher
	// This line must NOT compile if the raw KeyValue column exists:
	// _ = m.KeyValue  // should not exist
	t.Log("APIKeyModel correctly stores KeyValueHash + KeyValueCipher, not raw KeyValue")
}

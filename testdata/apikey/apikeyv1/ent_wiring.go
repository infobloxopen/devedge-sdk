// Hand-written ent wiring for APIKey — wires the generated ent client into
// the persistence.Repository[*APIKey, string] seam.
package apikeyv1

import (
	"context"
	"fmt"

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/entrepo"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/ent"
	entapikey "github.com/infobloxopen/devedge-sdk/testdata/apikey/ent/apikey"
)

// NewAPIKeyEntRepository wires the ent client into a persistence.Repository.
// enc may be nil if no secret fields need encryption (dev mode uses NewDev).
func NewAPIKeyEntRepository(client *ent.Client, enc secret.Encryptor) persistence.Repository[*APIKey, string] {
	return &entrepo.EntRepository[*APIKey, string]{
		Enc: enc,
		Create_: func(ctx context.Context, entity *APIKey) (*APIKey, error) {
			// Set tenant from context if not already set on the entity.
			tenantID := middleware.TenantIDFromContext(ctx)
			if entity.AccountId == "" && tenantID != "" {
				entity.AccountId = tenantID
			}
			b := client.APIKey.Create().
				SetID(entity.Id).
				SetName(entity.Name).
				SetAccountID(entity.AccountId).
				SetKeyPrefix(entity.KeyPrefix)
			if enc != nil && entity.KeyValue != "" {
				h, err := enc.Hash(ctx, entity.KeyValue)
				if err != nil {
					return nil, fmt.Errorf("hash key_value: %w", err)
				}
				c, err := enc.Encrypt(ctx, entity.KeyValue)
				if err != nil {
					return nil, fmt.Errorf("encrypt key_value: %w", err)
				}
				b = b.SetKeyValueHash(h).SetKeyValueCipher(c)
				entity.KeyValue = "" // clear plaintext before returning
			}
			created, err := b.Save(ctx)
			if err != nil {
				return nil, fmt.Errorf("create apikey: %w", err)
			}
			return fromEntAPIKey(created), nil
		},
		Get_: func(ctx context.Context, key string) (*APIKey, error) {
			// TenantMixin interceptor on the ent client automatically scopes this
			// query to the tenant in ctx — if bob queries alice's id he gets not-found.
			e, err := client.APIKey.Get(ctx, key)
			if err != nil {
				if ent.IsNotFound(err) {
					return nil, persistence.ErrNotFound
				}
				return nil, err
			}
			return fromEntAPIKey(e), nil
		},
		List_: func(ctx context.Context, opts persistence.ListOptions) ([]*APIKey, string, error) {
			// TenantMixin interceptor scopes the query to the tenant in ctx automatically.
			q := client.APIKey.Query()
			if opts.PageSize <= 0 {
				opts.PageSize = 50
			}
			offset := 0
			if opts.PageToken != "" {
				fmt.Sscanf(opts.PageToken, "%d", &offset) //nolint:errcheck
			}
			items, err := q.Limit(opts.PageSize).Offset(offset).All(ctx)
			if err != nil {
				return nil, "", err
			}
			out := make([]*APIKey, len(items))
			for i, e := range items {
				out[i] = fromEntAPIKey(e)
			}
			nextToken := ""
			if len(items) == opts.PageSize {
				nextToken = fmt.Sprintf("%d", offset+opts.PageSize)
			}
			return out, nextToken, nil
		},
		Update_: func(ctx context.Context, key string, entity *APIKey, fieldMask ...string) (*APIKey, error) {
			u := client.APIKey.UpdateOneID(key).
				SetName(entity.Name).
				SetKeyPrefix(entity.KeyPrefix)
			updated, err := u.Save(ctx)
			if err != nil {
				if ent.IsNotFound(err) {
					return nil, persistence.ErrNotFound
				}
				return nil, err
			}
			return fromEntAPIKey(updated), nil
		},
		Delete_: func(ctx context.Context, key string) error {
			err := client.APIKey.DeleteOneID(key).Exec(ctx)
			if ent.IsNotFound(err) {
				return persistence.ErrNotFound
			}
			return err
		},
	}
}

// fromEntAPIKey converts a generated ent.APIKey to the proto *APIKey.
// Secret fields (key_value) are intentionally omitted — they are never returned
// from storage after creation.
func fromEntAPIKey(e *ent.APIKey) *APIKey {
	if e == nil {
		return nil
	}
	return &APIKey{
		Id:        e.ID,
		Name:      e.Name,
		AccountId: e.AccountID,
		KeyPrefix: e.KeyPrefix,
		// KeyValue intentionally omitted — never returned from storage
	}
}

// LookupByKeyValueHash finds an APIKey by the HMAC-SHA256 hash of its key_value.
// Returns persistence.ErrNotFound when no record matches or hash is empty.
// The lookup is automatically tenant-scoped via the TenantMixin interceptor.
func LookupByKeyValueHash(ctx context.Context, client *ent.Client, hash string) (*APIKey, error) {
	if hash == "" {
		return nil, persistence.ErrNotFound
	}
	e, err := client.APIKey.Query().Where(entapikey.KeyValueHash(hash)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, persistence.ErrNotFound
		}
		return nil, err
	}
	return fromEntAPIKey(e), nil
}

// Hand-written ent wiring for APIKey — wires the generated ent client into
// the persistence.Repository[*APIKey, string] seam.
package apikeyv1

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/entrepo"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/ent"
	entapikey "github.com/infobloxopen/devedge-sdk/testdata/apikey/ent/apikey"
	entpredicate "github.com/infobloxopen/devedge-sdk/testdata/apikey/ent/predicate"
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
				SetKeyPrefix(entity.KeyPrefix).
				SetTags(entity.Tags)
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
				// Classify driver errors so a unique/FK/not-null violation becomes a
				// clean ErrConflict/ErrPreconditionFailed (409/412) with no raw SQL.
				if ce := persistence.ConstraintError(err); ce != nil {
					return nil, ce
				}
				return nil, fmt.Errorf("create apikey: %w", err)
			}
			return fromEntAPIKey(created), nil
		},
		Get_: func(ctx context.Context, key string) (*APIKey, error) {
			// TenantMixin + SoftDeleteMixin interceptors scope and filter automatically.
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
			// Route show_deleted flag into the context so SoftDeleteMixin interceptor
			// lifts the delete_time IS NULL predicate when requested.
			if opts.ShowDeleted {
				ctx = entrepo.WithShowDeleted(ctx)
			}
			q := client.APIKey.Query()
			// AIP-160 list filter, including tag (map) predicates such as
			// `tags.env = "prod"` and `has(tags.team)`. FilterPredicate renders
			// dialect-correct JSON SQL via ent's sqljson at query time.
			if opts.Filter != "" {
				pred, err := entrepo.FilterPredicate(opts.Filter, APIKeyColumns, APIKeyJSONColumns)
				if err != nil {
					return nil, "", err
				}
				if pred != nil {
					q = q.Where(entpredicate.APIKey(pred))
				}
			}
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
				SetKeyPrefix(entity.KeyPrefix).
				SetTags(entity.Tags)
			// Tenant guard: ent query interceptors do NOT run for mutations, so the
			// account_id predicate must be applied explicitly or a caller could
			// update another tenant's row by ID. Empty tenant = unscoped (matches
			// the TenantMixin query convention).
			if tenantID := middleware.TenantIDFromContext(ctx); tenantID != "" {
				u = u.Where(entapikey.AccountID(tenantID))
			}
			updated, err := u.Save(ctx)
			if err != nil {
				if ent.IsNotFound(err) {
					return nil, persistence.ErrNotFound
				}
				return nil, err
			}
			return fromEntAPIKey(updated), nil
		},
		// AIP-148 soft delete: stamp delete_time instead of hard-deleting.
		Delete_: func(ctx context.Context, key string) error {
			q := client.APIKey.UpdateOneID(key)
			// Tenant guard: ent query interceptors do NOT run for mutations, so the
			// account_id predicate must be applied explicitly or a caller could
			// soft-delete another tenant's row by ID.
			if tenantID := middleware.TenantIDFromContext(ctx); tenantID != "" {
				q = q.Where(entapikey.AccountID(tenantID))
			}
			// Only live rows are deletable: an already soft-deleted row must yield
			// ErrNotFound (consistent with MemoryRepository, the GORM shape, and the
			// batch methods), not silently re-stamp delete_time.
			q = q.Where(entapikey.DeleteTimeIsNil())
			err := q.SetDeleteTime(time.Now()).Exec(ctx)
			if ent.IsNotFound(err) {
				return persistence.ErrNotFound
			}
			return err
		},
		// AIP-149 undelete: clear delete_time. WithShowDeleted lets us query
		// soft-deleted rows; DeleteTimeNotNil ensures only deleted rows match
		// (returns ErrNotFound for live rows, per OQ-3 decision).
		Undelete_: func(ctx context.Context, key string) (*APIKey, error) {
			showCtx := entrepo.WithShowDeleted(ctx)
			existing, err := client.APIKey.Query().Where(
				entapikey.ID(key),
				entapikey.DeleteTimeNotNil(),
			).Only(showCtx)
			if err != nil {
				if ent.IsNotFound(err) {
					return nil, persistence.ErrNotFound
				}
				return nil, fmt.Errorf("undelete apikey: %w", err)
			}
			restored, err := existing.Update().ClearDeleteTime().Save(ctx)
			if err != nil {
				return nil, fmt.Errorf("undelete apikey: %w", err)
			}
			return fromEntAPIKey(restored), nil
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
	p := &APIKey{
		Id:        e.ID,
		Name:      e.Name,
		AccountId: e.AccountID,
		KeyPrefix: e.KeyPrefix,
		Tags:      e.Tags,
		Etag:      e.Etag, // AIP-154 (#49): surface the EtagMixin-stamped token so a
		// Get returns a stable value a client echoes as If-Match. The mixin stamps
		// and persists it automatically; this one line carries it onto the proto.
		// KeyValue intentionally omitted — never returned from storage
	}
	if e.DeleteTime != nil {
		p.DeleteTime = timestamppb.New(*e.DeleteTime)
	}
	if e.ExpireTime != nil {
		p.ExpireTime = timestamppb.New(*e.ExpireTime)
	}
	return p
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

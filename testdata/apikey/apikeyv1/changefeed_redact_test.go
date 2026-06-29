package apikeyv1

import (
	"context"
	"strings"
	"testing"

	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// TestChangeEmitting_DefaultMarshallerRedactsSecret proves the P1 change feed's
// DEFAULT marshaller is secret-safe against a real proto: APIKey.key_value is
// annotated (infoblox.field.v1.opts).secret = true, so the after-image the
// decorator writes to the durable outbox must have it redacted — a secret must
// never persist in the change feed. (It lives here, as an internal test package,
// because Go excludes testdata packages from cross-module imports, and the
// secret-annotated fixtures live in testdata.)
func TestChangeEmitting_DefaultMarshallerRedactsSecret(t *testing.T) {
	repo := persistence.NewMemoryRepository(func(k *APIKey) string { return k.GetId() })
	store := persistence.NewMemoryOutboxStore()
	tx := persistence.NewMemoryTxRunner(repo, store)
	pub := events.NewOutboxPublisher(store)

	// No Marshal option => the default redact-then-protojson marshaller.
	feed := events.ChangeEmitting[*APIKey, string](repo, tx, pub, events.ChangeFeedOptions[*APIKey]{
		ResourceType: "apikey.api_key",
		NameOf:       func(k *APIKey) string { return "apiKeys/" + k.GetId() },
	})

	ctx := middleware.WithTenantID(context.Background(), "acme")
	if _, err := feed.Create(ctx, &APIKey{Id: "k1", Label: "ci", KeyValue: "super-secret-material"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	var after string
	d := events.NewDispatcher(store, persistence.NewMemoryOutboxCursorStore(), tx, events.NewMemoryIdempotencyStore())
	d.Subscribe(events.ChangeEventType, "capture", func(_ context.Context, evt events.Event) error {
		ce, err := events.ChangeEventFromEvent(evt)
		if err != nil {
			return err
		}
		after = string(ce.After)
		return nil
	})
	if _, err := d.RunOnce(context.Background(), 100); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if after == "" {
		t.Fatal("no after-image captured")
	}
	if strings.Contains(after, "super-secret-material") {
		t.Errorf("SECURITY: secret key_value persisted in the change feed: %s", after)
	}
	if !strings.Contains(after, "REDACTED") {
		t.Errorf("secret field must be [REDACTED] in the after-image: %s", after)
	}
	if !strings.Contains(after, "k1") {
		t.Errorf("non-secret id must survive in the after-image: %s", after)
	}
}

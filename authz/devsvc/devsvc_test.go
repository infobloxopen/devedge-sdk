package devsvc_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/authz/devsvc"
)

// compile-time: Client and Store are authz.Authorizers (the same seam opaauthz implements).
var (
	_ authz.Authorizer = (*devsvc.Client)(nil)
	_ authz.Authorizer = (*devsvc.Store)(nil)
)

var adminReq = authz.AccessRequest{
	Principal: authz.Principal{Subject: "alice", Tenant: "tenant-a", Groups: []string{"admin"}},
	Verb:      authz.Get,
	Resource:  authz.Resource{Type: "order"},
}

func grantAdmin() authz.Grant {
	return authz.Grant{Tenant: "tenant-a", Subjects: []string{"group:admin"}, Verbs: []authz.Verb{"*"}, Resource: "*"}
}

func TestClientHandler_Decision(t *testing.T) {
	store := devsvc.NewStore() // no grants -> default deny
	srv := httptest.NewServer(devsvc.NewHandler(store))
	defer srv.Close()
	client := &devsvc.Client{BaseURL: srv.URL}

	// No grant -> deny.
	if dec, err := client.Authorize(context.Background(), adminReq); err != nil || dec.Allow {
		t.Fatalf("empty store: want deny, got allow=%v err=%v", dec.Allow, err)
	}
	// Add the grant live -> allow, with no restart.
	store.Replace(grantAdmin())
	if dec, err := client.Authorize(context.Background(), adminReq); err != nil || !dec.Allow {
		t.Fatalf("after Replace: want allow, got allow=%v err=%v", dec.Allow, err)
	}
	// Remove it live -> deny again.
	store.Replace()
	if dec, _ := client.Authorize(context.Background(), adminReq); dec.Allow {
		t.Fatal("after clearing grants: want deny, got allow")
	}
}

func TestClient_FailsClosed_WhenUnreachable(t *testing.T) {
	client := &devsvc.Client{BaseURL: "http://127.0.0.1:1", HTTP: &http.Client{Timeout: 200 * time.Millisecond}}
	dec, err := client.Authorize(context.Background(), adminReq)
	if err == nil {
		t.Fatal("unreachable service: want error")
	}
	if dec.Allow {
		t.Fatal("unreachable service: want deny (fail closed), got allow")
	}
}

func TestAdminEndpoint_FlipsLive(t *testing.T) {
	store := devsvc.NewStore()
	srv := httptest.NewServer(devsvc.NewHandler(store, devsvc.HandlerOptions{EnableAdmin: true}))
	defer srv.Close()
	client := &devsvc.Client{BaseURL: srv.URL}

	if dec, _ := client.Authorize(context.Background(), adminReq); dec.Allow {
		t.Fatal("want initial deny")
	}
	// PUT a granting rule set via the admin endpoint.
	body := []byte(`[{"Tenant":"tenant-a","Subjects":["group:admin"],"Verbs":["*"],"Resource":"*"}]`)
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/v1/grants", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("admin PUT: err=%v status=%v", err, resp.StatusCode)
	}
	resp.Body.Close()
	if dec, _ := client.Authorize(context.Background(), adminReq); !dec.Allow {
		t.Fatal("after admin PUT: want allow")
	}
}

// TestWatchGrantsFile_HotReload proves the "edit grants on disk -> decision flips
// live, no rebuild" acceptance: a denying file, then an edit granting access.
func TestWatchGrantsFile_HotReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "grants.json")
	if err := os.WriteFile(path, []byte(`[]`), 0o644); err != nil {
		t.Fatal(err)
	}

	store := devsvc.NewStore()
	ctx := t.Context() // canceled before test cleanup, so the watcher stops before TempDir removal
	if err := devsvc.WatchGrantsFile(ctx, path, store, 20*time.Millisecond, nil); err != nil {
		t.Fatalf("WatchGrantsFile: %v", err)
	}

	srv := httptest.NewServer(devsvc.NewHandler(store))
	defer srv.Close()
	client := &devsvc.Client{BaseURL: srv.URL}

	if dec, _ := client.Authorize(context.Background(), adminReq); dec.Allow {
		t.Fatal("empty grants file: want deny")
	}

	// Edit the file to grant access; force a later mtime so the poll detects it.
	if err := os.WriteFile(path, []byte(`[{"Tenant":"tenant-a","Subjects":["group:admin"],"Verbs":["*"],"Resource":"*"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Second)
	_ = os.Chtimes(path, future, future)

	// Poll until the reload takes effect (or fail after a bound).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if dec, _ := client.Authorize(context.Background(), adminReq); dec.Allow {
			return // hot-reload observed
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("grant edit was not hot-reloaded within 2s")
}

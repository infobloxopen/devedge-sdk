package widgetsv1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/lro"
	"github.com/infobloxopen/devedge-sdk/middleware/redact"
	"github.com/infobloxopen/devedge-sdk/server"
	"github.com/infobloxopen/devedge-sdk/testdata/toy/widgetsv1"
)

// newRedactingServer boots a real toy server with redact.ResponseUnary wired via
// Config.Interceptors, so write-only (INPUT_ONLY / secret) fields are stripped
// from every response at the wire boundary. Returns the HTTP gateway address.
func newRedactingServer(t *testing.T) string {
	t.Helper()
	lroStore := lro.NewMemoryStore(time.Hour)
	s, err := server.New(server.Config{
		GRPCAddr:   ":0",
		HTTPAddr:   ":0",
		Authorizer: authz.NewDevAuthorizer(authz.Grant{Tenant: "*", Subjects: []string{"*"}, Verbs: []authz.Verb{"*"}, Resource: "*"}),
		LROStore:   lroStore,
		// The response-redaction seam under test (FR-A4 / AC-3): opt-in, not the
		// framework default.
		Interceptors: []grpc.UnaryServerInterceptor{redact.ResponseUnary()},
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	if err := widgetsv1.RegisterWidgetService(s, newToyHandler(lroStore)); err != nil {
		t.Fatalf("RegisterWidgetService: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Serve(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if addr := s.HTTPAddr(); addr != "" && addr != ":0" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if addr := s.HTTPAddr(); addr == "" || addr == ":0" {
		t.Fatal("server did not bind HTTP address within 2s")
	}
	return s.HTTPAddr()
}

// TestE2E_InputOnlySecretStrippedFromRESTResponse is the runtime-boundary proof
// for AC-3: a widget created over REST with a secret (INPUT_ONLY) field set has
// that field ABSENT from both the create response and a subsequent GET response —
// stripped by middleware/redact, observed on the real wire (not asserted
// statically). The public displayName round-trips normally.
func TestE2E_InputOnlySecretStrippedFromRESTResponse(t *testing.T) {
	baseURL := "http://" + newRedactingServer(t)

	// Wait for the HTTP listener to accept connections.
	var httpClient http.Client
	ready := time.Now().Add(2 * time.Second)
	for time.Now().Before(ready) {
		if resp, err := httpClient.Get(baseURL + "/v1/widgets"); err == nil {
			_ = resp.Body.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Create a widget carrying a secret_token (INPUT_ONLY) over REST.
	body := `{"id":"e2e-secret-1","displayName":"e2e","color":"blue","secretToken":"topsecret"}`
	createResp, err := httpClient.Post(baseURL+"/v1/widgets", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/widgets: %v", err)
	}
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/widgets: status %d", createResp.StatusCode)
	}
	var created map[string]any
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	// The create response must NOT carry the secret material.
	if v, present := created["secretToken"]; present && v != "" {
		t.Errorf("create response leaked secretToken: %v", v)
	}
	if created["displayName"] != "e2e" {
		t.Errorf("create response displayName = %v, want e2e", created["displayName"])
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("create response missing id")
	}

	// GET the widget: the stored secret must not appear on the wire either.
	getResp, err := httpClient.Get(baseURL + "/v1/widgets/" + id)
	if err != nil {
		t.Fatalf("GET /v1/widgets/%s: %v", id, err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/widgets/%s: status %d", id, getResp.StatusCode)
	}
	raw, _ := json.Marshal(decodeBody(t, getResp))
	if strings.Contains(string(raw), "topsecret") {
		t.Errorf("GET response leaked the secret value: %s", raw)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("re-decode get response: %v", err)
	}
	if v, present := got["secretToken"]; present && v != "" {
		t.Errorf("GET response leaked secretToken: %v", v)
	}
	if got["displayName"] != "e2e" {
		t.Errorf("GET response displayName = %v, want e2e (public field must round-trip)", got["displayName"])
	}
}

func decodeBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return m
}

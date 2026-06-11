package widgetsv1_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/middleware/etag"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/server"
	"github.com/infobloxopen/devedge-sdk/testdata/toy/widgetsv1"
)

// toyHandler is a concrete in-memory WidgetServiceServer used by integration tests.
type toyHandler struct {
	widgetsv1.UnimplementedWidgetServiceServer
	repo *persistence.MemoryRepository[*widgetsv1.Widget, string]
}

func (h *toyHandler) CreateWidget(ctx context.Context, req *widgetsv1.CreateWidgetRequest) (*widgetsv1.Widget, error) {
	if req.Widget == nil {
		return nil, status.Error(codes.InvalidArgument, "widget required")
	}
	if req.Widget.Id == "" {
		req.Widget.Id = uuid.New().String()
	}
	w, err := h.repo.Create(ctx, req.Widget)
	if err != nil {
		return nil, err
	}
	storedETag := h.repo.GetETagForKey(w.Id)
	etag.SetNewETag(ctx, storedETag)
	return w, nil
}

func (h *toyHandler) GetWidget(ctx context.Context, req *widgetsv1.GetWidgetRequest) (*widgetsv1.Widget, error) {
	w, err := h.repo.Get(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	return w, nil
}

func (h *toyHandler) ListWidgets(ctx context.Context, req *widgetsv1.ListWidgetsRequest) (*widgetsv1.ListWidgetsResponse, error) {
	items, nextToken, err := h.repo.List(ctx, persistence.ListOptions{
		PageSize:  int(req.PageSize),
		PageToken: req.PageToken,
	})
	if err != nil {
		return nil, err
	}
	return &widgetsv1.ListWidgetsResponse{
		Widgets:       items,
		NextPageToken: nextToken,
	}, nil
}

func (h *toyHandler) UpdateWidget(ctx context.Context, req *widgetsv1.UpdateWidgetRequest) (*widgetsv1.Widget, error) {
	if req.Widget == nil {
		return nil, status.Error(codes.InvalidArgument, "widget required")
	}
	w, err := h.repo.Update(ctx, req.Widget.Id, req.Widget, req.UpdateMask...)
	if err != nil {
		return nil, err
	}
	newETag := h.repo.GetETagForKey(w.Id)
	etag.SetNewETag(ctx, newETag)
	return w, nil
}

func (h *toyHandler) DeleteWidget(ctx context.Context, req *widgetsv1.DeleteWidgetRequest) (*widgetsv1.DeleteWidgetResponse, error) {
	if err := h.repo.Delete(ctx, req.Id); err != nil {
		return nil, err
	}
	return &widgetsv1.DeleteWidgetResponse{}, nil
}

// newTestServer boots a real server with port :0 (kernel-assigned), registers
// the toy WidgetService, and returns the server plus its gRPC and HTTP addresses.
// It also wires a Cleanup that cancels the server's context on test finish.
func newTestServer(t *testing.T, authorizer authz.Authorizer) (*server.Server, string, string) {
	t.Helper()
	s, err := server.New(server.Config{
		GRPCAddr:   ":0",
		HTTPAddr:   ":0",
		Rules:      widgetsv1.WidgetServiceAuthzRules,
		Authorizer: authorizer,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	handler := &toyHandler{
		repo: persistence.NewMemoryRepository[*widgetsv1.Widget, string](func(w *widgetsv1.Widget) string { return w.Id }),
	}
	if err := widgetsv1.RegisterWidgetService(s, handler); err != nil {
		t.Fatalf("RegisterWidgetService: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	serveErr := make(chan error, 1)
	go func() {
		if err := s.Serve(ctx); err != nil {
			serveErr <- err
		}
	}()

	// Poll until the gRPC address is bound (Serve sets grpcLis before returning from
	// net.Listen, so GRPCAddr() becomes non-empty quickly).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if addr := s.GRPCAddr(); addr != "" && addr != ":0" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if addr := s.GRPCAddr(); addr == "" || addr == ":0" {
		t.Fatal("server did not bind gRPC address within 2s")
	}

	// Also wait for HTTP addr to be bound.
	for time.Now().Before(deadline) {
		if addr := s.HTTPAddr(); addr != "" && addr != ":0" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	return s, s.GRPCAddr(), s.HTTPAddr()
}

// dialGRPC dials the test gRPC endpoint with an insecure connection.
func dialGRPC(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient(%q): %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// ctxWithMD returns a context carrying the supplied key-value metadata pairs.
func ctxWithMD(pairs ...string) context.Context {
	return metadata.NewOutgoingContext(context.Background(), metadata.Pairs(pairs...))
}

// -----------------------------------------------------------------------
// TestIntegration_AuthZ: permissive authorizer vs. default-deny authorizer
// -----------------------------------------------------------------------

func TestIntegration_AuthZ(t *testing.T) {
	// Sub-test 1: DevAuthorizer that grants everything to every subject.
	t.Run("allowed when grant matches", func(t *testing.T) {
		permissive := authz.NewDevAuthorizer(authz.Grant{
			Tenant:   "*",
			Subjects: []string{"*"},
			Verbs:    []authz.Verb{"*"},
			Resource: "*",
		})
		_, grpcAddr, _ := newTestServer(t, permissive)

		conn := dialGRPC(t, grpcAddr)
		client := widgetsv1.NewWidgetServiceClient(conn)

		// Pass account-id so TenantID middleware is satisfied.
		ctx := ctxWithMD("account-id", "alice")
		_, err := client.CreateWidget(ctx, &widgetsv1.CreateWidgetRequest{
			Widget: &widgetsv1.Widget{Name: "gadget"},
		})
		if err != nil {
			t.Fatalf("CreateWidget: want nil error, got %v", err)
		}
	})

	// Sub-test 2: Default-deny DevAuthorizer (no grants) → PermissionDenied.
	t.Run("denied when no grant", func(t *testing.T) {
		denyAll := authz.NewDevAuthorizer() // zero grants
		_, grpcAddr, _ := newTestServer(t, denyAll)

		conn := dialGRPC(t, grpcAddr)
		client := widgetsv1.NewWidgetServiceClient(conn)

		ctx := ctxWithMD("account-id", "alice")
		_, err := client.CreateWidget(ctx, &widgetsv1.CreateWidgetRequest{
			Widget: &widgetsv1.Widget{Name: "gadget"},
		})
		if err == nil {
			t.Fatal("CreateWidget: want PermissionDenied error, got nil")
		}
		st, ok := status.FromError(err)
		if !ok {
			t.Fatalf("CreateWidget: expected gRPC status error, got %T: %v", err, err)
		}
		if st.Code() != codes.PermissionDenied {
			t.Fatalf("CreateWidget: want PermissionDenied, got %v", st.Code())
		}
	})
}

// -----------------------------------------------------------------------
// TestIntegration_ETag: create → update with correct ETag → update with stale ETag
// -----------------------------------------------------------------------

func TestIntegration_ETag(t *testing.T) {
	permissive := authz.NewDevAuthorizer(authz.Grant{
		Tenant:   "*",
		Subjects: []string{"*"},
		Verbs:    []authz.Verb{"*"},
		Resource: "*",
	})
	_, grpcAddr, _ := newTestServer(t, permissive)

	conn := dialGRPC(t, grpcAddr)
	client := widgetsv1.NewWidgetServiceClient(conn)
	ctx := ctxWithMD("account-id", "tenant1")

	// 1. Create a widget and capture the ETag from the trailer.
	var createTrailer metadata.MD
	w, err := client.CreateWidget(ctx, &widgetsv1.CreateWidgetRequest{
		Widget: &widgetsv1.Widget{Name: "sprocket"},
	}, grpc.Trailer(&createTrailer))
	if err != nil {
		t.Fatalf("CreateWidget: %v", err)
	}
	etagVals := createTrailer.Get("etag")
	if len(etagVals) == 0 {
		t.Fatal("CreateWidget trailer: missing etag")
	}
	firstETag := etagVals[0]
	if firstETag == "" {
		t.Fatal("CreateWidget trailer: empty etag")
	}
	t.Logf("created widget %q with etag %q", w.Id, firstETag)

	// 2. Update with the correct ETag → should succeed and return a new ETag.
	updateCtx := ctxWithMD("account-id", "tenant1", "if-match", firstETag)
	var updateTrailer metadata.MD
	updated, err := client.UpdateWidget(updateCtx, &widgetsv1.UpdateWidgetRequest{
		Widget:     &widgetsv1.Widget{Id: w.Id, Name: "sprocket-v2"},
		UpdateMask: []string{"name"},
	}, grpc.Trailer(&updateTrailer))
	if err != nil {
		t.Fatalf("UpdateWidget (correct ETag): %v", err)
	}
	if updated.Name != "sprocket-v2" {
		t.Errorf("UpdateWidget: want name %q, got %q", "sprocket-v2", updated.Name)
	}
	newETagVals := updateTrailer.Get("etag")
	if len(newETagVals) == 0 {
		t.Fatal("UpdateWidget trailer: missing etag")
	}
	secondETag := newETagVals[0]
	if secondETag == firstETag {
		t.Errorf("UpdateWidget: ETag should change after update, both are %q", firstETag)
	}
	t.Logf("updated widget; new etag %q", secondETag)

	// 3. Update with the now-stale first ETag → FailedPrecondition (codes.FailedPrecondition == 9).
	staleCtx := ctxWithMD("account-id", "tenant1", "if-match", firstETag)
	_, err = client.UpdateWidget(staleCtx, &widgetsv1.UpdateWidgetRequest{
		Widget:     &widgetsv1.Widget{Id: w.Id, Name: "should-not-apply"},
		UpdateMask: []string{"name"},
	})
	if err == nil {
		t.Fatal("UpdateWidget (stale ETag): want FailedPrecondition, got nil error")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("UpdateWidget (stale ETag): expected gRPC status error, got %T: %v", err, err)
	}
	if st.Code() != codes.FailedPrecondition {
		t.Fatalf("UpdateWidget (stale ETag): want FailedPrecondition (9), got %v (%d)", st.Code(), st.Code())
	}
	t.Logf("stale ETag correctly rejected with %v", st.Code())
}

// -----------------------------------------------------------------------
// TestIntegration_Pagination: create 5 widgets, list page_size=2 three times
// -----------------------------------------------------------------------

func TestIntegration_Pagination(t *testing.T) {
	permissive := authz.NewDevAuthorizer(authz.Grant{
		Tenant:   "*",
		Subjects: []string{"*"},
		Verbs:    []authz.Verb{"*"},
		Resource: "*",
	})
	_, grpcAddr, _ := newTestServer(t, permissive)

	conn := dialGRPC(t, grpcAddr)
	client := widgetsv1.NewWidgetServiceClient(conn)
	ctx := ctxWithMD("account-id", "tenant1")

	// Create 5 widgets.
	for i := range 5 {
		_, err := client.CreateWidget(ctx, &widgetsv1.CreateWidgetRequest{
			Widget: &widgetsv1.Widget{
				Id:   fmt.Sprintf("widget-%02d", i+1),
				Name: fmt.Sprintf("Widget %d", i+1),
			},
		})
		if err != nil {
			t.Fatalf("CreateWidget %d: %v", i+1, err)
		}
	}

	// Page 1: expect 2 items + next_page_token.
	page1, err := client.ListWidgets(ctx, &widgetsv1.ListWidgetsRequest{PageSize: 2})
	if err != nil {
		t.Fatalf("ListWidgets page1: %v", err)
	}
	if len(page1.Widgets) != 2 {
		t.Fatalf("page1: want 2 widgets, got %d", len(page1.Widgets))
	}
	if page1.NextPageToken == "" {
		t.Fatal("page1: expected non-empty NextPageToken")
	}
	t.Logf("page1: %d widgets, token=%q", len(page1.Widgets), page1.NextPageToken)

	// Page 2: expect 2 items + next_page_token.
	page2, err := client.ListWidgets(ctx, &widgetsv1.ListWidgetsRequest{
		PageSize:  2,
		PageToken: page1.NextPageToken,
	})
	if err != nil {
		t.Fatalf("ListWidgets page2: %v", err)
	}
	if len(page2.Widgets) != 2 {
		t.Fatalf("page2: want 2 widgets, got %d", len(page2.Widgets))
	}
	if page2.NextPageToken == "" {
		t.Fatal("page2: expected non-empty NextPageToken")
	}
	t.Logf("page2: %d widgets, token=%q", len(page2.Widgets), page2.NextPageToken)

	// Page 3: expect 1 item + empty next_page_token.
	page3, err := client.ListWidgets(ctx, &widgetsv1.ListWidgetsRequest{
		PageSize:  2,
		PageToken: page2.NextPageToken,
	})
	if err != nil {
		t.Fatalf("ListWidgets page3: %v", err)
	}
	if len(page3.Widgets) != 1 {
		t.Fatalf("page3: want 1 widget, got %d", len(page3.Widgets))
	}
	if page3.NextPageToken != "" {
		t.Fatalf("page3: expected empty NextPageToken, got %q", page3.NextPageToken)
	}
	t.Logf("page3: %d widget, no more pages", len(page3.Widgets))
}

// -----------------------------------------------------------------------
// TestIntegration_RequestID: request succeeds even without x-request-id header
// (the interceptor generates one automatically).
// -----------------------------------------------------------------------

func TestIntegration_RequestID(t *testing.T) {
	permissive := authz.NewDevAuthorizer(authz.Grant{
		Tenant:   "*",
		Subjects: []string{"*"},
		Verbs:    []authz.Verb{"*"},
		Resource: "*",
	})
	_, grpcAddr, _ := newTestServer(t, permissive)

	conn := dialGRPC(t, grpcAddr)
	client := widgetsv1.NewWidgetServiceClient(conn)

	// No x-request-id metadata — the server must inject one.
	ctx := ctxWithMD("account-id", "tenant1")
	var header metadata.MD
	_, err := client.CreateWidget(ctx, &widgetsv1.CreateWidgetRequest{
		Widget: &widgetsv1.Widget{Name: "auto-id-test"},
	}, grpc.Header(&header))
	if err != nil {
		t.Fatalf("CreateWidget (no x-request-id): %v", err)
	}
	// The server sets x-request-id in the response header via the interceptor.
	ridVals := header.Get("x-request-id")
	if len(ridVals) == 0 || ridVals[0] == "" {
		t.Error("response header: expected non-empty x-request-id set by server")
	} else {
		t.Logf("server-generated x-request-id: %q", ridVals[0])
	}
}

// -----------------------------------------------------------------------
// TestIntegration_HTTPGateway: exercise the JSON/HTTP gateway endpoints
// -----------------------------------------------------------------------

func TestIntegration_HTTPGateway(t *testing.T) {
	permissive := authz.NewDevAuthorizer(authz.Grant{
		Tenant:   "*",
		Subjects: []string{"*"},
		Verbs:    []authz.Verb{"*"},
		Resource: "*",
	})
	_, _, httpAddr := newTestServer(t, permissive)
	if httpAddr == "" {
		t.Skip("HTTP gateway not bound")
	}
	baseURL := "http://" + httpAddr

	// Wait until the HTTP server is actually accepting connections.
	httpReady := time.Now().Add(2 * time.Second)
	var httpClient http.Client
	for time.Now().Before(httpReady) {
		resp, err := httpClient.Get(baseURL + "/v1/widgets")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 1. POST /v1/widgets → 200 with JSON widget.
	t.Run("CreateWidget", func(t *testing.T) {
		body := `{"name":"http-widget","color":"blue"}`
		resp, err := httpClient.Post(baseURL+"/v1/widgets", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST /v1/widgets: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /v1/widgets: want 200, got %d", resp.StatusCode)
		}
		var result map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("POST /v1/widgets: decode response: %v", err)
		}
		if result["name"] != "http-widget" {
			t.Errorf("POST /v1/widgets: want name=%q, got %v", "http-widget", result["name"])
		}
		t.Logf("created via HTTP: id=%v name=%v", result["id"], result["name"])
	})

	// 2. GET /v1/widgets/{id} → 200 for an existing widget.
	t.Run("GetWidget", func(t *testing.T) {
		// First create one so we know its ID.
		body := `{"name":"get-test","color":"red"}`
		createResp, err := httpClient.Post(baseURL+"/v1/widgets", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST /v1/widgets: %v", err)
		}
		defer createResp.Body.Close()
		var created map[string]any
		if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
			t.Fatalf("decode create response: %v", err)
		}
		id, _ := created["id"].(string)
		if id == "" {
			t.Fatal("created widget has no id")
		}

		getResp, err := httpClient.Get(fmt.Sprintf("%s/v1/widgets/%s", baseURL, id))
		if err != nil {
			t.Fatalf("GET /v1/widgets/%s: %v", id, err)
		}
		defer getResp.Body.Close()
		if getResp.StatusCode != http.StatusOK {
			t.Fatalf("GET /v1/widgets/%s: want 200, got %d", id, getResp.StatusCode)
		}
		var got map[string]any
		if err := json.NewDecoder(getResp.Body).Decode(&got); err != nil {
			t.Fatalf("decode get response: %v", err)
		}
		if got["id"] != id {
			t.Errorf("GET /v1/widgets/%s: want id=%q, got %v", id, id, got["id"])
		}
		t.Logf("fetched via HTTP: id=%v name=%v", got["id"], got["name"])
	})

	// 3. GET /v1/widgets?page_size=2 → 200 with next_page_token when >2 items exist.
	t.Run("ListWidgets", func(t *testing.T) {
		// Create a few more to ensure pagination triggers.
		for i := range 3 {
			body := fmt.Sprintf(`{"name":"paginate-%d"}`, i)
			resp, err := httpClient.Post(baseURL+"/v1/widgets", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatalf("POST create: %v", err)
			}
			_ = resp.Body.Close()
		}

		listResp, err := httpClient.Get(baseURL + "/v1/widgets?page_size=2")
		if err != nil {
			t.Fatalf("GET /v1/widgets?page_size=2: %v", err)
		}
		defer listResp.Body.Close()
		if listResp.StatusCode != http.StatusOK {
			t.Fatalf("GET /v1/widgets?page_size=2: want 200, got %d", listResp.StatusCode)
		}
		var result map[string]any
		if err := json.NewDecoder(listResp.Body).Decode(&result); err != nil {
			t.Fatalf("decode list response: %v", err)
		}
		items, _ := result["widgets"].([]any)
		if len(items) != 2 {
			t.Errorf("ListWidgets: want 2 items, got %d", len(items))
		}
		if result["nextPageToken"] == "" || result["nextPageToken"] == nil {
			t.Errorf("ListWidgets: expected non-empty nextPageToken when more than 2 items exist")
		}
		t.Logf("list page1: %d items, next_page_token=%v", len(items), result["nextPageToken"])
	})
}

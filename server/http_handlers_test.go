package server_test

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/server"
)

// startWithHandlers spins up a server with the given HTTPHandlers and returns
// the bound HTTP address plus a cancel func.
func startWithHandlers(t *testing.T, handlers []server.HTTPHandler) (httpAddr string, cancel func()) {
	t.Helper()
	s, err := server.New(server.Config{
		GRPCAddr:     ":0",
		HTTPAddr:     ":0",
		HTTPHandlers: handlers,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ctx, ctxCancel := context.WithCancel(context.Background())
	go func() { _ = s.Serve(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ha := s.HTTPAddr(); ha != ":0" && ha != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s.HTTPAddr() == ":0" || s.HTTPAddr() == "" {
		ctxCancel()
		t.Fatal("server did not start within 5s")
	}
	return s.HTTPAddr(), ctxCancel
}

func getBody(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestHTTPHandlers_MountedAlongsideGateway verifies a custom handler on a
// specific prefix is served, the probes still win, and unmatched paths fall
// through to the (empty) gateway with a 404 — not to the custom handler.
func TestHTTPHandlers_MountedAlongsideGateway(t *testing.T) {
	httpAddr, cancel := startWithHandlers(t, []server.HTTPHandler{
		{Pattern: "/oauth/", Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "token-endpoint")
		})},
		{Pattern: "/.well-known/openid-configuration", Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "discovery")
		})},
	})
	defer cancel()
	base := "http://" + httpAddr

	if code, body := getBody(t, base+"/oauth/token"); code != 200 || body != "token-endpoint" {
		t.Errorf("/oauth/token = %d %q, want 200 \"token-endpoint\"", code, body)
	}
	if code, body := getBody(t, base+"/.well-known/openid-configuration"); code != 200 || body != "discovery" {
		t.Errorf("discovery = %d %q, want 200 \"discovery\"", code, body)
	}
	// Probe still owned by the server, not shadowed by any handler.
	if code, _ := getBody(t, base+"/healthz"); code != 200 {
		t.Errorf("/healthz = %d, want 200", code)
	}
	// Unmatched path falls through to the empty gateway (404), NOT to /oauth/.
	if code, _ := getBody(t, base+"/nope"); code != 404 {
		t.Errorf("/nope = %d, want 404 (gateway catch-all)", code)
	}
}

// TestHTTPHandlers_RootClaimReplacesGateway verifies a handler claiming "/"
// becomes the catch-all (no duplicate-pattern panic) while probes still win.
func TestHTTPHandlers_RootClaimReplacesGateway(t *testing.T) {
	httpAddr, cancel := startWithHandlers(t, []server.HTTPHandler{
		{Pattern: "/", Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "op-root")
		})},
	})
	defer cancel()
	base := "http://" + httpAddr

	if code, body := getBody(t, base+"/anything/here"); code != 200 || body != "op-root" {
		t.Errorf("/anything/here = %d %q, want 200 \"op-root\"", code, body)
	}
	if code, _ := getBody(t, base+"/readyz"); code != 200 {
		t.Errorf("/readyz = %d, want 200 (probe wins over root handler)", code)
	}
}

func TestHTTPHandlers_Validation(t *testing.T) {
	tests := []struct {
		name string
		cfg  server.Config
	}{
		{"no HTTPAddr", server.Config{GRPCAddr: ":0", HTTPHandlers: []server.HTTPHandler{{Pattern: "/x", Handler: http.NotFoundHandler()}}}},
		{"empty pattern", server.Config{GRPCAddr: ":0", HTTPAddr: ":0", HTTPHandlers: []server.HTTPHandler{{Pattern: "", Handler: http.NotFoundHandler()}}}},
		{"nil handler", server.Config{GRPCAddr: ":0", HTTPAddr: ":0", HTTPHandlers: []server.HTTPHandler{{Pattern: "/x"}}}},
		{"reserved healthz", server.Config{GRPCAddr: ":0", HTTPAddr: ":0", HTTPHandlers: []server.HTTPHandler{{Pattern: "/healthz", Handler: http.NotFoundHandler()}}}},
		{"duplicate pattern", server.Config{GRPCAddr: ":0", HTTPAddr: ":0", HTTPHandlers: []server.HTTPHandler{
			{Pattern: "/x", Handler: http.NotFoundHandler()},
			{Pattern: "/x", Handler: http.NotFoundHandler()},
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := server.New(tt.cfg); err == nil {
				t.Errorf("New(%s) = nil error, want error", tt.name)
			}
		})
	}
}

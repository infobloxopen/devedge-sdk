// Package devsvc is the out-of-process, hot-reloadable sibling of the in-process
// authz.DevAuthorizer (WS-026 P1b): the dev-manipulable OSS reference behind the
// authz.Authorizer seam. It ships both halves of one wire protocol:
//
//   - [Client] implements authz.Authorizer by calling a running dev authz service
//     over HTTP — the same seam opaauthz (OPA/PARGS) implements for production, so
//     a service swaps dev↔prod with no code change (server.Config.Authorizer).
//   - [Handler] serves that protocol from a [Store] of readable authz.Grants,
//     reusing authz.DevAuthorizer for the decision. The store is HOT-RELOADABLE
//     (edit grants on disk and reload, or PUT them via the admin endpoint) so a
//     developer flips a decision live — "make this method allowed/denied" — with
//     no rebuild or restart.
//
// It is dependency-light (stdlib + authz only) and lives in the root module.
// Production authz stays OPA/PARGS — this is the dev default, not a policy engine.
package devsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/infobloxopen/devedge-sdk/authz"
)

// DefaultAuthorizePath is the decision endpoint the Client posts to and the
// Handler serves.
const DefaultAuthorizePath = "/v1/authorize"

// wireRequest is the JSON authorization question on the wire. It mirrors
// authz.AccessRequest with explicit, stable field names.
type wireRequest struct {
	Principal authz.Principal `json:"principal"`
	Verb      authz.Verb      `json:"verb"`
	Resource  authz.Resource  `json:"resource"`
	Method    string          `json:"method,omitempty"`
	Features  []string        `json:"features,omitempty"`
	Context   map[string]any  `json:"context,omitempty"`
}

// wireDecision is the JSON answer on the wire.
type wireDecision struct {
	Allow       bool           `json:"allow"`
	Reason      string         `json:"reason,omitempty"`
	Obligations map[string]any `json:"obligations,omitempty"`
}

// Store holds the dev authz service's readable authz.Grants behind the
// decision engine, swappable atomically for hot-reload. Safe for concurrent use.
type Store struct {
	az atomic.Pointer[authz.DevAuthorizer]
	// grants keeps the last-installed set so callers/UT can read current state.
	mu     sync.RWMutex
	grants []authz.Grant
}

// NewStore returns a store seeded with grants.
func NewStore(grants ...authz.Grant) *Store {
	s := &Store{}
	s.Replace(grants...)
	return s
}

// Replace swaps the whole grant set atomically — the hot-reload primitive. A
// concurrent Authorize sees either the old or the new set, never a torn one.
func (s *Store) Replace(grants ...authz.Grant) {
	cp := append([]authz.Grant(nil), grants...)
	s.az.Store(authz.NewDevAuthorizer(cp...))
	s.mu.Lock()
	s.grants = cp
	s.mu.Unlock()
}

// Grants returns a copy of the currently installed grants.
func (s *Store) Grants() []authz.Grant {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]authz.Grant(nil), s.grants...)
}

// Authorize applies the current grants (implements authz.Authorizer too, so the
// Store is usable in-process as well as behind the Handler).
func (s *Store) Authorize(ctx context.Context, req authz.AccessRequest) (authz.Decision, error) {
	return s.az.Load().Authorize(ctx, req)
}

// HandlerOptions configures [NewHandler].
type HandlerOptions struct {
	// AuthorizePath overrides DefaultAuthorizePath.
	AuthorizePath string
	// EnableAdmin mounts a PUT <AuthorizePath>/../grants endpoint that replaces the
	// grant set live (dev-manipulability via API). Off by default — the endpoint is
	// unauthenticated and dev-only.
	EnableAdmin bool
	// AdminPath overrides the admin grants endpoint path (default "/v1/grants").
	AdminPath string
}

// NewHandler serves the dev authz protocol from store. POST <AuthorizePath>
// decides one request; when EnableAdmin, PUT <AdminPath> replaces the grants
// (body: JSON array of authz.Grant) so a developer flips decisions live.
func NewHandler(store *Store, opts ...HandlerOptions) http.Handler {
	var o HandlerOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	authorizePath := o.AuthorizePath
	if authorizePath == "" {
		authorizePath = DefaultAuthorizePath
	}
	adminPath := o.AdminPath
	if adminPath == "" {
		adminPath = "/v1/grants"
	}

	mux := http.NewServeMux()
	mux.HandleFunc(authorizePath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var wr wireRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&wr); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		dec, err := store.Authorize(r.Context(), authz.AccessRequest{
			Principal: wr.Principal, Verb: wr.Verb, Resource: wr.Resource,
			Method: wr.Method, Features: wr.Features, Context: wr.Context,
		})
		if err != nil {
			http.Error(w, "authorize error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(wireDecision{Allow: dec.Allow, Reason: dec.Reason, Obligations: dec.Obligations})
	})

	if o.EnableAdmin {
		mux.HandleFunc(adminPath, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPut {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var grants []authz.Grant
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&grants); err != nil {
				http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
				return
			}
			store.Replace(grants...)
			w.WriteHeader(http.StatusNoContent)
		})
	}
	return mux
}

// Client is an authz.Authorizer that decides by calling a running dev authz
// service. It is the dev-time stand-in for opaauthz: the same seam, a different
// backend. Fail-closed: any transport/decode error denies.
type Client struct {
	// BaseURL is the dev authz service root (e.g. "http://127.0.0.1:8090").
	BaseURL string
	// AuthorizePath overrides DefaultAuthorizePath.
	AuthorizePath string
	// HTTP is the client used; nil defaults to http.DefaultClient.
	HTTP *http.Client
}

// Authorize implements authz.Authorizer.
func (c *Client) Authorize(ctx context.Context, req authz.AccessRequest) (authz.Decision, error) {
	path := c.AuthorizePath
	if path == "" {
		path = DefaultAuthorizePath
	}
	body, err := json.Marshal(wireRequest{
		Principal: req.Principal, Verb: req.Verb, Resource: req.Resource,
		Method: req.Method, Features: req.Features, Context: req.Context,
	})
	if err != nil {
		return authz.Decision{Allow: false, Reason: "devsvc: marshal request"}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return authz.Decision{Allow: false, Reason: "devsvc: build request"}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		// Fail closed on a transport error.
		return authz.Decision{Allow: false, Reason: "devsvc: unreachable"}, fmt.Errorf("devsvc: call authz service: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return authz.Decision{Allow: false, Reason: fmt.Sprintf("devsvc: status %d", resp.StatusCode)},
			fmt.Errorf("devsvc: authz service status %d", resp.StatusCode)
	}
	var wd wireDecision
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&wd); err != nil {
		return authz.Decision{Allow: false, Reason: "devsvc: decode decision"}, fmt.Errorf("devsvc: decode decision: %w", err)
	}
	return authz.Decision{Allow: wd.Allow, Reason: wd.Reason, Obligations: wd.Obligations}, nil
}

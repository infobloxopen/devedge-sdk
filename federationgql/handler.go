package federationgql

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/graphql-go/graphql"
)

// graphQLRequest is the standard GraphQL-over-HTTP POST body.
type graphQLRequest struct {
	Query         string         `json:"query"`
	OperationName string         `json:"operationName"`
	Variables     map[string]any `json:"variables"`
}

// Handler returns an http.Handler that executes GraphQL queries against schema.
// It is authz-transparent (D-4): the incoming *http.Request's context — which
// carries whatever principal/metadata an upstream auth middleware placed on it —
// is used verbatim as the GraphQL execution context, and the resolvers forward
// it into their downstream service clients. The gateway constructs and elevates
// nothing; a downstream PermissionDenied surfaces as a per-field GraphQL error
// (null + errors[]), never composed data.
//
// It accepts a POST with a JSON body {query, operationName, variables} (the
// standard GraphQL-over-HTTP shape) and a GET with a ?query= parameter (handy
// for a browser/curl smoke). Each request gets a fresh request-scoped cache so
// the eager per-collection preload (D-3) is isolated per request.
func Handler(schema graphql.Schema) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, err := parseRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Query == "" {
			http.Error(w, "graphql: empty query", http.StatusBadRequest)
			return
		}

		// Carry the request's context (principal/metadata) into execution and
		// attach a fresh per-request preload cache.
		ctx := withCache(r.Context(), newRequestCache())

		result := graphql.Do(graphql.Params{
			Schema:         schema,
			RequestString:  req.Query,
			OperationName:  req.OperationName,
			VariableValues: req.Variables,
			Context:        ctx,
		})

		w.Header().Set("Content-Type", "application/json")
		// GraphQL returns 200 even with field-level errors (partial data +
		// errors[]); a transport-level failure would already have short-circuited
		// above.
		_ = json.NewEncoder(w).Encode(result)
	})
}

// Execute runs a query against schema with the given context (principal/metadata
// on ctx propagate to downstream calls) and variables, attaching a fresh
// request-scoped preload cache. It is the programmatic entry the HTTP Handler
// wraps — used directly by tests and by a caller embedding the gateway without
// HTTP.
func Execute(ctx context.Context, schema graphql.Schema, query string, variables map[string]any) *graphql.Result {
	return graphql.Do(graphql.Params{
		Schema:         schema,
		RequestString:  query,
		VariableValues: variables,
		Context:        withCache(ctx, newRequestCache()),
	})
}

func parseRequest(r *http.Request) (graphQLRequest, error) {
	var req graphQLRequest
	switch r.Method {
	case http.MethodGet:
		req.Query = r.URL.Query().Get("query")
		req.OperationName = r.URL.Query().Get("operationName")
		return req, nil
	default:
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			return req, err
		}
		if len(body) == 0 {
			return req, nil
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return req, err
		}
		return req, nil
	}
}

# F042 GraphQL federation gateway sample. Standalone consumer module (own go.mod
# + replace to the working tree), so every target runs GOWORK=off.

GRAPHQL_URL ?= http://localhost:8080/graphql

.PHONY: run query e2e test build

# run starts the region + asset services in-process, seeds the demo data, and
# serves the GraphQL gateway on :8080. Ctrl-C to stop.
run:
	GOWORK=off go run ./gateway

# query curls the federated cross-service query against a running gateway.
query:
	curl -s $(GRAPHQL_URL) \
	  -H 'X-Account-Id: acme' \
	  -H 'Content-Type: application/json' \
	  -d '{"query":"{ assets { id name region { id name } } }"}' | { command -v jq >/dev/null && jq . || cat; }

# e2e runs the end-to-end test: region + asset + gateway over real listeners,
# asserting the single-BatchGet / read_mask / authz guarantees.
e2e test:
	GOWORK=off go test -race ./...

build:
	GOWORK=off go build ./...

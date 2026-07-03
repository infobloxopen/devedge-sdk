package slo

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// AIP standard-method classifications carried by x-aip-method in the enriched
// OpenAPI (WS-024).
const (
	aipGet      = "Get"
	aipList     = "List"
	aipBatchGet = "BatchGet"
	aipCreate   = "Create"
	aipUpdate   = "Update"
	aipDelete   = "Delete"
	aipUndelete = "Undelete"
)

// readMethods and writeMethods are the AIP classes the grouped default SLOs
// cover. Custom/unclassified methods are excluded (a service refines them).
var (
	readMethods  = map[string]bool{aipGet: true, aipList: true, aipBatchGet: true}
	writeMethods = map[string]bool{aipCreate: true, aipUpdate: true, aipDelete: true, aipUndelete: true}
)

// operation is one enumerated OpenAPI operation with its AIP classification.
type operation struct {
	Service    string // short service name, from operationId prefix or tag
	Method     string // short method name, e.g. ListWidgets
	AIPMethod  string // x-aip-method
	HTTPMethod string // get/post/...
	Path       string
}

type openAPIDoc struct {
	Info struct {
		Title string `yaml:"title"`
	} `yaml:"info"`
	Paths map[string]pathItem `yaml:"paths"`
}

type pathItem struct {
	Get    *openAPIOperation `yaml:"get,omitempty"`
	Put    *openAPIOperation `yaml:"put,omitempty"`
	Post   *openAPIOperation `yaml:"post,omitempty"`
	Patch  *openAPIOperation `yaml:"patch,omitempty"`
	Delete *openAPIOperation `yaml:"delete,omitempty"`
}

type openAPIOperation struct {
	OperationID string   `yaml:"operationId"`
	Tags        []string `yaml:"tags"`
	XAIPMethod  string   `yaml:"x-aip-method"`
}

// parseOpenAPI reads an enriched OpenAPI YAML doc and enumerates its operations
// with their AIP classification, deterministically ordered by (path, verb).
func parseOpenAPI(data []byte) (title string, ops []operation, err error) {
	var doc openAPIDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", nil, fmt.Errorf("slo: parse openapi: %w", err)
	}
	paths := make([]string, 0, len(doc.Paths))
	for p := range doc.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		item := doc.Paths[p]
		for _, e := range []struct {
			verb string
			op   *openAPIOperation
		}{
			{"get", item.Get},
			{"put", item.Put},
			{"post", item.Post},
			{"patch", item.Patch},
			{"delete", item.Delete},
		} {
			if e.op == nil {
				continue
			}
			svc, method := splitOperationID(e.op.OperationID, e.op.Tags)
			ops = append(ops, operation{
				Service:    svc,
				Method:     method,
				AIPMethod:  e.op.XAIPMethod,
				HTTPMethod: e.verb,
				Path:       p,
			})
		}
	}
	return doc.Info.Title, ops, nil
}

// splitOperationID turns "WidgetService_ListWidgets" into ("WidgetService",
// "ListWidgets"). Falls back to the first tag as the service when there is no
// underscore.
func splitOperationID(id string, tags []string) (service, method string) {
	if i := strings.Index(id, "_"); i >= 0 {
		return id[:i], id[i+1:]
	}
	if len(tags) > 0 {
		return tags[0], id
	}
	return "", id
}

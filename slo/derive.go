package slo

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// Transport selects the OTel RED instrument the derived SLIs measure.
const (
	TransportGRPC = "grpc"
	TransportHTTP = "http"
)

// signalFor returns the OTel duration metric base name for a transport (the SDK
// default new-semconv instruments).
func signalFor(transport string) string {
	if transport == TransportHTTP {
		return "http.server.request.duration"
	}
	return "rpc.server.call.duration"
}

// DeriveOptions tune the generated defaults. The zero value is not valid; use
// DefaultDeriveOptions and adjust.
type DeriveOptions struct {
	Transport string // grpc (default) or http

	// SignalOverride, when set, replaces the transport's default OTel metric base
	// name — e.g. rpc.server.duration for the OTEL_SEMCONV_STABILITY_OPT_IN legacy
	// path. Empty uses signalFor(Transport).
	SignalOverride string

	// Window is the SLO measurement window. 28d rolling by default.
	Window TimeWindow

	// Objective targets (un-calibrated placeholders, marked as such).
	ReadAvailabilityTarget  float64
	WriteAvailabilityTarget float64
	ReadLatencyTarget       float64
	WriteLatencyTarget      float64

	// Latency thresholds in seconds, placed on histogram bucket boundaries.
	ReadLatencyThresholdSeconds  float64
	WriteLatencyThresholdSeconds float64

	// Status sets. Defaults are the gRPC new-semconv canonical strings.
	ServerFaultStatuses []string
	ClientFaultStatuses []string
}

// DefaultDeriveOptions returns the WS-025 defaults: 28d rolling window, 99.9%
// availability, 99% latency under 250ms (read) / 500ms (write) — all
// un-calibrated placeholders — and the gRPC good/valid status split (D4).
func DefaultDeriveOptions() DeriveOptions {
	return DeriveOptions{
		Transport:                    TransportGRPC,
		Window:                       TimeWindow{Duration: "28d", IsRolling: true},
		ReadAvailabilityTarget:       0.999,
		WriteAvailabilityTarget:      0.999,
		ReadLatencyTarget:            0.99,
		WriteLatencyTarget:           0.99,
		ReadLatencyThresholdSeconds:  0.25,
		WriteLatencyThresholdSeconds: 0.5,
		ServerFaultStatuses:          append([]string(nil), GRPCServerFaultStatuses...),
		ClientFaultStatuses:          append([]string(nil), GRPCClientFaultStatuses...),
	}
}

// ResourceDefaults describes a resource well enough to emit its standard AIP
// method SLOs without an OpenAPI doc (the scaffold path). The caller sets the
// method-set flags to match the service's ACTUAL proto — a service with no
// BatchGet or Undelete RPC leaves them off, so the day-one output matches what
// DefaultsFromOpenAPI produces for the same service.
type ResourceDefaults struct {
	// ServiceShort is the gRPC service short name (e.g. "OrderService"), the same
	// value DefaultsFromOpenAPI reads from the operation-id prefix. It drives the
	// object-name slug, so both paths must pass the same string.
	ServiceShort string
	// ServiceLabel is the value of the rpc.service label (proto FQN), e.g.
	// orders.v1.OrderService. Optional; empty filters by method only.
	ServiceLabel string
	// Resource is the PascalCase resource, e.g. "Order".
	Resource string
	// ResourcePlural is the PascalCase plural, e.g. "Orders".
	ResourcePlural string
	// IncludeBatchGet adds BatchGet<Plural> to the read group. Set it only when
	// the proto actually serves a BatchGet RPC.
	IncludeBatchGet bool
	// SoftDelete adds Undelete<Resource> to the write group. Set it only when the
	// proto actually serves an Undelete RPC.
	SoftDelete bool
}

// DefaultsForResource emits the four grouped default SLOs from a resource's
// standard AIP method names — no OpenAPI required, so the scaffold can write a
// good slo.yaml on day one. The read group is {Get, List} (+ BatchGet when
// IncludeBatchGet); the write group is {Create, Update, Delete} (+ Undelete when
// SoftDelete). Method names are sorted and the slug derives from ServiceShort, so
// the output is byte-identical to DefaultsFromOpenAPI for the same service.
func DefaultsForResource(rd ResourceDefaults, opts DeriveOptions) (*Document, error) {
	if rd.Resource == "" {
		return nil, fmt.Errorf("slo: DefaultsForResource: empty resource")
	}
	plural := rd.ResourcePlural
	if plural == "" {
		plural = rd.Resource + "s"
	}
	read := []string{"Get" + rd.Resource, "List" + plural}
	if rd.IncludeBatchGet {
		read = append(read, "BatchGet"+plural)
	}
	write := []string{"Create" + rd.Resource, "Update" + rd.Resource, "Delete" + rd.Resource}
	if rd.SoftDelete {
		write = append(write, "Undelete"+rd.Resource)
	}
	// Sort so the method regex + methods list match DefaultsFromOpenAPI's
	// keysSorted ordering exactly (no drift between the two derivation paths).
	sort.Strings(read)
	sort.Strings(write)
	short := rd.ServiceShort
	if short == "" {
		short = strings.ToLower(rd.Resource)
	}
	doc := &Document{}
	buildServiceSLOs(doc, short, rd.ServiceLabel, read, write, opts)
	return doc, nil
}

// DefaultsFromOpenAPI enumerates the operations in an enriched OpenAPI doc,
// groups them by AIP method class, and emits the grouped default SLOs. When the
// doc contains exactly one service and serviceLabel is non-empty, that label is
// applied (the rpc.service filter); otherwise the queries filter by method only.
func DefaultsFromOpenAPI(data []byte, serviceLabel string, opts DeriveOptions) (*Document, error) {
	_, ops, err := parseOpenAPI(data)
	if err != nil {
		return nil, err
	}
	// Group read/write method names per short service, deterministically.
	type group struct{ read, write map[string]bool }
	groups := map[string]*group{}
	var order []string
	for _, op := range ops {
		g := groups[op.Service]
		if g == nil {
			g = &group{read: map[string]bool{}, write: map[string]bool{}}
			groups[op.Service] = g
			order = append(order, op.Service)
		}
		switch {
		case readMethods[op.AIPMethod]:
			g.read[op.Method] = true
		case writeMethods[op.AIPMethod]:
			g.write[op.Method] = true
		}
	}
	sort.Strings(order)
	if len(order) == 0 {
		return nil, fmt.Errorf("slo: no classifiable operations in OpenAPI (need x-aip-method annotations from an enriched spec)")
	}
	doc := &Document{}
	for _, svc := range order {
		g := groups[svc]
		label := ""
		if len(order) == 1 {
			label = serviceLabel
		}
		buildServiceSLOs(doc, svc, label, keysSorted(g.read), keysSorted(g.write), opts)
	}
	return doc, nil
}

// buildServiceSLOs appends one Service + four SLIs/SLOs + one AlertPolicy + three
// AlertConditions for a service to doc.
func buildServiceSLOs(doc *Document, serviceShort, serviceLabel string, read, write []string, opts DeriveOptions) {
	slug := slugify(serviceShort)
	transport := opts.Transport
	if transport == "" {
		transport = TransportGRPC
	}
	signal := opts.SignalOverride
	if signal == "" {
		signal = signalFor(transport)
	}

	doc.Services = append(doc.Services, Service{
		APIVersion: APIVersion, Kind: KindService,
		Metadata: Metadata{Name: slug},
		Spec:     ServiceSpec{Description: fmt.Sprintf("Reliability targets for the %s service (derived defaults — calibrate before paging).", serviceShort)},
	})

	policyName := slug + "-burn-rate"

	// availability + latency SLOs for each non-empty method group.
	add := func(class string, methods []string) {
		if len(methods) == 0 {
			return
		}
		availTarget, latTarget, latThresh := opts.ReadAvailabilityTarget, opts.ReadLatencyTarget, opts.ReadLatencyThresholdSeconds
		if class == "write" {
			availTarget, latTarget, latThresh = opts.WriteAvailabilityTarget, opts.WriteLatencyTarget, opts.WriteLatencyThresholdSeconds
		}
		// Availability SLI: good = valid − server-fault; valid = all − client-fault.
		clientFault := opts.ClientFaultStatuses
		bothFault := append(append([]string(nil), opts.ClientFaultStatuses...), opts.ServerFaultStatuses...)
		availName := fmt.Sprintf("%s-%s-availability", slug, class)
		doc.SLIs = append(doc.SLIs, SLI{
			APIVersion: APIVersion, Kind: KindSLI,
			Metadata: Metadata{Name: availName},
			Spec: SLISpec{
				Description: fmt.Sprintf("%s %s availability: server-fault responses over valid requests (client faults excluded).", serviceShort, class),
				RatioMetric: &RatioMetric{
					Counter: true,
					Good:    otelSource(SLITypeAvailability, signal, transport, serviceLabel, methods, class, bothFault, 0),
					Total:   otelSource(SLITypeAvailability, signal, transport, serviceLabel, methods, class, clientFault, 0),
				},
			},
		})
		doc.SLOs = append(doc.SLOs, newSLO(availName, slug, class, "availability", serviceShort, availTarget, opts.Window, policyName))

		// Latency SLI: good = valid requests under threshold; valid = all − client-fault.
		latName := fmt.Sprintf("%s-%s-latency", slug, class)
		doc.SLIs = append(doc.SLIs, SLI{
			APIVersion: APIVersion, Kind: KindSLI,
			Metadata: Metadata{Name: latName},
			Spec: SLISpec{
				Description: fmt.Sprintf("%s %s latency: fraction of valid requests completing within %gs.", serviceShort, class, latThresh),
				RatioMetric: &RatioMetric{
					Counter: true,
					Good:    otelSource(SLITypeLatency, signal, transport, serviceLabel, methods, class, clientFault, latThresh),
					Total:   otelSource(SLITypeLatency, signal, transport, serviceLabel, methods, class, clientFault, 0),
				},
			},
		})
		doc.SLOs = append(doc.SLOs, newSLO(latName, slug, class, "latency", serviceShort, latTarget, opts.Window, policyName))
	}
	add("read", read)
	add("write", write)

	// One AlertPolicy + three shared burn-rate conditions (SRE Workbook MWMBR).
	doc.AlertPolicies = append(doc.AlertPolicies, AlertPolicy{
		APIVersion: APIVersion, Kind: KindAlertPolicy,
		Metadata: Metadata{Name: policyName},
		Spec: AlertPolicySpec{
			Description:        "Multi-window multi-burn-rate error-budget policy (SRE Workbook ch. 5).",
			AlertWhenBreaching: true,
			Conditions: []AlertPolicyCondition{
				{ConditionRef: policyName + "-fast"},
				{ConditionRef: policyName + "-medium"},
				{ConditionRef: policyName + "-slow"},
			},
		},
	})
	doc.AlertConditions = append(doc.AlertConditions,
		burnCondition(policyName+"-fast", "page", 14.4, "1h", "5m"),
		burnCondition(policyName+"-medium", "page", 6, "6h", "30m"),
		burnCondition(policyName+"-slow", "ticket", 1, "3d", "6h"),
	)
}

func otelSource(sliType, signal, transport, serviceLabel string, methods []string, class string, exclude []string, latency float64) MetricSource {
	return MetricSource{
		Type: MetricSourceTypeOTel,
		Spec: OTelRatioSource{
			SLIType:                 sliType,
			Signal:                  signal,
			Transport:               transport,
			Service:                 serviceLabel,
			Methods:                 methods,
			MethodClass:             class,
			ExcludeStatuses:         exclude,
			LatencyThresholdSeconds: latency,
		},
	}
}

func newSLO(indicator, slug, class, sliType, serviceShort string, target float64, window TimeWindow, policy string) SLO {
	return SLO{
		APIVersion: APIVersion, Kind: KindSLO,
		Metadata: Metadata{
			Name: indicator,
			Labels: map[string]string{
				"devedge.io/sli-type":     sliType,
				"devedge.io/method-class": class,
				"devedge.io/service":      slug,
			},
			Annotations: map[string]string{
				"devedge.io/error-budget-policy": "TODO: define the consequence when this budget is spent (e.g. freeze feature releases for " + slug + " until the budget recovers over a full window).",
				"devedge.io/layer":               "service",
				"devedge.io/uncalibrated":        "true",
			},
		},
		Spec: SLOSpec{
			Description:     fmt.Sprintf("%s %s %s objective (un-calibrated default — set from a measured baseline before it pages).", serviceShort, class, sliType),
			Service:         slug,
			IndicatorRef:    indicator,
			BudgetingMethod: "Occurrences",
			TimeWindow:      []TimeWindow{window},
			Objectives:      []Objective{{DisplayName: fmt.Sprintf("%s %s", class, sliType), Target: target}},
			AlertPolicies:   []string{policy},
		},
	}
}

func burnCondition(name, severity string, burn float64, long, short string) AlertCondition {
	return AlertCondition{
		APIVersion: APIVersion, Kind: KindAlertCondition,
		Metadata: Metadata{Name: name},
		Spec: AlertConditionSpec{
			Description: fmt.Sprintf("Burn rate %g over %s and %s.", burn, long, short),
			Severity:    severity,
			Condition: AlertConditionDetail{
				Kind:           "burnrate",
				Op:             "gt",
				Threshold:      burn,
				LookbackWindow: long,
				ShortWindow:    short,
			},
		},
	}
}

func keysSorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// slugify lowercases and hyphenates a CamelCase or spaced name.
func slugify(s string) string {
	var b strings.Builder
	prevLower := false
	for i, r := range s {
		switch {
		case r == ' ' || r == '_' || r == '-':
			b.WriteByte('-')
			prevLower = false
		case unicode.IsUpper(r):
			if i > 0 && prevLower {
				b.WriteByte('-')
			}
			b.WriteRune(unicode.ToLower(r))
			prevLower = false
		default:
			b.WriteRune(r)
			prevLower = unicode.IsLower(r) || unicode.IsDigit(r)
		}
	}
	return strings.Trim(b.String(), "-")
}

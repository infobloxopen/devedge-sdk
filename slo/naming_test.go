package slo

import "testing"

func TestNamingSeries(t *testing.T) {
	n := DefaultGRPCNaming()
	if got := n.series("rpc.server.call.duration", "_count"); got != "rpc_server_call_duration_seconds_count" {
		t.Errorf("count series = %q", got)
	}
	if got := n.series("rpc.server.call.duration", "_bucket"); got != "rpc_server_call_duration_seconds_bucket" {
		t.Errorf("bucket series = %q", got)
	}
	l := LegacyGRPCNaming()
	if got := l.series("rpc.server.duration", "_count"); got != "rpc_server_duration_milliseconds_count" {
		t.Errorf("legacy series = %q", got)
	}
	h := HTTPGatewayNaming()
	if got := h.series("http.server.request.duration", "_bucket"); got != "http_server_request_duration_seconds_bucket" {
		t.Errorf("http series = %q", got)
	}
}

func TestSelectorMatchers(t *testing.T) {
	got := selector(
		eqMatcher("rpc_service", "toy.v1.WidgetService"),
		reMatcher("rpc_method", []string{"GetWidget", "ListWidgets"}),
		notReMatcher("rpc_response_status_code", []string{"NOT_FOUND"}),
		"",
	)
	want := `{rpc_service="toy.v1.WidgetService", rpc_method=~"GetWidget|ListWidgets", rpc_response_status_code!~"NOT_FOUND"}`
	if got != want {
		t.Errorf("selector = %q, want %q", got, want)
	}
	// Empty service label drops the matcher.
	if got := selector(eqMatcher("", "x"), reMatcher("rpc_method", []string{"A"})); got != `{rpc_method=~"A"}` {
		t.Errorf("empty label not dropped: %q", got)
	}
}

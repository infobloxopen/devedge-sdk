package apilayout

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in      string
		want    Layout
		wantErr bool
	}{
		{"", Default, false},
		{"product-rest", ProductREST, false},
		{"k8s-apis", K8sAPIs, false},
		{"nope", "", true},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("Parse(%q): want error, got %q", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("Parse(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	if Default != ProductREST {
		t.Errorf("Default = %q; want product-rest", Default)
	}
}

func TestPrefix(t *testing.T) {
	if ProductREST.Prefix() != "/api" {
		t.Errorf("product-rest prefix = %q", ProductREST.Prefix())
	}
	if K8sAPIs.Prefix() != "/apis" {
		t.Errorf("k8s-apis prefix = %q", K8sAPIs.Prefix())
	}
}

func TestCollectionPath(t *testing.T) {
	cases := []struct {
		name   string
		layout Layout
		res    Resource
		want   string
	}{
		{
			name:   "product-rest",
			layout: ProductREST,
			res:    Resource{Domain: "ipam", Version: "v1", Resource: "ip-spaces"},
			want:   "/api/ipam/v1/ip-spaces",
		},
		{
			name:   "product-rest beta version",
			layout: ProductREST,
			res:    Resource{Domain: "dns", Version: "v1beta1", Resource: "zones"},
			want:   "/api/dns/v1beta1/zones",
		},
		{
			name:   "k8s-apis",
			layout: K8sAPIs,
			res:    Resource{Group: "ipam.infoblox.com", Version: "v1", Resource: "ip-spaces"},
			want:   "/apis/ipam.infoblox.com/v1/ip-spaces",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.layout.CollectionPath(c.res)
			if err != nil {
				t.Fatalf("CollectionPath: %v", err)
			}
			if got != c.want {
				t.Errorf("CollectionPath = %q; want %q", got, c.want)
			}
		})
	}
}

func TestItemPath(t *testing.T) {
	got, err := ProductREST.ItemPath(Resource{Domain: "ipam", Version: "v1", Resource: "ip-spaces"}, "{id}")
	if err != nil {
		t.Fatalf("ItemPath: %v", err)
	}
	if got != "/api/ipam/v1/ip-spaces/{id}" {
		t.Errorf("ItemPath = %q", got)
	}
	got, err = K8sAPIs.ItemPath(Resource{Group: "dns.infoblox.com", Version: "v1", Resource: "zones"}, "prod")
	if err != nil {
		t.Fatalf("ItemPath: %v", err)
	}
	if got != "/apis/dns.infoblox.com/v1/zones/prod" {
		t.Errorf("ItemPath = %q", got)
	}
	if _, err := ProductREST.ItemPath(Resource{Domain: "ipam", Version: "v1", Resource: "ip-spaces"}, ""); err == nil {
		t.Error("ItemPath with empty idParam: want error")
	}
}

func TestValidation(t *testing.T) {
	bad := []struct {
		name   string
		layout Layout
		res    Resource
	}{
		{"product-rest missing domain", ProductREST, Resource{Version: "v1", Resource: "ip-spaces"}},
		{"k8s missing group", K8sAPIs, Resource{Version: "v1", Resource: "ip-spaces"}},
		{"k8s non-dotted group", K8sAPIs, Resource{Group: "ipam", Version: "v1", Resource: "ip-spaces"}},
		{"bad version", ProductREST, Resource{Domain: "ipam", Version: "1", Resource: "ip-spaces"}},
		{"version-ish after resource rejected as bad version", ProductREST, Resource{Domain: "ipam", Version: "ip-spaces", Resource: "v1"}},
		{"upper-case resource", ProductREST, Resource{Domain: "ipam", Version: "v1", Resource: "IpSpaces"}},
		{"empty resource", ProductREST, Resource{Domain: "ipam", Version: "v1"}},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if _, err := c.layout.CollectionPath(c.res); err == nil {
				t.Errorf("CollectionPath(%+v): want error", c.res)
			}
		})
	}
}

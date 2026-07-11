package gormtx

import "testing"

// TestQuoteIdent_RejectsUnsafe covers the identifier guard that protects every raw-SQL DDL
// path (the WS-043 tuning/partition DDL AND the MigrateModule CREATE SCHEMA, both of which
// now route through quoteIdent instead of Go %q) from injection via a hostile schema/prefix.
func TestQuoteIdent_RejectsUnsafe(t *testing.T) {
	safe := []string{"idempotency_keys", "orders", "m1", "_x", "idempotency_keys_p0", "Ord_Schema"}
	for _, s := range safe {
		if got, err := quoteIdent(s); err != nil || got != `"`+s+`"` {
			t.Fatalf("quoteIdent(%q) = (%q, %v), want (%q, nil)", s, got, err, `"`+s+`"`)
		}
	}
	unsafe := []string{
		`foo"; DROP TABLE x; --`, // quote-break
		`foo bar`,                // space
		`foo.bar`,                // dot
		`foo;bar`,                // statement sep
		`"quoted"`,               // embedded quotes
		"",                       // empty
		"1abc",                   // digit-leading
		"schema-name",            // hyphen
		"tab\tname",              // control char
	}
	for _, s := range unsafe {
		if _, err := quoteIdent(s); err == nil {
			t.Fatalf("quoteIdent(%q) must reject an unsafe identifier", s)
		}
	}
}

// TestQuoteQualified covers the schema.table split + quoting used by the partition DDL.
func TestQuoteQualified(t *testing.T) {
	cases := map[string]string{
		"idempotency_keys":         `"idempotency_keys"`,
		"orders.idempotency_keys":  `"orders"."idempotency_keys"`,
		"ord_idempotency_keys":     `"ord_idempotency_keys"`,
	}
	for in, want := range cases {
		got, err := quoteQualified(in)
		if err != nil || got != want {
			t.Fatalf("quoteQualified(%q) = (%q, %v), want (%q, nil)", in, got, err, want)
		}
	}
	// A hostile schema part is rejected.
	if _, err := quoteQualified(`ev"il.idempotency_keys`); err == nil {
		t.Fatal("quoteQualified must reject an unsafe schema part")
	}
}

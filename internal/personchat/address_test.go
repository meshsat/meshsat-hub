package personchat

import "testing"

func TestParseAddressed(t *testing.T) {
	cases := []struct {
		in       string
		ok       bool
		kind     Kind
		value    string
		wantBody string
	}{
		{"@+31612345678 water at the gate", true, KindSMS, "+31612345678", "water at the gate"},
		{"  @+31612345678: water", true, KindSMS, "+31612345678", "water"},
		{"@+31612345678:water", true, KindSMS, "+31612345678", "water"},
		{"@ops@Example.ORG need fuel\nline two", true, KindEmail, "ops@example.org", "need fuel\nline two"},
		{"@OpsDesk on my way", true, KindName, "OpsDesk", "on my way"},
		{"@bob", false, "", "", ""},                            // no body
		{"@+31612345678 ", false, "", "", ""},                  // no body
		{"@ +31612345678 hi", false, "", "", ""},               // space after @
		{"@+3161234567x hi", false, "", "", ""},                // not digits
		{"@+0612345678 hi", false, "", "", ""},                 // leading zero
		{"@+123456 hi", false, "", "", ""},                     // too short
		{"@+1234567890123456 hi", false, "", "", ""},           // too long
		{"@0612345678 hi", true, KindName, "0612345678", "hi"}, // a name: resolves only if a contact has it
		{"@a@b@c.org hi", false, "", "", ""},
		{"@a@localhost hi", false, "", "", ""}, // no dot in domain
		{"@\"a b\"@c.org hi", false, "", "", ""},
		{"@a<b>@c.org hi", false, "", "", ""},
		{"@bob! hi", false, "", "", ""},            // not a name
		{"hello @+31612345678", false, "", "", ""}, // not first
		{"*K7 on my way", false, "", "", ""},
		{"#A7 hi", false, "", "", ""},
	}
	for _, c := range cases {
		target, body, ok := ParseAddressed(c.in)
		if ok != c.ok {
			t.Errorf("%q: ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if target.Kind != c.kind || target.Value != c.value || body != c.wantBody {
			t.Errorf("%q = (%s %q, %q), want (%s %q, %q)", c.in, target.Kind, target.Value, body, c.kind, c.value, c.wantBody)
		}
	}
}

func TestNormaliseEmail(t *testing.T) {
	cases := map[string]string{
		"Ops Desk <ops@Example.org>": "ops@example.org",
		"ops@EXAMPLE.ORG":            "ops@example.org",
		"<Ops@example.org>":          "Ops@example.org",
	}
	for in, want := range cases {
		got, ok := NormaliseEmail(in)
		if !ok || got != want {
			t.Errorf("NormaliseEmail(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "not an address", "a@b", "x@y.org, z@w.org"} {
		if got, ok := NormaliseEmail(bad); ok {
			t.Errorf("NormaliseEmail(%q) = %q, want refused", bad, got)
		}
	}
}

func TestNameKey(t *testing.T) {
	if NameKey(" Ops  Desk ") != "opsdesk" || NameKey("OPSDESK") != "opsdesk" {
		t.Fatal("NameKey must drop spaces and fold case")
	}
}

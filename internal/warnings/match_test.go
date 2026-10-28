package warnings

import (
	"reflect"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
)

func account(fields map[string]string) *integrations.Account {
	return &integrations.Account{Provider: integrations.ProviderWarnings, TenantID: "t1", Fields: fields}
}

func TestParseTenantConfig(t *testing.T) {
	cfg, err := ParseTenantConfig(account(map[string]string{
		"enabled": "true", "satellite": "no",
		"areas":     "EMMA_ID:NL804\n52.37,4.90,25\r\n501000005\n",
		"languages": "nl, en",
		"feeds":     "meteoalarm-nl",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || cfg.Satellite {
		t.Fatalf("switches: %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.Geocodes, []string{"EMMA_ID:NL804", "501000005"}) {
		t.Fatalf("geocodes: %v", cfg.Geocodes)
	}
	if len(cfg.Circles) != 1 || cfg.Circles[0].RadiusKm != 25 || cfg.Circles[0].Centre.Lat != 52.37 {
		t.Fatalf("circles: %+v", cfg.Circles)
	}
	if !reflect.DeepEqual(cfg.Languages, []string{"nl", "en"}) || !reflect.DeepEqual(cfg.Feeds, []string{"meteoalarm-nl"}) {
		t.Fatalf("lists: %+v", cfg)
	}
	if cfg.TakesFeed("dwd") || !cfg.TakesFeed("meteoalarm-nl") {
		t.Fatal("feeds filter")
	}
	for _, bad := range []map[string]string{
		{"enabled": "true", "feeds": "nina"},
		{"enabled": "true", "areas": "ünïcode"},
		{"enabled": "true", "areas": "52.37,4.90"},
	} {
		if _, err := ParseTenantConfig(account(bad)); err == nil {
			t.Errorf("%v: accepted", bad)
		}
	}
	if cfg, _ := ParseTenantConfig(account(map[string]string{})); cfg.Enabled || !cfg.TakesFeed("dwd") {
		t.Fatal("an empty account is off and takes every feed")
	}
}

func TestMatchTenant(t *testing.T) {
	a, err := ParseCAP(fixture(t, "meteoalarm-nl-texel.cap.xml")) // polygon around Texel, EMMA_ID:NL804
	if err != nil {
		t.Fatal(err)
	}
	dwd, err := ParseCAP(fixture(t, "dwd-boeen.cap.xml")) // WARNCELLID geocodes, no polygon
	if err != nil {
		t.Fatal(err)
	}
	onTexel := &store.Bridge{BridgeID: "texel", LocationLat: 53.05, LocationLon: 4.80}
	inLeiden := &store.Bridge{BridgeID: "leiden", LocationLat: 52.16, LocationLon: 4.49}
	noFix := &store.Bridge{BridgeID: "nofix"}
	bridges := []*store.Bridge{onTexel, inLeiden, noFix}

	cases := []struct {
		name   string
		alert  *Alert
		fields map[string]string
		want   []string
	}{
		{"off", a, map[string]string{"enabled": "false", "areas": "NL804"}, nil},
		{"geocode by value sends to every bridge", a, map[string]string{"enabled": "true", "areas": "NL804"}, []string{"leiden", "nofix", "texel"}},
		{"geocode by name and value", a, map[string]string{"enabled": "true", "areas": "EMMA_ID:NL804"}, []string{"leiden", "nofix", "texel"}},
		{"another name does not match, only the kit on Texel", a, map[string]string{"enabled": "true", "areas": "WARNCELLID:NL804"}, []string{"texel"}},
		{"bridge position in the polygon", a, map[string]string{"enabled": "true"}, []string{"texel"}},
		{"circle touching the polygon", a, map[string]string{"enabled": "true", "areas": "52.90,4.75,5"}, []string{"leiden", "nofix", "texel"}},
		{"circle far away", a, map[string]string{"enabled": "true", "areas": "51.0,5.9,5"}, []string{"texel"}},
		{"dwd cell", dwd, map[string]string{"enabled": "true", "areas": "501000002"}, []string{"leiden", "nofix", "texel"}},
		{"dwd without a polygon matches no position", dwd, map[string]string{"enabled": "true"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := ParseTenantConfig(account(c.fields))
			if err != nil {
				t.Fatal(err)
			}
			m := MatchTenant(c.alert, cfg, bridges)
			if !reflect.DeepEqual(m.Bridges, c.want) {
				t.Fatalf("bridges %v, want %v (why %v)", m.Bridges, c.want, m.Why)
			}
		})
	}
}

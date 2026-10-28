package warnings

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// MESHSAT-1734. The victim of every isolation test is store.DefaultTenantID.

type sent struct {
	topic   string
	qos     byte
	payload []byte
}

type fakeBus struct {
	mu   sync.Mutex
	sent []sent
	fail error
}

func (b *fakeBus) Publish(topic string, qos byte, _ bool, payload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail != nil {
		return b.fail
	}
	b.sent = append(b.sent, sent{topic, qos, append([]byte(nil), payload...)})
	return nil
}

func (b *fakeBus) published() []sent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]sent(nil), b.sent...)
}

// fakeAccounts is a tenant -> fields map.
type fakeAccounts map[string]map[string]string

func (f fakeAccounts) TenantsWith(_ context.Context, provider string) ([]string, error) {
	var out []string
	for tid := range f {
		if provider == integrations.ProviderWarnings {
			out = append(out, tid)
		}
	}
	return out, nil
}

func (f fakeAccounts) ForTenant(_ context.Context, tenantID, provider string) (*integrations.Account, error) {
	fields, ok := f[tenantID]
	if !ok {
		return nil, nil
	}
	return &integrations.Account{Provider: provider, TenantID: tenantID, Fields: fields}, nil
}

type fakeFeed struct {
	source int
	msgs   []Fetched
	err    error
	polls  int
}

func (f *fakeFeed) Source() int { return f.source }
func (f *fakeFeed) Fetch(context.Context, func(string, time.Time) bool) ([]Fetched, error) {
	f.polls++
	return f.msgs, f.err
}

type satCall struct {
	tenant, imei string
	wire         []byte
}

func rig(t *testing.T, accounts fakeAccounts, feeds ...Feed) (*Service, *fakeBus, store.Store, *[]satCall) {
	t.Helper()
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for tenant, bridges := range map[string][]*store.Bridge{
		"t1":                  {{BridgeID: "texel", LocationLat: 53.05, LocationLon: 4.80}, {BridgeID: "leiden", LocationLat: 52.16, LocationLon: 4.49}},
		store.DefaultTenantID: {{BridgeID: "kit-v", LocationLat: 53.05, LocationLon: 4.80}},
	} {
		_ = db.CreateTenant(ctx, &store.Tenant{ID: tenant, Slug: tenant, Name: tenant, Status: store.TenantActive})
		for _, b := range bridges {
			if err := db.CreateOrUpdateBridge(ctx, tenant, b); err != nil {
				t.Fatal(err)
			}
		}
	}
	// t1's modem, on bridge texel.
	if err := db.CreateDevice(ctx, "t1", &store.Device{IMEI: "300234067500220", Type: "rockblock"}); err != nil {
		t.Fatal(err)
	}
	if err := db.AssociateDeviceWithBridge(ctx, "t1", "300234067500220", "texel"); err != nil {
		t.Fatal(err)
	}
	bus := &fakeBus{}
	var sats []satCall
	var mu sync.Mutex
	sat := func(_ context.Context, tenant, imei string, wire []byte) error {
		mu.Lock()
		defer mu.Unlock()
		sats = append(sats, satCall{tenant, imei, wire})
		return nil
	}
	svc := New(db, bus, accounts, newTestSigner(t), sat, feeds, time.Minute, nil)
	return svc, bus, db, &sats
}

func TestPollDeliversOnceToEachMatchingKit(t *testing.T) {
	doc := fixture(t, "meteoalarm-nl-texel.cap.xml")
	a, _ := ParseCAP(doc)
	feed := &fakeFeed{source: SourceMeteoAlarmNL, msgs: []Fetched{{Alert: a, Raw: doc}}}
	accounts := fakeAccounts{
		"t1":                  {"enabled": "true", "languages": "nl"},
		store.DefaultTenantID: {"enabled": "false"},
	}
	svc, bus, db, sats := rig(t, accounts, feed)
	ctx := context.Background()

	svc.Poll(ctx)
	got := bus.published()
	if len(got) != 1 || got[0].topic != "meshsat/t1/bridge/texel/warn/in" || got[0].qos != 1 {
		t.Fatalf("published: %+v", got)
	}
	d, err := Verify(svc.PublicKey(), got[0].payload)
	if err != nil {
		t.Fatalf("the published digest does not verify: %v", err)
	}
	if d.Language != "nl" || d.MsgType != TypeUpdate || d.Source != SourceMeteoAlarmNL || len(got[0].payload) > MaxDigestBytes {
		t.Fatalf("digest: %+v (%d bytes)", d, len(got[0].payload))
	}
	if len(*sats) != 0 {
		t.Fatal("satellite is off by default")
	}
	// The second poll sees the same message: nothing new, nothing sent.
	svc.Poll(ctx)
	if n := len(bus.published()); n != 1 {
		t.Fatalf("a re-poll sent again: %d", n)
	}
	// The match is listed for t1 and not for the victim.
	list, err := db.ListTenantWarnings(ctx, "t1", 10)
	if err != nil || len(list) != 1 || list[0].Bridges != `["texel"]` || list[0].Event != "Moderate Wind" {
		t.Fatalf("t1's list: %+v %v", list, err)
	}
	if v, _ := db.ListTenantWarnings(ctx, store.DefaultTenantID, 10); len(v) != 0 {
		t.Fatalf("the victim got a match: %+v", v)
	}
	// Another replica (a second Service on the same store) processing the
	// same message delivers nothing: the claim is taken.
	bus2 := &fakeBus{}
	svc2 := New(db, bus2, accounts, newTestSigner(t), nil, nil, time.Minute, nil)
	w, _ := db.GetWarning(ctx, "meteoalarm-nl", a.Identifier)
	svc2.process(ctx, a, w, "")
	if n := len(bus2.published()); n != 0 {
		t.Fatalf("the other replica sent %d", n)
	}
}

func TestSatelliteGoesToTheKitsModemWhenTheTenantPays(t *testing.T) {
	doc := fixture(t, "meteoalarm-nl-texel.cap.xml")
	a, _ := ParseCAP(doc)
	feed := &fakeFeed{source: SourceMeteoAlarmNL, msgs: []Fetched{{Alert: a, Raw: doc}}}
	svc, bus, _, sats := rig(t, fakeAccounts{"t1": {"enabled": "true", "satellite": "true", "areas": "NL804"}}, feed)
	svc.Poll(context.Background())
	// Both of t1's bridges match by geocode; only texel has a modem.
	if n := len(bus.published()); n != 2 {
		t.Fatalf("mqtt sends: %d", n)
	}
	if len(*sats) != 1 || (*sats)[0].imei != "300234067500220" || (*sats)[0].tenant != "t1" {
		t.Fatalf("satellite sends: %+v", *sats)
	}
	if string((*sats)[0].wire) != string(bus.published()[0].payload) && string((*sats)[0].wire) != string(bus.published()[1].payload) {
		t.Fatal("the satellite bytes are not the published digest")
	}
	if (*sats)[0].wire[0] != Magic {
		t.Fatalf("the MT does not start with the type byte: %#x", (*sats)[0].wire[0])
	}
}

func TestFeedFailureIsCountedAndTheOthersStillRun(t *testing.T) {
	doc := fixture(t, "dwd-boeen.cap.xml")
	a, _ := ParseCAP(doc)
	bad := &fakeFeed{source: SourceMeteoAlarmNL, err: errors.New("down")}
	good := &fakeFeed{source: SourceDWD, msgs: []Fetched{{Alert: a, Raw: doc}}}
	svc, bus, _, _ := rig(t, fakeAccounts{"t1": {"enabled": "true", "areas": "WARNCELLID:501000005", "feeds": "dwd"}}, bad, good)
	svc.Poll(context.Background())
	if good.polls != 1 || len(bus.published()) != 2 {
		t.Fatalf("polls %d, sends %d", good.polls, len(bus.published()))
	}
}

func TestATenantThatDoesNotTakeAFeedGetsNothingFromIt(t *testing.T) {
	doc := fixture(t, "dwd-boeen.cap.xml")
	a, _ := ParseCAP(doc)
	svc, bus, _, _ := rig(t, fakeAccounts{"t1": {"enabled": "true", "areas": "501000005", "feeds": "meteoalarm-nl"}},
		&fakeFeed{source: SourceDWD, msgs: []Fetched{{Alert: a, Raw: doc}}})
	svc.Poll(context.Background())
	if n := len(bus.published()); n != 0 {
		t.Fatalf("sent %d", n)
	}
}

const testCAP = `<?xml version="1.0" encoding="UTF-8"?>
<alert xmlns="urn:oasis:names:tc:emergency:cap:1.2">
  <identifier>meshsat-test-%s</identifier><sender>owner@example.org</sender><sent>2026-10-28T12:00:00+00:00</sent>
  <status>%s</status><msgType>Alert</msgType><scope>Public</scope>
  <info><language>nl</language><category>Met</category><event>Test</event><urgency>Immediate</urgency><severity>Severe</severity><certainty>Observed</certainty>
  <expires>2026-10-28T14:00:00+00:00</expires><senderName>MeshSat</senderName><headline>Relay test, no action needed</headline>
  <area><areaDesc>Texel</areaDesc><geocode><valueName>EMMA_ID</valueName><value>NL804</value></geocode></area></info></alert>`

func TestInjectRunsATestForOneTenantOnly(t *testing.T) {
	svc, bus, db, _ := rig(t, fakeAccounts{
		"t1":                  {"enabled": "true", "areas": "NL804"},
		store.DefaultTenantID: {"enabled": "true", "areas": "NL804"},
	})
	ctx := context.Background()
	res, err := svc.Inject(ctx, "t1", []byte(strings.ReplaceAll(strings.Replace(testCAP, "%s", "1", 1), "%s", "Test")))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deliveries) != 1 || res.Deliveries[0].TenantID != "t1" || len(res.Deliveries[0].Bridges) != 2 || res.Deliveries[0].Error != "" {
		t.Fatalf("result: %+v", res)
	}
	got := bus.published()
	if len(got) != 2 {
		t.Fatalf("sends: %+v", got)
	}
	for _, s := range got {
		if !strings.HasPrefix(s.topic, "meshsat/t1/bridge/") {
			t.Fatalf("sent outside t1: %s", s.topic)
		}
		d, err := Verify(svc.PublicKey(), s.payload)
		if err != nil || !d.Test || d.Source != SourceTest || d.Severity != 3 {
			t.Fatalf("digest: %+v %v", d, err)
		}
	}
	if res.Deliveries[0].Digest != hex.EncodeToString(got[0].payload) {
		t.Fatal("the answer does not show the digest sent")
	}
	if v, _ := db.ListTenantWarnings(ctx, store.DefaultTenantID, 10); len(v) != 0 {
		t.Fatalf("the victim got the test: %+v", v)
	}
	if list, _ := db.ListTenantWarnings(ctx, "t1", 10); len(list) != 1 || !list[0].Test || list[0].Source != "test" {
		t.Fatalf("t1's list: %+v", list)
	}
	// An Actual document is refused as a test.
	if _, err := svc.Inject(ctx, "t1", []byte(strings.ReplaceAll(strings.Replace(testCAP, "%s", "2", 1), "%s", "Actual"))); !errors.Is(err, ErrNotTest) {
		t.Fatalf("an Actual document: %v", err)
	}
	// Not CAP at all.
	if _, err := svc.Inject(ctx, "t1", []byte("<html/>")); !errors.Is(err, ErrNotCAP) {
		t.Fatalf("html: %v", err)
	}
	// A tenant with the relay off is told so, and gets nothing.
	svc2, bus2, _, _ := rig(t, fakeAccounts{"t1": {"enabled": "false", "areas": "NL804"}})
	res, err = svc2.Inject(ctx, "t1", []byte(strings.ReplaceAll(strings.Replace(testCAP, "%s", "3", 1), "%s", "Exercise")))
	if err != nil || len(res.Deliveries) != 1 || res.Deliveries[0].Error == "" || len(bus2.published()) != 0 {
		t.Fatalf("relay off: %+v %v", res, err)
	}
}

func TestAPublishFailureIsNotAMatchLost(t *testing.T) {
	doc := fixture(t, "meteoalarm-nl-texel.cap.xml")
	a, _ := ParseCAP(doc)
	svc, bus, db, _ := rig(t, fakeAccounts{"t1": {"enabled": "true", "areas": "NL804"}}, &fakeFeed{source: SourceMeteoAlarmNL, msgs: []Fetched{{Alert: a, Raw: doc}}})
	bus.fail = errors.New("broker down")
	svc.Poll(context.Background())
	list, err := db.ListTenantWarnings(context.Background(), "t1", 10)
	if err != nil || len(list) != 1 || !strings.Contains(list[0].Detail, `"bearers":{}`) {
		t.Fatalf("the match should be recorded with no bearer: %+v %v", list, err)
	}
}

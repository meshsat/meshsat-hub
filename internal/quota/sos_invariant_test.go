package quota_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/quota"
	"github.com/meshsat/meshsat-hub/internal/ratelimit"
	"github.com/meshsat/meshsat-hub/internal/rockblock"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// recordingBus captures what the ingest path published.
type recordingBus struct {
	mu     sync.Mutex
	topics []string
}

func (b *recordingBus) Connect() error { return nil }
func (b *recordingBus) Publish(topic string, _ byte, _ bool, _ []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.topics = append(b.topics, topic)
	return nil
}
func (b *recordingBus) PublishJSON(topic string, q byte, r bool, _ any) error {
	return b.Publish(topic, q, r, nil)
}
func (b *recordingBus) Subscribe(string, byte, bus.MessageHandler) error              { return nil }
func (b *recordingBus) QueueSubscribe(string, byte, string, bus.MessageHandler) error { return nil }
func (b *recordingBus) IsConnected() bool                                             { return true }
func (b *recordingBus) Disconnect()                                                   {}

func (b *recordingBus) published(substr string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, t := range b.topics {
		if strings.Contains(t, substr) {
			return true
		}
	}
	return false
}

// TestSOSSurvivesTheQuota is the one test in this package that must never be
// deleted or weakened.
//
// A subscription ceiling is a commercial mechanism. The traffic it governs
// includes a distress call from somebody in a place with no other way to ask
// for help. Refusing that call because an invoice lapsed is not an acceptable
// failure at any price, so the ceiling gates REGISTRATION and nothing else.
//
// The test puts a tenant well over its cap -- five devices on a four-device
// free plan, the state a downgrade or a re-priced tier produces -- and then
// drives the real inbound path: a RockBLOCK MO webhook carrying an SOS. The
// message must persist, the MQTT fan-out must happen, and the per-device rate
// limiter must still let the SOS through even with its budget exhausted.
//
// If this test fails, something moved the quota check into an ingest path.
// Move it back out. Do not adjust the test.
func TestSOSSurvivesTheQuota(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	const tenantID = "t-over-cap"
	now := time.Now().UTC()
	if err := db.CreateTenant(ctx, &store.Tenant{
		ID: tenantID, Slug: tenantID, Name: "Over Cap", Plan: plans.Free,
		Status: store.TenantActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("tenant: %v", err)
	}

	// Five devices on a plan that allows four: over the cap by one, exactly
	// what a downgrade leaves behind.
	imeis := []string{
		"300234065000001", "300234065000002", "300234065000003",
		"300234065000004", "300234065000005",
	}
	for _, imei := range imeis {
		if err := db.CreateDevice(ctx, tenantID, &store.Device{IMEI: imei, Label: imei, Type: "rockblock"}); err != nil {
			t.Fatalf("device %s: %v", imei, err)
		}
	}

	q := quota.New(db, func(context.Context, string) (string, error) { return plans.Free, nil })

	// Precondition: the tenant really is over its cap, so the rest of the test
	// is testing what it claims to test.
	if ok, _ := q.AllowAnother(ctx, tenantID); ok {
		t.Fatal("precondition failed: the tenant should be over its cap")
	}
	u, err := q.Usage(ctx, tenantID)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if !u.OverLimit || u.Used != 5 || u.Limit != 4 {
		t.Fatalf("precondition failed: usage = %+v", u)
	}

	// The SOS arrives on the device that put the tenant over.
	sosIMEI := imeis[4]
	mb := &recordingBus{}
	h := rockblock.NewHandler(mb, "test-secret")
	h.SetStore(db)
	h.SetTenants(tenancy.NewResolver(db, store.DefaultTenantID, time.Minute))

	form := url.Values{
		"imei":              {sosIMEI},
		"momsn":             {"1"},
		"transmit_time":     {"26-09-09 10:00:00"},
		"iridium_latitude":  {"52.3676"},
		"iridium_longitude": {"4.9041"},
		// "SOS" in hex; the payload does not matter to the invariant, only
		// that an over-cap tenant's traffic is carried at all.
		"data": {"534f53"},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/rockblock", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mac := hmac.New(sha256.New, []byte("test-secret"))
	mac.Write([]byte(form.Get("data")))
	req.Header.Set("X-Hub-Signature", hex.EncodeToString(mac.Sum(nil)))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("an over-cap tenant's MO was refused with %d: %s", rr.Code, rr.Body.String())
	}
	if !mb.published("/mo/") {
		t.Error("the MO was not published to the bus for an over-cap tenant")
	}
	msgs, err := db.ListMessages(ctx, tenantID, sosIMEI, 10)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(msgs) == 0 {
		t.Error("the MO of an over-cap tenant was not persisted")
	}

	// And the other half of the invariant, which predates this feature: a
	// device that has spent its entire rate-limit budget still gets an SOS out.
	rl := ratelimit.NewDeviceLimiter(1, 0, 1, 1, nil)
	for i := 0; i < 5; i++ {
		rl.Allow(sosIMEI, false)
	}
	if rl.Allow(sosIMEI, false) {
		t.Fatal("precondition failed: the rate limiter should be exhausted")
	}
	if !rl.Allow(sosIMEI, true) {
		t.Error("an SOS was dropped by the rate limiter")
	}
}

// TestQuotaIsNotOnAnyIngestPath is the structural half of the same promise.
//
// The behavioural test above proves one path carries an over-cap tenant's
// traffic. This one proves no package that receives field traffic reaches the
// subscription ceiling at all, so the next handler somebody adds cannot
// quietly acquire one.
//
// It reads direct imports and source text rather than the transitive
// dependency graph on purpose: some of these packages reach internal/api for
// its JSON helpers, which drags in the whole handler set including this
// package. A transitive check would go permanently red for a reason that has
// nothing to do with the invariant, and a permanently red test is a deleted
// test.
//
// internal/bridge is in the list even though it does gate one registration --
// a birth for a device nobody has seen. It asks through its own one-method
// interface rather than importing this package, and
// TestQuotaCheckOnlyOnDeviceBirth over there pins where the question is asked.
func TestQuotaIsNotOnAnyIngestPath(t *testing.T) {
	const self = "github.com/meshsat/meshsat-hub/internal/quota"
	ingest := []string{
		"rockblock",    // Iridium MO webhook
		"cloudloop",    // Cloudloop/LingoMO webhook + MT sender
		"globalstar",   // Globalstar webhook
		"sms",          // inbound SMS
		"email",        // inbound PGP email gateway
		"sos",          // SOS detection and escalation
		"deadman",      // dead man's switch
		"escalation",   // escalation chains
		"routing",      // the dispatcher
		"message",      // MO/MT subscriber
		"ratelimit",    // the only other thing allowed to refuse traffic
		"reticulum",    // off-Iridium bearer
		"oob",          // out-of-band commands
		"webhookroute", // tenant-addressed webhook paths
		"bridge",       // bridge MQTT ingest (asks via its own interface)
	}
	const why = "\nA subscription ceiling must never sit on a path that carries field traffic: " +
		"it would let a lapsed plan drop an SOS. Keep the check on registration only."

	for _, pkg := range ingest {
		dir := "../" + pkg

		out, err := exec.Command("go", "list", "-f", "{{join .Imports \"\\n\"}}", "./"+dir).Output()
		if err != nil {
			t.Fatalf("go list %s: %v", pkg, err)
		}
		for _, imp := range strings.Split(string(out), "\n") {
			if strings.TrimSpace(imp) == self {
				t.Errorf("internal/%s imports internal/quota."+why, pkg)
			}
		}

		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatalf("glob %s: %v", pkg, err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			// The bridge subscriber asks once, on the registration half of the
			// package; its own test pins that. Everything else asks never.
			if pkg == "bridge" && strings.HasSuffix(f, "subscriber.go") {
				continue
			}
			if strings.Contains(string(src), "AllowAnother(") {
				t.Errorf("%s calls AllowAnother."+why, f)
			}
		}
	}
}

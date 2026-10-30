package sms

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// twilioRecorder is a fake Twilio that records the From of every send and
// answers refuse(from) with a 400, else 201.
type twilioRecorder struct {
	mu     sync.Mutex
	sends  [][2]string // {To, From}
	refuse func(from string) int
}

func (tr *twilioRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		from := r.FormValue("From")
		tr.mu.Lock()
		tr.sends = append(tr.sends, [2]string{r.FormValue("To"), from})
		tr.mu.Unlock()
		if tr.refuse != nil {
			if code := tr.refuse(from); code != 0 {
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"code":21212,"message":"Invalid From Number"}`))
				return
			}
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"sid": "SM1", "status": "queued"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newAlphaClient(t *testing.T, url string) *Client {
	t.Helper()
	c := NewClient("AC", "tok", "+31600000001")
	c.SetAPIURL(url)
	if err := c.SetAlphaSender("MeshSat", "+31"); err != nil {
		t.Fatal(err)
	}
	return c
}

// MESHSAT-1217. An alert page to a number in one of the sender name's
// countries leaves from the name; one to any other country, and EVERY Send --
// the kits' SMS bearer, OOB mgmt_* commands, the booth, the "@" lane -- leaves
// from the number, because nobody can answer a name.
func TestSendNotificationUsesTheSenderNameOnlyForItsCountries(t *testing.T) {
	ctx := context.Background()
	tr := &twilioRecorder{}
	c := newAlphaClient(t, tr.server(t).URL)

	if _, err := c.SendNotification(ctx, "+31612345678", "SOS dev1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SendNotification(ctx, "+14155551234", "SOS dev1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(ctx, "+31612345678", "mgmt_ping"); err != nil {
		t.Fatal(err)
	}
	want := [][2]string{
		{"+31612345678", "MeshSat"},
		{"+14155551234", "+31600000001"},
		{"+31612345678", "+31600000001"},
	}
	if len(tr.sends) != len(want) {
		t.Fatalf("sends = %v, want %v", tr.sends, want)
	}
	for i := range want {
		if tr.sends[i] != want[i] {
			t.Errorf("send %d = %v, want %v (a kit or a person who must answer was texted from a name)", i, tr.sends[i], want[i])
		}
	}
}

// A refused sender name (a 4xx: nothing queued) must not cost the page: it
// goes again from the number. A server error is NOT retried, because the first
// request may have been queued and the person would be texted twice.
func TestSendNotificationFallsBackOnlyWhenTwilioRefusesTheName(t *testing.T) {
	ctx := context.Background()
	tr := &twilioRecorder{refuse: func(from string) int {
		if from == "MeshSat" {
			return http.StatusBadRequest
		}
		return 0
	}}
	c := newAlphaClient(t, tr.server(t).URL)
	res, err := c.SendNotification(ctx, "+31612345678", "SOS dev1")
	if err != nil || res == nil || res.SID != "SM1" {
		t.Fatalf("refused name: res=%+v err=%v, want the page sent from the number", res, err)
	}
	if len(tr.sends) != 2 || tr.sends[1][1] != "+31600000001" {
		t.Fatalf("sends = %v, want the name then the number", tr.sends)
	}

	tr5 := &twilioRecorder{refuse: func(string) int { return http.StatusBadGateway }}
	c = newAlphaClient(t, tr5.server(t).URL)
	if _, err := c.SendNotification(ctx, "+31612345678", "SOS dev1"); err == nil {
		t.Fatal("a 502 was reported as sent")
	}
	if len(tr5.sends) != 1 {
		t.Fatalf("a server error was retried (%d sends): the person may be paged twice", len(tr5.sends))
	}
}

// WhatsApp has no sender names; a notification on a WhatsApp client keeps its
// whatsapp: number.
func TestWhatsAppNeverSendsFromTheSenderName(t *testing.T) {
	tr := &twilioRecorder{}
	c := newAlphaClient(t, tr.server(t).URL)
	c.SetChannel("whatsapp")
	if _, err := c.SendNotification(context.Background(), "+31612345678", "SOS dev1"); err != nil {
		t.Fatal(err)
	}
	if got := tr.sends[0][1]; got != "whatsapp:+31600000001" {
		t.Errorf("From = %q, want the WhatsApp number", got)
	}
}

func TestSetAlphaSenderRefusesWhatTwilioWouldRefuse(t *testing.T) {
	for _, tc := range []struct {
		name, countries string
		ok              bool
	}{
		{"MeshSat", "+31", true},
		{"Mesh Sat 2", "+31, +32", true},
		{"", "", true},                   // off
		{"MeshSatAlerts", "+31", false},  // 13 characters
		{"MeshSat!", "+31", false},       // punctuation
		{"12345", "+31", false},          // no letter
		{"MeshSat", "", false},           // no country to use it for
		{"MeshSat", "31", false},         // not a calling code
		{"MeshSat", "+31,+0", false},     // no code starts with 0
		{"MeshSat", "+31;DROP", false},   // garbage
		{"Mësh", "+31", false},           // not ASCII
		{" MeshSat", "+31", false},       // padded
		{"MeshSat", "+123456789", false}, // too long a prefix
	} {
		c := NewClient("AC", "tok", "+31600000001")
		err := c.SetAlphaSender(tc.name, tc.countries)
		if (err == nil) != tc.ok {
			t.Errorf("SetAlphaSender(%q, %q) err = %v, want ok=%v", tc.name, tc.countries, err, tc.ok)
		}
		if err != nil && c.AlphaSender() != "" {
			t.Errorf("SetAlphaSender(%q, %q) refused but left the name on", tc.name, tc.countries)
		}
	}
}

// The escalation notifier is the path SOS, dead man's switch, geofence and
// alert-rule pages take, and the one that pages from the name.
func TestNotifierPagesFromTheSenderName(t *testing.T) {
	tr := &twilioRecorder{}
	n := NewNotifier(newAlphaClient(t, tr.server(t).URL))
	if err := n.Notify(context.Background(), []string{"+31612345678", "+4915112345678"}, "SOS", "dev1"); err != nil {
		t.Fatal(err)
	}
	if len(tr.sends) != 2 || tr.sends[0][1] != "MeshSat" || tr.sends[1][1] != "+31600000001" {
		t.Fatalf("sends = %v, want the NL page from MeshSat and the DE one from the number", tr.sends)
	}
}

// A tenant's own Twilio account carries its own sender name; the default
// tenant gets the platform client as configured; a tenant without one sends
// from its number.
func TestPoolGivesEachTenantItsOwnSenderName(t *testing.T) {
	ctx := context.Background()
	accounts := newWAAccounts(t)
	if _, err := accounts.Set(ctx, "t_alpha", integrations.ProviderTwilio, map[string]string{
		"account_sid": "ACa", "auth_token": "tok", "from_number": "+3197010000000",
		"alpha_sender": "Acme", "alpha_sender_countries": "+31, +32"}); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.Set(ctx, "t_plain", integrations.ProviderTwilio, map[string]string{
		"account_sid": "ACp", "auth_token": "tok", "from_number": "+3197010000001"}); err != nil {
		t.Fatal(err)
	}
	accounts.SetPlatform(integrations.ProviderTwilio, map[string]string{"account_sid": "ACplat", "auth_token": "tok", "from_number": "+31600000010"})
	platform := NewClient("ACplat", "tok", "+31600000010")
	if err := platform.SetAlphaSender("MeshSat", "+31"); err != nil {
		t.Fatal(err)
	}
	pool := NewClientPool(platform, accounts)

	if c := pool.ForTenant(ctx, "t_alpha"); c == nil || c.AlphaSender() != "Acme" || c.notificationFrom("+32470123456") != "Acme" {
		t.Errorf("tenant with a sender name: %+v", c)
	}
	if c := pool.ForTenant(ctx, "t_plain"); c == nil || c.AlphaSender() != "" {
		t.Errorf("tenant without a sender name got one: %+v (the platform's name on a customer's account)", c)
	}
	if pool.ForTenant(ctx, store.DefaultTenantID) != platform {
		t.Errorf("the default tenant does not get the platform client")
	}

	// A changed name reaches the cached client.
	if _, err := accounts.Set(ctx, "t_alpha", integrations.ProviderTwilio, map[string]string{"alpha_sender": "Acme2"}); err != nil {
		t.Fatal(err)
	}
	if c := pool.ForTenant(ctx, "t_alpha"); c == nil || c.AlphaSender() != "Acme2" {
		t.Errorf("changed sender name not picked up: %+v", c)
	}
}

// TestOnlyTheAlertNotifierSendsFromTheAlphaSender walks every non-test Go file
// in the module: SendNotification may be called from the escalation notifier
// only. Anything a kit or a person answers -- the SMS bearer, OOB commands,
// the booth, the "@" lane, routed messages, the API send -- must use Send, or
// the reply goes to a name nobody can text (MESHSAT-1217).
func TestOnlyTheAlertNotifierSendsFromTheAlphaSender(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	allowed := filepath.Join(root, "internal", "sms", "notifier.go")
	var seen bool
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "web", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "SendNotification" {
				return true
			}
			if path == allowed {
				seen = true
				return true
			}
			t.Errorf("%s calls SendNotification: only the alert notifier may send from the sender name", fset.Position(call.Pos()))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Error("the alert notifier no longer calls SendNotification; alert pages would never use the sender name")
	}
}

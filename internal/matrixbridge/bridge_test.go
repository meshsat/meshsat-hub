package matrixbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/integrations"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
	"github.com/meshsat/meshsat-hub/internal/tak"
	"github.com/meshsat/meshsat-hub/internal/takchat"
)

// MESHSAT-1736. The victim of every isolation test is store.DefaultTenantID.

const (
	room  = "!incident:hs.test"
	anna  = "@anna:hs.test"
	bot   = "@bot:hs.test"
	token = "bot-token"
	crew  = "meshsat-0a0b0c0d"
)

// --- a homeserver ------------------------------------------------------------

type put struct {
	room, typ, txn string
	content        map[string]any
}

type homeserver struct {
	t  *testing.T
	mu sync.Mutex
	// initial is the first sync's timeline (history the bot must not act on);
	// batches[n] answers since "b<n>" and leads to "b<n+1>". Past the last
	// batch the homeserver fails, which ends a session.
	initial   []event
	batches   [][]event
	encrypted map[string]bool
	puts      []put
	syncs     int
}

func newHomeserver(t *testing.T) (*homeserver, *httptest.Server) {
	h := &homeserver{t: t, encrypted: map[string]bool{}}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	prev := httpClient
	httpClient = srv.Client
	t.Cleanup(func() { httpClient = prev })
	return h, srv
}

func (h *homeserver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer "+token {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN_TOKEN","error":"Invalid access token passed."}`))
		return
	}
	p := strings.Split(strings.TrimPrefix(r.URL.Path, "/_matrix/client/v3/"), "/")
	switch {
	case p[0] == "account" && p[1] == "whoami":
		_, _ = fmt.Fprintf(w, `{"user_id":%q}`, bot)
	case p[0] == "join":
		_, _ = fmt.Fprintf(w, `{"room_id":%q}`, p[1])
	case p[0] == "rooms" && len(p) >= 4 && p[2] == "state" && p[3] == "m.room.encryption":
		if h.encrypted[p[1]] {
			_, _ = w.Write([]byte(`{"algorithm":"m.megolm.v1.aes-sha2"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"Event not found."}`))
	case p[0] == "rooms" && len(p) >= 5 && p[2] == "state" && p[3] == "m.room.member":
		_, _ = w.Write([]byte(`{"displayname":"Anna","membership":"join"}`))
	case p[0] == "rooms" && len(p) == 5 && p[2] == "send":
		var c map[string]any
		_ = json.NewDecoder(r.Body).Decode(&c)
		h.puts = append(h.puts, put{room: p[1], typ: p[3], txn: p[4], content: c})
		_, _ = w.Write([]byte(`{"event_id":"$sent"}`))
	case p[0] == "sync":
		h.syncs++
		since := r.URL.Query().Get("since")
		var evs []event
		next := "b0"
		if since == "" {
			evs = h.initial
		} else {
			var n int
			if _, err := fmt.Sscanf(since, "b%d", &n); err != nil || n >= len(h.batches) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"errcode":"M_TEST_DONE","error":"no more batches"}`))
				return
			}
			evs, next = h.batches[n], fmt.Sprintf("b%d", n+1)
		}
		out := map[string]any{"next_batch": next}
		if len(evs) > 0 {
			out["rooms"] = map[string]any{"join": map[string]any{room: map[string]any{"timeline": map[string]any{"events": evs}}}}
		}
		_ = json.NewEncoder(w).Encode(out)
	default:
		h.t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (h *homeserver) sent() []put {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]put(nil), h.puts...)
}

func text(id, sender, body string) event {
	c, _ := json.Marshal(map[string]any{"msgtype": "m.text", "body": body})
	return event{Type: "m.room.message", EventID: id, Sender: sender, Content: c}
}

// --- the rest of the Hub -----------------------------------------------------

type fakeBus struct {
	mu        sync.Mutex
	published []string
	handlers  []bus.MessageHandler
}

func (b *fakeBus) Publish(topic string, _ byte, _ bool, payload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published = append(b.published, topic+" "+string(payload))
	return nil
}

func (b *fakeBus) Subscribe(_ string, _ byte, h bus.MessageHandler) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers = append(b.handlers, h)
	return nil
}

func (b *fakeBus) sent() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.published...)
}

type fakeAccounts struct {
	accts map[string]*integrations.Account
}

func (f *fakeAccounts) TenantsWith(context.Context, string) ([]string, error) {
	var out []string
	for t := range f.accts {
		out = append(out, t)
	}
	return out, nil
}

func (f *fakeAccounts) ForTenant(_ context.Context, tenantID, _ string) (*integrations.Account, error) {
	return f.accts[tenantID], nil
}

type rigged struct {
	s    *Service
	hs   *homeserver
	bus  *fakeBus
	db   store.Store
	chat *takchat.Service
	acct *integrations.Account
}

func rig(t *testing.T) *rigged {
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
	now := time.Now().UTC()
	for tenant, kit := range map[string]string{"t1": "kit-a", store.DefaultTenantID: "kit-v"} {
		_ = db.CreateTenant(ctx, &store.Tenant{ID: tenant, Slug: tenant, Name: tenant, Status: store.TenantActive})
		// On MQTT, seen now (MESHSAT-1744 sends a kit off MQTT a frame instead).
		if err := db.CreateOrUpdateBridge(ctx, tenant, &store.Bridge{BridgeID: kit, Label: kit, Online: true}); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []store.TAKContact{
		{TenantID: "t1", UID: crew, Callsign: "MSPB", BridgeID: "kit-a", LastSeen: now},
		{TenantID: "t1", UID: "meshsat-0b0b0b0b", Callsign: "MSPB 2", BridgeID: "kit-a", LastSeen: now},
		// The victim has a crew with the same callsign.
		{TenantID: store.DefaultTenantID, UID: "meshsat-77777777", Callsign: "MSPB", BridgeID: "kit-v", LastSeen: now},
	} {
		if err := db.UpsertTAKContact(ctx, &c); err != nil {
			t.Fatal(err)
		}
	}
	hs, srv := newHomeserver(t)
	b := &fakeBus{}
	chat := takchat.New(db, b, true, nil)
	acct := &integrations.Account{Provider: integrations.ProviderMatrix, TenantID: "t1", Fields: map[string]string{
		"homeserver_url": srv.URL, "access_token": token, "rooms": room}}
	s := New(db, &fakeAccounts{accts: map[string]*integrations.Account{"t1": acct}}, chat, nil, true, nil)
	return &rigged{s: s, hs: hs, bus: b, db: db, chat: chat, acct: acct}
}

// session runs one reader session for t1 until the homeserver runs out.
func (r *rigged) session(t *testing.T) {
	t.Helper()
	cfg, err := parseConfig(r.acct)
	if err != nil {
		t.Fatal(err)
	}
	w := &worker{s: r.s, tenant: "t1", cfg: cfg, encrypted: map[string]bool{}, names: map[string]string{}}
	if _, err := w.session(context.Background()); err == nil || !strings.Contains(err.Error(), "M_TEST_DONE") {
		t.Fatalf("the session ended with %v", err)
	}
}

func (r *rigged) toCrew() []string {
	var out []string
	for _, p := range r.bus.sent() {
		if strings.HasPrefix(p, hubmqtt.TopicTAKCotInFor("t1", "kit-a")+" ") {
			out = append(out, p)
		}
	}
	return out
}

func byType(puts []put, typ string) []put {
	var out []put
	for _, p := range puts {
		if p.typ == typ {
			out = append(out, p)
		}
	}
	return out
}

// --- the tests ---------------------------------------------------------------

func TestOnlyAMessageAddressedToACrewReachesTheRadio(t *testing.T) {
	r := rig(t)
	edit, _ := json.Marshal(map[string]any{"msgtype": "m.text", "body": "* @MSPB changed",
		"m.relates_to": map[string]any{"rel_type": "m.replace", "event_id": "$e1"}})
	notice, _ := json.Marshal(map[string]any{"msgtype": "m.notice", "body": "@MSPB from another bot"})
	r.hs.batches = [][]event{{
		text("$e0", anna, "the road north is closed"),
		text("$e1", anna, "@mspb: are you at the bridge?"),
		text("$e2", bot, "@MSPB the bot's own line"),
		{Type: "m.room.message", EventID: "$e3", Sender: anna, Content: edit},
		{Type: "m.room.message", EventID: "$e4", Sender: "@bob:hs.test", Content: notice},
	}}
	r.session(t)

	sent := r.toCrew()
	if len(sent) != 1 || !strings.Contains(sent[0], "are you at the bridge?") || !strings.Contains(sent[0], `senderCallsign="Anna"`) {
		t.Fatalf("to the crew: %v", sent)
	}
	if v := r.bus.sent(); len(v) != 1 {
		t.Fatalf("published %v", v)
	}
	acks := byType(r.hs.sent(), "m.reaction")
	if len(acks) != 1 || acks[0].txn != "ack-$e1" {
		t.Fatalf("reactions %+v", acks)
	}
	op, _ := r.db.GetMatrixOperator(context.Background(), "t1", operatorID("t1", room, anna))
	if op == nil || op.RoomID != room || op.MatrixUserID != anna || op.DisplayName != "Anna" {
		t.Fatalf("operator %+v", op)
	}
	if st, _ := r.db.GetMatrixSync(context.Background(), "t1"); st == nil || st.NextBatch != "b1" || st.BotUserID != bot {
		t.Fatalf("sync state %+v", st)
	}
}

func TestTheFirstStartDoesNotActOnHistory(t *testing.T) {
	r := rig(t)
	r.hs.initial = []event{text("$old", anna, "@MSPB said before the bot existed")}
	r.session(t)
	if v := r.bus.sent(); len(v) != 0 {
		t.Fatalf("history went to the radio: %v", v)
	}
}

func TestABatchReadTwiceAcrossAHandoverSendsOnce(t *testing.T) {
	r := rig(t)
	r.hs.batches = [][]event{{text("$e1", anna, "@MSPB status?")}}
	r.session(t)
	// The old leader stored its place late; the new one reads the batch again.
	if err := r.db.SaveMatrixSync(context.Background(), &store.MatrixSync{TenantID: "t1", BotUserID: bot, NextBatch: "b0"}); err != nil {
		t.Fatal(err)
	}
	r.session(t)
	if v := r.toCrew(); len(v) != 1 {
		t.Fatalf("sent %d times: %v", len(v), v)
	}
	if acks := byType(r.hs.sent(), "m.reaction"); len(acks) != 1 {
		t.Fatalf("reactions %+v", acks)
	}
}

func TestTheLongestCallsignWinsAndAnUnknownOneIsAnswered(t *testing.T) {
	r := rig(t)
	r.hs.batches = [][]event{{
		text("$e1", anna, "@MSPB 2 hold position"),
		text("$e2", anna, "@NOPE anyone?"),
		text("$e3", anna, "@carol:hs.test can you check the map"),
	}}
	r.session(t)
	sent := r.toCrew()
	if len(sent) != 1 || !strings.Contains(sent[0], `uid="meshsat-0b0b0b0b"`) || !strings.Contains(sent[0], "hold position") {
		t.Fatalf("to the crew: %v", sent)
	}
	notices := byType(r.hs.sent(), "m.room.message")
	if len(notices) != 1 || !strings.Contains(notices[0].content["body"].(string), "No crew called NOPE") ||
		notices[0].content["msgtype"] != "m.notice" {
		t.Fatalf("notices %+v", notices)
	}
}

func TestOnlyTheListedSendersMayWriteToCrews(t *testing.T) {
	r := rig(t)
	r.acct.Fields["senders"] = ":other.test\n@dave:hs.test"
	r.hs.batches = [][]event{{text("$e1", anna, "@MSPB go"), text("$e2", "@dave:hs.test", "@MSPB go now")}}
	r.session(t)
	sent := r.toCrew()
	if len(sent) != 1 || !strings.Contains(sent[0], "go now") {
		t.Fatalf("to the crew: %v", sent)
	}
}

func TestAnEncryptedRoomIsNeitherReadNorAnswered(t *testing.T) {
	r := rig(t)
	r.hs.encrypted[room] = true
	r.hs.batches = [][]event{{text("$e1", anna, "@MSPB go")}}
	r.session(t)
	if v := r.bus.sent(); len(v) != 0 {
		t.Fatalf("an encrypted room reached the radio: %v", v)
	}
	if v := r.hs.sent(); len(v) != 0 {
		t.Fatalf("the bot wrote into an encrypted room: %+v", v)
	}
	// An answer to an operator of that room is not posted either.
	ctx := context.Background()
	op := operatorID("t1", room, anna)
	_ = r.db.UpsertMatrixOperator(ctx, &store.MatrixOperator{TenantID: "t1", UserID: op, RoomID: room, MatrixUserID: anna})
	r.s.postAnswer(ctx, &store.TAKChatMessage{TenantID: "t1", ID: "ans-x", CrewUID: crew, UserID: op, Direction: "in", Text: "copy"})
	if v := r.hs.sent(); len(v) != 0 {
		t.Fatalf("an answer went into an encrypted room: %+v", v)
	}
}

func TestTheCrewsAnswerIsPostedIntoTheRoomOnce(t *testing.T) {
	r := rig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.chat.SetReplyHook(r.s.OnReply)
	if err := r.chat.StartReplySink(ctx); err != nil {
		t.Fatal(err)
	}
	r.s.Start(ctx)
	r.hs.batches = [][]event{{text("$e1", anna, "@MSPB where are you?")}}
	r.session(t)

	op := tak.OperatorUIDPrefix + operatorID("t1", room, anna)
	xml, err := tak.MarshalCotEvent(tak.BuildGeoChat(tak.ChatPeer{UID: crew, Callsign: "MSPB"},
		tak.ChatPeer{UID: op, Callsign: "Anna"}, "at the bridge", "ans-1", time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	// Both replicas receive the export; the sink stores it once.
	r.bus.mu.Lock()
	handlers := append([]bus.MessageHandler(nil), r.bus.handlers...)
	r.bus.mu.Unlock()
	for i := 0; i < 2; i++ {
		for _, h := range handlers {
			h(hubmqtt.TopicTAKCotOutFor("t1", "kit-a"), xml)
		}
	}
	var answers []put
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if answers = byType(r.hs.sent(), "m.room.message"); len(answers) > 0 {
			break
		}
	}
	time.Sleep(100 * time.Millisecond)
	answers = byType(r.hs.sent(), "m.room.message")
	if len(answers) != 1 {
		t.Fatalf("answers posted %+v", answers)
	}
	a := answers[0]
	if a.room != room || a.txn != "answer-ans-1" || a.content["body"] != "MSPB: at the bridge" || a.content["msgtype"] != "m.text" {
		t.Fatalf("answer %+v", a)
	}
	if m, _ := json.Marshal(a.content["m.mentions"]); string(m) != `{"user_ids":["`+anna+`"]}` {
		t.Fatalf("mentions %s", m)
	}
	// A second poster (the other replica) finds the answer claimed.
	r.s.postAnswer(context.Background(), &store.TAKChatMessage{TenantID: "t1", ID: "ans-1", CrewUID: crew,
		UserID: operatorID("t1", room, anna), Direction: "in", Text: "at the bridge"})
	if n := len(byType(r.hs.sent(), "m.room.message")); n != 1 {
		t.Fatalf("posted %d times", n)
	}
}

func TestAnAnswerIsNeverPostedForAnotherTenant(t *testing.T) {
	r := rig(t)
	ctx := context.Background()
	op := operatorID("t1", room, anna)
	_ = r.db.UpsertMatrixOperator(ctx, &store.MatrixOperator{TenantID: "t1", UserID: op, RoomID: room, MatrixUserID: anna})
	// The victim tenant stores an answer under the same operator id: it has no
	// Matrix operator of that id, so nothing is posted, least of all t1's room.
	r.s.postAnswer(ctx, &store.TAKChatMessage{TenantID: store.DefaultTenantID, ID: "ans-v", CrewUID: "meshsat-77777777",
		UserID: op, Direction: "in", Text: "victim"})
	if v := r.hs.sent(); len(v) != 0 {
		t.Fatalf("posted %+v", v)
	}
}

func TestTheCrewsCommandListsTheCrews(t *testing.T) {
	r := rig(t)
	r.hs.batches = [][]event{{text("$e1", anna, "!crews")}}
	r.session(t)
	n := byType(r.hs.sent(), "m.room.message")
	if len(n) != 1 || n[0].content["body"] != "Crews seen in the last day: MSPB, MSPB 2. Write @ and a callsign, then your message." {
		t.Fatalf("notices %+v", n)
	}
}

func TestATokenTheHomeserverRefusesEndsTheSession(t *testing.T) {
	r := rig(t)
	r.acct.Fields["access_token"] = "wrong"
	cfg, _ := parseConfig(r.acct)
	w := &worker{s: r.s, tenant: "t1", cfg: cfg, encrypted: map[string]bool{}, names: map[string]string{}}
	healthy, err := w.session(context.Background())
	if healthy || err == nil || !strings.Contains(err.Error(), "M_UNKNOWN_TOKEN") || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("healthy %v, err %v", healthy, err)
	}
}

func TestCheckReportsEachRoom(t *testing.T) {
	r := rig(t)
	r.acct.Fields["rooms"] = room + "\n!secret:hs.test"
	r.hs.encrypted["!secret:hs.test"] = true
	ok, detail := Check(context.Background(), r.acct)
	if ok || !strings.Contains(detail, "Signed in as "+bot) || !strings.Contains(detail, room+": ready") ||
		!strings.Contains(detail, "!secret:hs.test: encrypted") {
		t.Fatalf("%v %s", ok, detail)
	}
}

func TestParseConfigRefusesWhatIsNotAnId(t *testing.T) {
	base := map[string]string{"homeserver_url": "https://hs.test", "access_token": "x"}
	for name, f := range map[string]map[string]string{
		"no rooms":       {},
		"a room alias":   {"rooms": "#ops:hs.test"},
		"a bare sender":  {"rooms": room, "senders": "anna"},
		"no homeserver":  {"rooms": room, "homeserver_url": ""},
		"no bot's token": {"rooms": room, "access_token": ""},
	} {
		fields := map[string]string{}
		for k, v := range base {
			fields[k] = v
		}
		for k, v := range f {
			fields[k] = v
		}
		if _, err := parseConfig(&integrations.Account{Fields: fields}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c, err := parseConfig(&integrations.Account{Fields: map[string]string{"homeserver_url": "https://hs.test", "access_token": "x",
		"rooms": room + ", " + room + "\n!two:hs.test", "senders": ":hs.test"}})
	if err != nil || len(c.rooms) != 2 || !c.senderAllowed(anna) || c.senderAllowed("@eve:evil.test") || c.senderAllowed("@eve:evilhs.test") {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestStripReplyFallback(t *testing.T) {
	for in, want := range map[string]string{
		"> <@anna:hs.test> are you there?\n> second line\n\n@MSPB yes": "\n@MSPB yes",
		"@MSPB plain":  "@MSPB plain",
		">not a quote": ">not a quote",
	} {
		if got := stripReplyFallback(in); got != want {
			t.Errorf("%q: got %q", in, got)
		}
	}
}

// MESHSAT-1746: the kit's word on a message written from the room lands beside
// that message: ❌ and one notice when the kit did not deliver it, ✅✅ when the
// far end confirmed it, nothing for queued, each once across both replicas.
func TestAKitsWordLandsBesideTheMessageInTheRoom(t *testing.T) {
	r := rig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.chat.SetStatusHook(r.s.OnStatus)
	if err := r.chat.StartReplySink(ctx); err != nil {
		t.Fatal(err)
	}
	r.s.Start(ctx)
	r.hs.batches = [][]event{{text("$e1", anna, "@MSPB where are you?"), text("$e2", anna, "@MSPB 2 hold")}}
	r.session(t)
	if v := r.toCrew(); len(v) != 2 {
		t.Fatalf("to the crews: %v", v)
	}
	idOf := func(crewUID string) string {
		rows, _ := r.db.ListTAKChatMessages(ctx, "t1", crewUID, 10)
		if len(rows) != 1 || rows[0].ClientRef == "" {
			t.Fatalf("rows for %s: %+v", crewUID, rows)
		}
		return rows[0].ID
	}
	m1, m2 := idOf(crew), idOf("meshsat-0b0b0b0b")

	// The kit's word arrives on its own subtree, at both replicas.
	word := func(id, crewUID, status, reason, detail string) {
		t.Helper()
		b, _ := json.Marshal(hubmqtt.TAKChatStatus{MessageID: id, CrewUID: crewUID, Status: status, Reason: reason, Detail: detail})
		r.bus.mu.Lock()
		handlers := append([]bus.MessageHandler(nil), r.bus.handlers...)
		r.bus.mu.Unlock()
		for i := 0; i < 2; i++ {
			for _, h := range handlers {
				h(hubmqtt.TopicTAKChatStatusFor("t1", "kit-a"), b)
			}
		}
	}
	posted := func(want int) []put {
		t.Helper()
		var out []put
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if out = r.hs.sent(); len(out) >= want {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
		return r.hs.sent()
	}
	// Two ✅ from the sends; a queued adds nothing.
	word(m1, crew, "queued", "", "queued as a direct message to !0a0b0c0d")
	if v := posted(2); len(v) != 2 || v[0].typ != "m.reaction" || v[1].typ != "m.reaction" {
		t.Fatalf("after queued: %+v", v)
	}
	// Not delivered: ❌ on $e1 and one notice naming the crew and the reason.
	word(m1, crew, "undelivered", "not_heard", "the kit has not heard MSPB in the last day")
	v := posted(4)
	if len(v) != 4 {
		t.Fatalf("after undelivered: %+v", v)
	}
	nack, notice := v[2], v[3]
	if nack.typ != "m.reaction" || nack.txn != "nack-$e1" {
		t.Fatalf("reaction %+v", nack)
	}
	if rel, _ := json.Marshal(nack.content["m.relates_to"]); string(rel) != `{"event_id":"$e1","key":"❌","rel_type":"m.annotation"}` {
		t.Fatalf("relates_to %s", rel)
	}
	if notice.typ != "m.room.message" || notice.txn != "status-"+m1+"-undelivered" || notice.content["msgtype"] != "m.notice" ||
		notice.content["body"] != "Not delivered to MSPB: the kit has not heard MSPB in the last day." {
		t.Fatalf("notice %+v", notice)
	}
	if m, _ := json.Marshal(notice.content["m.mentions"]); string(m) != `{"user_ids":["`+anna+`"]}` {
		t.Fatalf("mentions %s", m)
	}
	// Delivered on the other message: ✅✅ on $e2, no notice.
	word(m2, "meshsat-0b0b0b0b", "delivered", "", "")
	v = posted(5)
	if len(v) != 5 || v[4].typ != "m.reaction" || v[4].txn != "ack2-$e2" {
		t.Fatalf("after delivered: %+v", v)
	}
	if rel, _ := json.Marshal(v[4].content["m.relates_to"]); string(rel) != `{"event_id":"$e2","key":"✅✅","rel_type":"m.annotation"}` {
		t.Fatalf("relates_to %s", rel)
	}
	// The other replica's poster finds each claimed; a console operator's
	// message and one without its event id say nothing.
	op := operatorID("t1", room, anna)
	r.s.postStatus(ctx, &store.TAKChatMessage{TenantID: "t1", ID: m1, CrewUID: crew, UserID: op, Direction: "out",
		Status: "undelivered", StatusDetail: "again", ClientRef: "matrix:$e1"})
	r.s.postStatus(ctx, &store.TAKChatMessage{TenantID: "t1", ID: "console-1", CrewUID: crew, UserID: "7", Direction: "out",
		Status: "undelivered", StatusDetail: "a console message"})
	r.s.postStatus(ctx, &store.TAKChatMessage{TenantID: "t1", ID: "old-1", CrewUID: crew, UserID: op, Direction: "out",
		Status: "undelivered", StatusDetail: "before the ref was kept"})
	if n := len(r.hs.sent()); n != 5 {
		t.Fatalf("posted %d times: %+v", n, r.hs.sent())
	}
}

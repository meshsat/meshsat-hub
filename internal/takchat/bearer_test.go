package takchat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/tak"
)

// MESHSAT-1744: a dispatcher's chat to a kit that is off MQTT goes as a
// frame over satellite or SMS, and the kit's answer and its word come back
// the same way.

type frameSends struct {
	mu   sync.Mutex
	mt   []string // "<imei>:<hex-free base64 of wire>"
	sms  []string // "<bridge>:<body>"
	fail error
}

func (f *frameSends) sendMT(_ context.Context, _, imei string, wire []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.mt = append(f.mt, imei+":"+base64.StdEncoding.EncodeToString(wire))
	return nil
}

func (f *frameSends) sendSMS(_ context.Context, _, bridgeID, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.sms = append(f.sms, bridgeID+":"+body)
	return nil
}

// kit describes one bridge of t1 for a bearer case.
type kit struct {
	online     bool
	stale      bool // last seen an hour ago
	lastBearer string
	satIMEI    string // a modem in the birth
	satStatus  string
	cellular   bool
	phone      string
	registered bool // the modem is a device of the tenant
}

func (k kit) birth() string {
	var b protocol.BridgeBirth
	if k.satIMEI != "" {
		b.Interfaces = append(b.Interfaces, protocol.InterfaceInfo{Name: "iridium_0", Type: "iridium_sbd", Status: k.satStatus, IMEI: k.satIMEI})
	}
	if k.cellular {
		b.Interfaces = append(b.Interfaces, protocol.InterfaceInfo{Name: "cellular_0", Type: "cellular", Status: "online"})
	}
	if len(b.Interfaces) == 0 {
		return ""
	}
	j, _ := json.Marshal(b)
	return string(j)
}

func rigKit(t *testing.T, s *Service, db store.Store, bridgeID string, k kit) {
	t.Helper()
	ctx := context.Background()
	if err := db.CreateOrUpdateBridge(ctx, "t1", &store.Bridge{BridgeID: bridgeID, Label: bridgeID, Online: k.online, LastBirth: k.birth()}); err != nil {
		t.Fatal(err)
	}
	if k.lastBearer != "" {
		if err := db.SetBridgeLastReport(ctx, "t1", bridgeID, k.lastBearer, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if k.phone != "" {
		if err := db.UpsertOOBPeer(ctx, &store.OOBPeer{TenantID: "t1", BridgeID: bridgeID, PeerID: 7, KeyEnc: []byte{1, 2, 3}, Phone: k.phone, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	if k.satIMEI != "" && k.registered {
		_ = db.CreateDevice(ctx, "t1", &store.Device{IMEI: k.satIMEI, Label: "modem", Type: "rockblock"})
		if err := db.AssociateDeviceWithBridge(ctx, "t1", k.satIMEI, bridgeID); err != nil {
			t.Fatal(err)
		}
	}
}

func setCrewChat(t *testing.T, db store.Store, sat, smsOn bool, force string) {
	t.Helper()
	ctx := context.Background()
	tenant, err := db.GetTenant(ctx, "t1")
	if err != nil || tenant == nil {
		t.Fatal(err)
	}
	tenant.CrewChatSatellite, tenant.CrewChatSMSOff, tenant.CrewChatForceBearer = sat, !smsOn, force
	if err := db.UpdateTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
}

func TestTheBearerFollowsTheKitsStateAndTheWorkspacesChoices(t *testing.T) {
	const imei = "300234067504200"
	reachable := kit{online: true, satIMEI: imei, satStatus: "online", cellular: true, phone: "+31612345678", registered: true}
	cases := []struct {
		name          string
		kit           kit
		sat, sms      bool
		force         string
		noMT, noSMS   bool
		want          string
		wantErr       error
		wantWhy       string
		wantReplyHere bool
	}{
		{name: "on MQTT: mqtt whatever is allowed", kit: reachable, sat: true, sms: true, want: BearerMQTT},
		{name: "off MQTT, defaults: SMS", kit: kit{stale: true, satIMEI: imei, satStatus: "online", cellular: true, phone: "+31612345678", registered: true},
			sat: false, sms: true, want: BearerSMS},
		{name: "off MQTT, satellite allowed: the modem", kit: kit{stale: true, satIMEI: imei, satStatus: "online", cellular: true, phone: "+31612345678", registered: true},
			sat: true, sms: true, want: BearerSBD},
		{name: "last report came over SMS: off MQTT", kit: kit{online: true, lastBearer: "sms", cellular: true, phone: "+31612345678"},
			sat: false, sms: true, want: BearerSMS},
		{name: "never online: SMS", kit: kit{cellular: true, phone: "+31612345678"}, sat: false, sms: true, want: BearerSMS},
		{name: "off MQTT, no phone on file", kit: kit{stale: true, cellular: true}, sat: false, sms: true,
			wantErr: ErrNoBearer, wantWhy: "no phone number on file"},
		{name: "off MQTT, birth names no cellular interface", kit: kit{stale: true, satIMEI: imei, satStatus: "offline", phone: "+31612345678"},
			sat: false, sms: true, wantErr: ErrNoBearer, wantWhy: "no cellular interface"},
		{name: "off MQTT, SMS off, satellite off", kit: kit{stale: true, cellular: true, phone: "+31612345678"}, sat: false, sms: false,
			wantErr: ErrNoBearer, wantWhy: "SMS: off for this workspace"},
		{name: "off MQTT, satellite allowed but the modem is another tenant's", kit: kit{stale: true, satIMEI: imei, satStatus: "online", cellular: false},
			sat: true, sms: false, wantErr: ErrNoBearer, wantWhy: "not registered to this workspace"},
		{name: "off MQTT, satellite allowed, modem offline in the birth still named", kit: kit{stale: true, satIMEI: imei, satStatus: "offline", registered: true},
			sat: true, sms: false, want: BearerSBD},
		{name: "off MQTT, no SMS sender on this Hub", kit: kit{stale: true, cellular: true, phone: "+31612345678"}, sat: false, sms: true, noSMS: true,
			wantErr: ErrNoBearer, wantWhy: "no SMS sender"},
		{name: "forced SMS while on MQTT", kit: reachable, sat: false, sms: true, force: "sms", want: BearerSMS, wantReplyHere: true},
		{name: "forced satellite while on MQTT", kit: reachable, sat: false, sms: false, force: "satellite", want: BearerSBD, wantReplyHere: true},
		{name: "forced mqtt while off", kit: kit{stale: true}, force: "mqtt", want: BearerMQTT},
		{name: "forced SMS with no phone", kit: kit{online: true, cellular: true}, force: "sms", wantErr: ErrNoBearer, wantWhy: "SMS forced"},
		{name: "a forced word that is not a bearer", kit: reachable, force: "pigeon", wantErr: ErrNoBearer, wantWhy: "not a bearer"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, b, db, now := rig(t)
			sends := &frameSends{}
			bearers := Bearers{SendMT: sends.sendMT, SendSMS: sends.sendSMS}
			if c.noMT {
				bearers.SendMT = nil
			}
			if c.noSMS {
				bearers.SendSMS = nil
			}
			s.SetBearers(bearers)
			rigKit(t, s, db, "kit-a", c.kit)
			setCrewChat(t, db, c.sat, c.sms, c.force)
			if c.kit.stale {
				s.now = func() time.Time { return now.Add(time.Hour) }
			}
			msg, created, err := s.Send(context.Background(), SendRequest{TenantID: "t1", CrewUID: "meshsat-0a0b0c0d",
				UserID: "7", Name: "Anna", Text: "check in please"})
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) || !strings.Contains(err.Error(), c.wantWhy) {
					t.Fatalf("err %v, want %v with %q", err, c.wantErr, c.wantWhy)
				}
				if len(b.published()) != 0 || len(sends.sms) != 0 || len(sends.mt) != 0 {
					t.Error("a refused send went somewhere")
				}
				if rows, _ := db.ListTAKChatMessages(context.Background(), "t1", "meshsat-0a0b0c0d", 10); len(rows) != 0 {
					t.Error("a refused send was stored")
				}
				return
			}
			if err != nil || !created {
				t.Fatalf("send: %v %v", created, err)
			}
			if msg.Bearer != c.want {
				t.Fatalf("bearer %q, want %q", msg.Bearer, c.want)
			}
			stored, _ := db.GetTAKChatMessage(context.Background(), "t1", msg.ID)
			if stored == nil || stored.Bearer != c.want || stored.Status != "sent" {
				t.Errorf("stored %+v", stored)
			}
			var frame []byte
			switch c.want {
			case BearerMQTT:
				if len(b.published()) != 1 || len(sends.sms)+len(sends.mt) != 0 {
					t.Fatalf("mqtt: published %d, frames %d/%d", len(b.published()), len(sends.sms), len(sends.mt))
				}
				return
			case BearerSMS:
				if len(sends.sms) != 1 || len(b.published()) != 0 || len(sends.mt) != 0 {
					t.Fatalf("sms: %v / %v / %d", sends.sms, sends.mt, len(b.published()))
				}
				bridgeID, body, _ := strings.Cut(sends.sms[0], ":")
				if bridgeID != "kit-a" {
					t.Errorf("sent to %s", bridgeID)
				}
				frame, _ = base64.StdEncoding.DecodeString(body)
				if len(body) > 160 {
					t.Errorf("the SMS is %d characters", len(body))
				}
			case BearerSBD:
				if len(sends.mt) != 1 || len(b.published()) != 0 || len(sends.sms) != 0 {
					t.Fatalf("sbd: %v / %v / %d", sends.mt, sends.sms, len(b.published()))
				}
				imeiSent, wire, _ := strings.Cut(sends.mt[0], ":")
				if imeiSent != imei {
					t.Errorf("sent to modem %s", imeiSent)
				}
				frame, _ = base64.StdEncoding.DecodeString(wire)
			}
			chat, err := DecodeChat(frame)
			if err != nil {
				t.Fatal(err)
			}
			if chat.MessageID != msg.ID || chat.Node != 0x0a0b0c0d || chat.ToKit || chat.Callsign != "Anna" || chat.Text != "check in please" ||
				chat.OperatorHash != OperatorHash(tak.OperatorUIDPrefix+"7") || chat.ReplyHere != c.wantReplyHere {
				t.Errorf("the frame: %+v (reply here %v)", chat, c.wantReplyHere)
			}
		})
	}
}

func TestAFrameSendIsClaimedOnceAndAFailureIsRecorded(t *testing.T) {
	s, _, db, now := rig(t)
	sends := &frameSends{}
	s.SetBearers(Bearers{SendSMS: sends.sendSMS})
	rigKit(t, s, db, "kit-a", kit{stale: true, cellular: true, phone: "+31612345678"})
	s.now = func() time.Time { return now.Add(time.Hour) }
	ctx := context.Background()
	// The claim is taken before the send: a second replica running the same
	// message (the same id) sends nothing.
	if ok, err := db.ClaimOnce(ctx, "takchat:out:t1:"+messageID("t1", "7", "r-1")+":sms"); err != nil || !ok {
		t.Fatal(err)
	}
	msg, created, err := s.Send(ctx, SendRequest{TenantID: "t1", CrewUID: "meshsat-0a0b0c0d", UserID: "7", Text: "hi", ClientRef: "r-1"})
	if err != nil || !created || msg.Bearer != BearerSMS || len(sends.sms) != 0 {
		t.Fatalf("claimed elsewhere: %+v %v %v, sent %v", msg, created, err, sends.sms)
	}
	// A send the bearer refuses is recorded as failed, with why.
	rigKit(t, s, db, "kit-b", kit{stale: true, cellular: true, phone: "+31600000002"})
	sends.fail = errors.New("twilio: 21614 not a mobile number")
	s.now = func() time.Time { return now.Add(2 * time.Hour) }
	msg, _, err = s.Send(ctx, SendRequest{TenantID: "t1", CrewUID: "meshsat-11112222", UserID: "7", Text: "hi"})
	if !errors.Is(err, ErrPublish) || msg == nil || msg.Status != "failed" || !strings.Contains(msg.StatusDetail, "over SMS") {
		t.Fatalf("a failed send: %+v %v", msg, err)
	}
	if stored, _ := db.GetTAKChatMessage(ctx, "t1", msg.ID); stored == nil || stored.Status != "failed" || stored.Bearer != BearerSMS {
		t.Errorf("stored %+v", stored)
	}
}

func TestATextTooLongForTheBearerIsRefusedBeforeAnythingIsStored(t *testing.T) {
	s, _, db, now := rig(t)
	sends := &frameSends{}
	s.SetBearers(Bearers{SendSMS: sends.sendSMS, SendMT: sends.sendMT})
	rigKit(t, s, db, "kit-a", kit{stale: true, cellular: true, phone: "+31612345678"})
	s.now = func() time.Time { return now.Add(time.Hour) }
	long := strings.Repeat("zq9 ", 45) // 180 bytes, under MaxBytes, and SMAZ2 does not help it
	_, _, err := s.Send(context.Background(), SendRequest{TenantID: "t1", CrewUID: "meshsat-0a0b0c0d", UserID: "7", Text: long})
	if !errors.Is(err, ErrFrameTooLong) || !strings.Contains(err.Error(), "sms carries 120") {
		t.Fatalf("err %v", err)
	}
	if rows, _ := db.ListTAKChatMessages(context.Background(), "t1", "meshsat-0a0b0c0d", 10); len(rows) != 0 || len(sends.sms) != 0 {
		t.Error("stored or sent")
	}
	// The same text fits an SBD MT.
	setCrewChat(t, db, true, true, "")
	rigKit(t, s, db, "kit-a", kit{stale: true, satIMEI: "300234067504200", satStatus: "online", cellular: true, phone: "+31612345678", registered: true})
	msg, _, err := s.Send(context.Background(), SendRequest{TenantID: "t1", CrewUID: "meshsat-0a0b0c0d", UserID: "7", Text: long})
	if err != nil || msg.Bearer != BearerSBD || len(sends.mt) != 1 {
		t.Fatalf("over SBD: %+v %v", msg, err)
	}
}

// sendOverSMS is one question to the test crew over SMS, as the kit got it.
func sendOverSMS(t *testing.T, s *Service, db store.Store, now time.Time) (*store.TAKChatMessage, *frameSends) {
	t.Helper()
	sends := &frameSends{}
	s.SetBearers(Bearers{SendSMS: sends.sendSMS})
	rigKit(t, s, db, "kit-a", kit{stale: true, cellular: true, phone: "+31612345678"})
	s.now = func() time.Time { return now.Add(time.Hour) }
	msg, created, err := s.Send(context.Background(), SendRequest{TenantID: "t1", CrewUID: "meshsat-0a0b0c0d", UserID: "7", Name: "Anna", Text: "check in please"})
	if err != nil || !created || msg.Bearer != BearerSMS {
		t.Fatalf("send: %+v %v %v", msg, created, err)
	}
	return msg, sends
}

func TestACrewsAnswerFrameIsStoredAgainstItsQuestionOnce(t *testing.T) {
	s, _, db, now := rig(t)
	var replies []*store.TAKChatMessage
	s.SetReplyHook(func(_ context.Context, m *store.TAKChatMessage) { replies = append(replies, m) })
	q, _ := sendOverSMS(t, s, db, now)
	ctx := context.Background()
	s.now = func() time.Time { return now.Add(2 * time.Hour) } // the answer comes later
	payload, _ := EncodeAnswer(Answer{MessageID: q.ID, Node: 0x0a0b0c0d, At: now, Text: "on my way"})
	if got := s.CrewAnswer(ctx, "t1", BearerSMS, "+31612345678", payload); got != "kit-a" {
		t.Fatalf("taken as %q", got)
	}
	// The SMS was retried by the carrier: the same answer is one row, one post.
	if got := s.CrewAnswer(ctx, "t1", BearerSMS, "+31612345678", payload); got != "kit-a" {
		t.Fatalf("the retry: %q", got)
	}
	rows, _ := db.ListTAKChatMessages(ctx, "t1", "meshsat-0a0b0c0d", 10)
	if len(rows) != 2 || rows[1].Direction != "in" || rows[1].Text != "on my way" || rows[1].Bearer != BearerSMS ||
		rows[1].OperatorUID != tak.OperatorUIDPrefix+"7" || rows[1].UserID != "7" || rows[1].BridgeID != "kit-a" || rows[1].Status != "received" {
		t.Fatalf("rows %+v", rows)
	}
	if len(replies) != 1 || replies[0].ID != rows[1].ID {
		t.Errorf("the reply hook ran %d times", len(replies))
	}
	// Refused: another number when the kit's is on file, another tenant,
	// a node that is not the question's crew, an unknown question, and
	// nothing from a satellite modem that is not the kit's.
	for name, try := range map[string]func() string{
		"another number": func() string { return s.CrewAnswer(ctx, "t1", BearerSMS, "+31600000000", payload) },
		"another tenant": func() string { return s.CrewAnswer(ctx, store.DefaultTenantID, BearerSMS, "+31612345678", payload) },
		"unknown modem":  func() string { return s.CrewAnswer(ctx, "t1", BearerSBD, "300234060000000", payload) },
		"not the crew": func() string {
			p, _ := EncodeAnswer(Answer{MessageID: q.ID, Node: 0x11112222, At: now, Text: "x"})
			return s.CrewAnswer(ctx, "t1", BearerSMS, "+31612345678", p)
		},
		"unknown message": func() string {
			p, _ := EncodeAnswer(Answer{MessageID: vecMessageID, Node: 0x0a0b0c0d, At: now, Text: "x"})
			return s.CrewAnswer(ctx, "t1", BearerSMS, "+31612345678", p)
		},
		"unreadable": func() string { return s.CrewAnswer(ctx, "t1", BearerSMS, "+31612345678", payload[:10]) },
	} {
		if got := try(); got != "" {
			t.Errorf("%s was taken as %q", name, got)
		}
	}
	if rows, _ := db.ListTAKChatMessages(ctx, "t1", "meshsat-0a0b0c0d", 10); len(rows) != 2 {
		t.Errorf("a refused answer was stored: %+v", rows)
	}
}

func TestAKitsStatusFrameIsRecordedOnItsQuestion(t *testing.T) {
	s, _, db, now := rig(t)
	var statuses []*store.TAKChatMessage
	s.SetStatusHook(func(_ context.Context, m *store.TAKChatMessage) { statuses = append(statuses, m) })
	q, _ := sendOverSMS(t, s, db, now)
	ctx := context.Background()
	payload, _ := EncodeStatus(Status{MessageID: q.ID, Node: 0x0a0b0c0d, At: now, Status: StatusUndelivered, Reason: "not_heard",
		Detail: "the kit has not heard MESHSAT-0c0d in the last day"})
	if got := s.CrewStatus(ctx, "t1", BearerSMS, "+31612345678", payload); got != "kit-a" {
		t.Fatalf("taken as %q", got)
	}
	stored, _ := db.GetTAKChatMessage(ctx, "t1", q.ID)
	if stored == nil || stored.Status != StatusUndelivered || stored.StatusDetail != "the kit has not heard MESHSAT-0c0d in the last day" || stored.Bearer != BearerSMS {
		t.Fatalf("stored %+v", stored)
	}
	if len(statuses) != 1 || statuses[0].Status != StatusUndelivered {
		t.Errorf("the status hook: %+v", statuses)
	}
	// A late "queued" never undoes the final word; a wrong number is refused.
	late, _ := EncodeStatus(Status{MessageID: q.ID, Node: 0x0a0b0c0d, At: now, Status: StatusQueued, Detail: "queued"})
	if got := s.CrewStatus(ctx, "t1", BearerSMS, "+31612345678", late); got != "" {
		t.Error("a late queued was taken")
	}
	if got := s.CrewStatus(ctx, "t1", BearerSMS, "+31600000000", payload); got != "" {
		t.Error("another number's status was taken")
	}
	if stored, _ := db.GetTAKChatMessage(ctx, "t1", q.ID); stored.Status != StatusUndelivered {
		t.Errorf("changed: %+v", stored)
	}
}

func TestAnAnswerOverMQTTToAHashedOperatorFindsTheOperator(t *testing.T) {
	s, b, db, now := rig(t)
	var replies []*store.TAKChatMessage
	s.SetReplyHook(func(_ context.Context, m *store.TAKChatMessage) { replies = append(replies, m) })
	q, _ := sendOverSMS(t, s, db, now)
	if err := s.StartReplySink(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now.Add(2 * time.Hour) } // the answer comes later
	// The kit came back on MQTT and answers as a GeoChat to the operator it
	// knows by hash alone.
	hashed := OperatorUIDFromHash(OperatorHash(tak.OperatorUIDPrefix + "7"))
	ev := tak.BuildGeoChat(tak.ChatPeer{UID: "meshsat-0a0b0c0d", Callsign: "MESHSAT-0c0d"}, tak.ChatPeer{UID: hashed, Callsign: "Anna"},
		"on my way", "a1", now.Add(time.Hour))
	xml, _ := tak.MarshalCotEvent(ev)
	b.subs[hubmqtt.TAKCotOutFilters()[0]](hubmqtt.TopicTAKCotOutFor("t1", "kit-a"), xml)
	deadline := time.Now().Add(2 * time.Second)
	var rows []store.TAKChatMessage
	for time.Now().Before(deadline) {
		rows, _ = db.ListTAKChatMessages(context.Background(), "t1", "meshsat-0a0b0c0d", 10)
		if len(rows) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(rows) != 2 || rows[1].ID != "a1" || rows[1].OperatorUID != tak.OperatorUIDPrefix+"7" || rows[1].UserID != "7" || rows[1].Bearer != "" {
		t.Fatalf("rows %+v (question %s)", rows, q.ID)
	}
	if len(replies) != 1 || replies[0].UserID != "7" {
		t.Errorf("the reply hook: %+v", replies)
	}
	// A hash nobody wrote under is not an answer to anyone.
	other := tak.BuildGeoChat(tak.ChatPeer{UID: "meshsat-0a0b0c0d", Callsign: "MESHSAT-0c0d"},
		tak.ChatPeer{UID: OperatorUIDFromHash(OperatorHash("meshsat-hub-op-nobody")), Callsign: "X"}, "hello?", "a2", now.Add(time.Hour))
	xml, _ = tak.MarshalCotEvent(other)
	b.subs[hubmqtt.TAKCotOutFilters()[0]](hubmqtt.TopicTAKCotOutFor("t1", "kit-a"), xml)
	time.Sleep(50 * time.Millisecond)
	if rows, _ := db.ListTAKChatMessages(context.Background(), "t1", "meshsat-0a0b0c0d", 10); len(rows) != 2 {
		t.Errorf("an answer to nobody was stored: %+v", rows)
	}
}

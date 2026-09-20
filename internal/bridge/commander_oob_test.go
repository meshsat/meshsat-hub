package bridge

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/oob"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"
)

type fakeOOB struct {
	peer  *store.OOBPeer
	sent  []string // bearer|cmd
	args  oob.ArgSpec
	reply *oob.Reply
	err   error
}

func (f *fakeOOB) Send(_ context.Context, _, _ string, bearer, cmdName string, args oob.ArgSpec, _ bool) (*oob.Reply, error) {
	f.sent = append(f.sent, bearer+"|"+cmdName)
	f.args = args
	return f.reply, f.err
}
func (f *fakeOOB) ChooseBearer(p *store.OOBPeer, via string, isIMT func(string) bool) (string, error) {
	if via != "" {
		return via, nil
	}
	if p.Phone != "" {
		return oob.BearerSMS, nil
	}
	if isIMT != nil && isIMT(p.SatIMEI) {
		return oob.BearerIMT, nil
	}
	return oob.BearerSBD, nil
}
func (f *fakeOOB) Peer(context.Context, string, string) (*store.OOBPeer, error) { return f.peer, nil }

// Bearer selection: MQTT while online (or forced), the out-of-band leg when
// offline and paired or when a bearer is requested; the reply becomes a
// CommandResponse with the OOB result mapped onto status (MESHSAT-964 C).
func TestSendCommandVia(t *testing.T) {
	c := NewCommander(nil, nil)
	if _, err := c.SendCommandVia(context.Background(), "t1", "tesseract", protocol.Command{Cmd: "mgmt_ping"}, "sms", false); err == nil {
		t.Fatalf("no OOB configured must fail for via=sms")
	}
	f := &fakeOOB{peer: &store.OOBPeer{BridgeID: "tesseract", Phone: "+3160", SatIMEI: "3002"}, reply: &oob.Reply{Bearer: "sms", RC: oob.RCOK, Result: "ok", Body: "u17h b98A", Counter: 4, Received: time.Now()}}
	c.SetOOB(f, func(string) bool { return false })

	// Offline + auto -> SMS (phone present).
	resp, err := c.SendCommandVia(context.Background(), "t1", "tesseract", protocol.Command{Cmd: "mgmt_ping"}, "", false)
	if err != nil || resp.Status != "ok" || len(f.sent) != 1 || f.sent[0] != "sms|mgmt_ping" {
		t.Fatalf("auto offline: %+v %v %v", resp, err, f.sent)
	}
	var res map[string]any
	_ = json.Unmarshal(resp.Result, &res)
	if res["body"] != "u17h b98A" || res["bearer"] != "sms" || res["rc"] != float64(0) {
		t.Errorf("result: %v", res)
	}
	// Forced sbd with args for a reset.
	payload, _ := json.Marshal(map[string]any{"target": "cellular", "level": 2})
	f.reply = &oob.Reply{Bearer: "sbd", RC: oob.RCUnavailable, Result: "unavailable"}
	resp, err = c.SendCommandVia(context.Background(), "t1", "tesseract", protocol.Command{Cmd: "mgmt_reset", Payload: payload}, "sbd", true)
	if err != nil || resp.Status != "unavailable" || f.sent[1] != "sbd|mgmt_reset" || f.args.Target != "cellular" || f.args.Level != 2 {
		t.Fatalf("forced sbd: %+v %v %v %+v", resp, err, f.sent, f.args)
	}
	// Offline, auto, no phone -> satellite by modem type.
	f.peer.Phone = ""
	c.SetOOB(f, func(imei string) bool { return imei == "3002" })
	f.reply = &oob.Reply{Bearer: "imt", RC: oob.RCOK, Result: "ok"}
	if _, err := c.SendCommandVia(context.Background(), "t1", "tesseract", protocol.Command{Cmd: "mgmt_status"}, "", false); err != nil || f.sent[2] != "imt|mgmt_status" {
		t.Fatalf("auto imt: %v %v", err, f.sent)
	}
	// Offline, auto, not paired -> clear error, nothing sent.
	f.peer = nil
	if _, err := c.SendCommandVia(context.Background(), "t1", "ghost", protocol.Command{Cmd: "mgmt_ping"}, "", false); err == nil || len(f.sent) != 3 {
		t.Fatalf("unpaired offline: %v %d", err, len(f.sent))
	}
}

// Every answer says which leg carried it (MESHSAT-964 AC6). The bridge does
// not know the field exists, so the Hub stamps it: "mqtt" on the MQTT path,
// the bearer ChooseBearer picked on the out-of-band one. Without this the
// bearer was only ever a string inside the Result blob on the OOB path and
// absent entirely over MQTT, so the command page could not name it.
func TestCommandResponseNamesItsBearer(t *testing.T) {
	// --- out-of-band legs carry the bearer that was chosen, not the one the
	// reply happened to come back on ---
	for _, tc := range []struct {
		name, via, replyBearer, want string
		phone                        string
	}{
		{name: "auto picks sms", via: "", replyBearer: "sms", want: "sms", phone: "+3160"},
		{name: "forced sbd", via: "sbd", replyBearer: "sbd", want: "sbd", phone: "+3160"},
		{name: "reply on another bearer does not rewrite it", via: "imt", replyBearer: "sms", want: "imt", phone: "+3160"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeOOB{
				peer:  &store.OOBPeer{BridgeID: "tesseract", Phone: tc.phone, SatIMEI: "3002"},
				reply: &oob.Reply{Bearer: tc.replyBearer, RC: oob.RCOK, Result: "ok", Received: time.Now()},
			}
			c := NewCommander(nil, nil)
			c.SetOOB(f, func(string) bool { return false })
			resp, err := c.SendCommandVia(context.Background(), "t1", "tesseract", protocol.Command{Cmd: "mgmt_ping"}, tc.via, false)
			if err != nil {
				t.Fatalf("send: %v", err)
			}
			if resp.Bearer != tc.want {
				t.Errorf("Bearer = %q, want %q", resp.Bearer, tc.want)
			}
		})
	}

	// --- the MQTT path names itself, even though the bridge sent no bearer ---
	mb := newMockBus()
	c := NewCommander(mb, nil)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()

	done := make(chan *protocol.CommandResponse, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := c.SendCommand(ctx, "bridge-01", protocol.Command{Cmd: "ping", RequestID: "req-bearer-1"})
		if err != nil {
			t.Errorf("mqtt send: %v", err)
			done <- nil
			return
		}
		done <- resp
	}()
	time.Sleep(50 * time.Millisecond)

	// Exactly what a bridge publishes: no bearer field at all.
	payload, _ := json.Marshal(&protocol.CommandResponse{
		Protocol: protocol.ProtocolVersion, RequestID: "req-bearer-1", Cmd: "ping",
		Status: "ok", Timestamp: time.Now().UTC(),
	})
	mb.deliver(protocol.TopicBridgeCmdResp("bridge-01"), payload)

	select {
	case resp := <-done:
		if resp == nil {
			t.Fatal("no response")
		}
		if resp.Bearer != ViaMQTT {
			t.Errorf("MQTT leg: Bearer = %q, want %q", resp.Bearer, ViaMQTT)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the MQTT response")
	}
}

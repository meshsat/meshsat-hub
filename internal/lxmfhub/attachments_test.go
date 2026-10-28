package lxmfhub

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/lxmf"
	"github.com/meshsat/meshsat-hub/internal/lxmfhub/lxmfhubtest"
	"github.com/meshsat/meshsat-hub/internal/rnswire"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// The smallest valid WebP: a 1x1 lossless (VP8L) image, 34 bytes.
var testWebP = []byte{
	0x52, 0x49, 0x46, 0x46, 0x1a, 0x00, 0x00, 0x00, 0x57, 0x45, 0x42, 0x50, 0x56, 0x50, 0x38, 0x4c,
	0x0d, 0x00, 0x00, 0x00, 0x2f, 0x00, 0x00, 0x00, 0x10, 0x07, 0x10, 0x11, 0x11, 0x88, 0x88, 0xfe, 0x07, 0x00,
}

func frames(n int, fill byte) []byte { return bytes.Repeat([]byte{fill}, n) }

// A peer's voice note and photo land in the object store under the inbox
// row, the row's fields leave them out, the API reads them back by index,
// the tenant's reply with both goes out in the message's fields and the
// peer reads them intact; the other tenant sees none of it. [MESHSAT-1735]
func TestAttachmentsInboundAndOutbound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newFixture(t, ctx, Config{AnnounceInterval: time.Hour})
	blobs := lxmfhubtest.NewMemBlobs()
	f.svc.SetBlobs(blobs)
	victim, other := store.DefaultTenantID, "other"
	vid, err := f.svc.EnsureIdentity(ctx, victim, "owner@example.org")
	if err != nil {
		t.Fatal(err)
	}
	f.makePublic(t, ctx, victim)
	p := dialPeer(t, ctx, f.addr, "Sideband user")
	waitFor(t, "peer connected", func() bool { return f.node.BridgeCount() == 1 })
	if err := p.router.Announce(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "hub learns the peer", func() bool { return f.node.Node().Paths().Has(p.router.Hash()) })
	f.svc.Reload(victim)
	waitFor(t, "peer learns the victim", func() bool { return p.node.Paths().Has(h16(vid.Address)) })

	// Peer -> victim with both fields; 600 B of frames makes it a resource.
	note := frames(600, 0x11)
	fields, err := lxmf.WithAttachments(lxmf.MapOf(), lxmf.Attachment{Kind: lxmf.AttachmentAudio, Mode: lxmf.AudioMode700C, Data: note},
		lxmf.Attachment{Kind: lxmf.AttachmentImage, Format: "webp", Data: testWebP})
	if err != nil {
		t.Fatal(err)
	}
	sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
	defer scancel()
	if _, _, err := p.router.Send(sctx, h16(vid.Address), []byte("listen"), []byte("note"), fields, 0); err != nil {
		t.Fatalf("peer send: %v", err)
	}
	waitFor(t, "victim inbox", func() bool { return len(f.st.Inbox(victim)) == 1 })
	in := f.st.Inbox(victim)[0]
	if in.Content != "listen" || in.FieldsJSON != "" {
		t.Fatalf("inbox row %+v", in)
	}
	waitFor(t, "attachments filed", func() bool { return f.st.AttachmentCount(victim) == 2 })
	list, _ := f.st.ListLXMFAttachments(ctx, victim, in.ID)
	if len(list) != 2 || list[0].Kind != "image" || list[0].Format != "webp" || list[0].Size != len(testWebP) || list[1].Kind != "audio" || list[1].Mode != 3 || list[1].Size != 600 {
		t.Fatalf("rows %+v", list)
	}
	if keys := blobs.Keys(); len(keys) != 2 || keys[0] != "lxmf-attachments/"+victim+"/"+in.ID+"/0" || blobs.ContentType(keys[1]) != "audio/codec2" || blobs.ContentType(keys[0]) != "image/webp" {
		t.Fatalf("keys %v", keys)
	}
	row, data, err := f.svc.Attachment(ctx, victim, in.ID, 1)
	if err != nil || row.Kind != "audio" || !bytes.Equal(data, note) {
		t.Fatalf("read back: %+v %v", row, err)
	}
	if _, _, err := f.svc.Attachment(ctx, victim, in.ID, 2); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("index 2: %v", err)
	}
	if _, _, err := f.svc.Attachment(ctx, other, in.ID, 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other tenant read it: %v", err)
	}
	if f.st.AttachmentCount(other) != 0 {
		t.Fatal("the other tenant got rows")
	}

	// Victim -> peer with both, through the queue.
	out, err := f.svc.Send(ctx, victim, SendRequest{Destination: p.router.HashHex(), Content: "", Title: "re", Actor: "op@example.org",
		Audio: &lxmf.Attachment{Mode: lxmf.AudioMode700C, Data: frames(500, 0x22)}, Image: &lxmf.Attachment{Format: "WebP", Data: testWebP}})
	if err != nil {
		t.Fatal(err)
	}
	if out.State != store.LXMFStateQueued || out.Content != "" {
		t.Fatalf("queued row %+v", out)
	}
	if list, _ := f.st.ListLXMFAttachments(ctx, victim, out.ID); len(list) != 2 || list[0].Kind != "image" || list[0].Format != "webp" {
		t.Fatalf("outbound rows %+v", list)
	}
	select {
	case got := <-p.got:
		atts := lxmf.AttachmentsOf(got.Fields)
		if string(got.Title) != "re" || len(atts) != 2 || atts[0].Kind != "image" || !bytes.Equal(atts[0].Data, testWebP) ||
			atts[1].Mode != 3 || !bytes.Equal(atts[1].Data, frames(500, 0x22)) {
			t.Fatalf("peer got %+v", atts)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("peer did not receive the hub's message")
	}
	waitFor(t, "delivered state", func() bool { return f.st.States(victim)[out.ID] == store.LXMFStateDelivered })

	// What the queue refuses: over the cap, a bad mode, a bad format, a
	// message with nothing in it.
	for name, bad := range map[string]SendRequest{
		"too large": {Destination: p.router.HashHex(), Image: &lxmf.Attachment{Format: "webp", Data: make([]byte, lxmf.MaxAttachmentBytes+1)}},
		"bad mode":  {Destination: p.router.HashHex(), Audio: &lxmf.Attachment{Mode: 0x0b, Data: note}},
		"bad fmt":   {Destination: p.router.HashHex(), Image: &lxmf.Attachment{Format: "../x", Data: testWebP}},
		"empty":     {Destination: p.router.HashHex()},
	} {
		if _, err := f.svc.Send(ctx, victim, bad); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if n := f.st.AttachmentCount(victim); n != 4 {
		t.Fatalf("refused sends left rows: %d", n)
	}

	// A message whose bytes do not land is failed, not queued.
	blobs.FailPut = errors.New("store down")
	if _, err := f.svc.Send(ctx, victim, SendRequest{Destination: p.router.HashHex(), Content: "x", Audio: &lxmf.Attachment{Mode: 3, Data: note}}); err == nil || !strings.Contains(err.Error(), "store down") {
		t.Fatalf("send with a dead store: %v", err)
	}
	failed, _ := f.st.ListLXMFMessages(ctx, victim, store.LXMFQuery{State: store.LXMFStateFailed})
	if len(failed) != 1 || !strings.Contains(failed[0].Detail, "store down") {
		t.Fatalf("failed rows %+v", failed)
	}
}

// Past the tenant's cap the oldest attachments go, bytes and rows, and the
// other tenant's stay; over the per-message cap a received message keeps its
// text only; without an object store nothing is kept and a send with an
// attachment is refused.
func TestAttachmentsCapsAndNoStore(t *testing.T) {
	ctx := context.Background()
	st := lxmfhubtest.NewMemStore(store.DefaultTenantID, "other")
	svc := New(Config{AttachmentMaxBytes: 1000, AttachmentPrefix: "/att/"}, st, lxmfhubtest.NewMemAccounts(), nil)
	victim, other := store.DefaultTenantID, "other"
	note := func(n int) []lxmf.Attachment {
		return []lxmf.Attachment{{Kind: lxmf.AttachmentAudio, Mode: 3, Data: frames(n, 1)}}
	}
	if err := svc.fileAttachments(ctx, victim, "m1", note(400)); !errors.Is(err, errNoBlobStore) {
		t.Fatalf("without a store: %v", err)
	}
	if _, err := svc.Send(ctx, victim, SendRequest{Destination: strings.Repeat("ab", 16), Audio: &lxmf.Attachment{Mode: 3, Data: frames(10, 1)}}); !errors.Is(err, errNoBlobStore) && err != ErrNoIdentity {
		t.Fatalf("send without a store: %v", err)
	}
	blobs := lxmfhubtest.NewMemBlobs()
	svc.SetBlobs(blobs)
	if svc.cfg.AttachmentPrefix != "att" {
		t.Fatalf("prefix %q", svc.cfg.AttachmentPrefix)
	}
	for i, id := range []string{"m1", "m2", "m3"} {
		svc.now = func() time.Time { return time.Date(2026, 10, 28, 12, i, 0, 0, time.UTC) }
		if err := svc.fileAttachments(ctx, victim, id, note(400)); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.fileAttachments(ctx, other, "m1", note(400)); err != nil {
		t.Fatal(err)
	}
	// 1200 > 1000: m1's note went, bytes and row; m2 and m3 stay.
	if n, _ := st.LXMFAttachmentBytes(ctx, victim); n != 800 || st.AttachmentCount(victim) != 2 {
		t.Fatalf("after eviction: %d bytes, %d rows", n, st.AttachmentCount(victim))
	}
	if _, err := st.GetLXMFAttachment(ctx, victim, "m1", 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("m1's row survived")
	}
	if keys := blobs.Keys(); len(keys) != 3 || keys[0] != "att/"+victim+"/m2/0" || keys[2] != "att/other/m1/0" {
		t.Fatalf("keys %v", keys)
	}
	if st.AttachmentCount(other) != 1 {
		t.Fatal("eviction touched the other tenant")
	}
	// Loading a message whose bytes are gone fails, not silently empty.
	_ = blobs.Delete(ctx, "att/"+victim+"/m2/0")
	if _, err := svc.loadAttachments(ctx, victim, "m2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing bytes: %v", err)
	}
	if atts, err := svc.loadAttachments(ctx, victim, "none"); err != nil || atts != nil {
		t.Fatalf("no attachments: %v %v", atts, err)
	}

	// Over the per-message cap, a received message keeps its text only.
	src, _ := rnswire.GenerateIdentity()
	svc.mu.Lock()
	svc.routers[victim] = &tenantRouter{bucket: newBucket(60, time.Now())}
	svc.mu.Unlock()
	fields := lxmf.MapOf(lxmf.KV{Key: lxmf.Int(lxmf.FieldImage), Val: lxmf.Array(lxmf.Str("webp"), lxmf.Bin(make([]byte, lxmf.MaxAttachmentBytes+1)))})
	m := &lxmf.Message{Source: src.DestHash(lxmf.DeliveryName), Content: []byte("huge"), Fields: fields, Timestamp: float64(time.Now().Unix())}
	if err := m.Pack(src, 0, nil); err != nil {
		t.Fatal(err)
	}
	svc.onMessage(victim, m)
	in := st.Inbox(victim)
	if len(in) != 1 || in[0].Content != "huge" || in[0].FieldsJSON != "" {
		t.Fatalf("inbox %+v", in)
	}
	if list, _ := st.ListLXMFAttachments(ctx, victim, in[0].ID); len(list) != 0 {
		t.Fatalf("over the cap and filed: %+v", list)
	}
}

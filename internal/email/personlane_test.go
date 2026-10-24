package email

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

type recordingReplyLane struct{ got []InboundReply }

func (l *recordingReplyLane) TakeMailReply(_ context.Context, _ string, in InboundReply) bool {
	l.got = append(l.got, in)
	return true
}

func encryptTo(t *testing.T, text string, to ...*openpgp.Entity) string {
	t.Helper()
	var buf bytes.Buffer
	aw, _ := armor.Encode(&buf, "PGP MESSAGE", nil)
	w, err := openpgp.Encrypt(aw, to, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, text)
	_ = w.Close()
	_ = aw.Close()
	return buf.String()
}

// Mailgun forwards inbound mail as multipart/form-data, with a PGP/MIME
// mail's ciphertext as an attachment. The webhook used to read only an
// urlencoded form, so none of this reached it (MESHSAT-1685).
func TestTheWebhookReadsAMailgunPostWithAPGPMIMEPart(t *testing.T) {
	other, _ := openpgp.NewEntity("device", "", "device@example.org", nil)
	block := encryptTo(t, "the code is 4471", other)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range map[string]string{
		"sender":          "ops@example.org",
		"recipient":       "replies@alpha.example",
		"subject":         "Re: Node One via MeshSat [ref ABCD-EFGH-JKLM]",
		"body-plain":      "Version: 1",
		"Message-Id":      "<abc@example.org>",
		"X-Mailgun-Spf":   "Fail",
		"message-headers": `[["Auto-Submitted","auto-replied"],["Subject","x"]]`,
	} {
		_ = mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("attachment-1", "encrypted.asc")
	_, _ = io.WriteString(fw, block)
	_ = mw.Close()

	h := NewWebhookHandler(nil, nil)
	h.SetSecret("s3cret")
	lane := &recordingReplyLane{}
	h.SetReplyLane(lane)
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/email?secret=s3cret", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rr.Code, rr.Body.String())
	}
	if len(lane.got) != 1 {
		t.Fatalf("the lane got %d replies", len(lane.got))
	}
	in := lane.got[0]
	if in.From != "ops@example.org" || in.MessageID != "<abc@example.org>" || !strings.Contains(in.Subject, "[ref ABCD-EFGH-JKLM]") {
		t.Fatalf("fields = %+v", in)
	}
	if in.PGP != strings.TrimSpace(block) || in.Text != "" {
		t.Fatal("a PGP/MIME part encrypted to another key must reach the lane as ciphertext, with no text")
	}
	if !in.AutoReply || !in.AuthFailed {
		t.Fatalf("auto_reply=%v auth_failed=%v; both headers say yes", in.AutoReply, in.AuthFailed)
	}
}

func TestAuthVerdicts(t *testing.T) {
	for _, tc := range []struct {
		fields map[string]string
		failed bool
	}{
		{map[string]string{}, false},
		{map[string]string{"X-Mailgun-Spf": "Fail", "X-Mailgun-Dkim-Check-Result": "Pass"}, false}, // forwarded: DKIM survives
		{map[string]string{"SPF": "softfail"}, true},
		{map[string]string{"SPF": "none"}, false}, // a domain with no policy
		{map[string]string{"Authentication-Results": "mx.example; dkim=fail; spf=fail"}, true},
		{map[string]string{"Authentication-Results": "mx.example; dkim=pass header.d=other.example; dmarc=fail"}, true}, // unaligned pass
	} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Form = map[string][]string{}
		for k, v := range tc.fields {
			req.Form.Set(k, v)
		}
		if got := authFailed(req); got != tc.failed {
			t.Errorf("%v: failed = %v", tc.fields, got)
		}
	}
}

func TestEncryptedOnlyToMe(t *testing.T) {
	kr, err := NewKeyRing("Hub", "hub@alpha.example", "")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := openpgp.NewEntity("device", "", "device@example.org", nil)
	if !kr.EncryptedOnlyToMe(encryptTo(t, "x", kr.hubEntity)) {
		t.Fatal("a message to the tenant's key alone is the hop to the Hub")
	}
	if kr.EncryptedOnlyToMe(encryptTo(t, "x", kr.hubEntity, other)) {
		t.Fatal("a message to the device's key too is end to end; opening it would undo the sender's choice")
	}
	if kr.EncryptedOnlyToMe(encryptTo(t, "x", other)) || kr.EncryptedOnlyToMe("not pgp") {
		t.Fatal("only-to-me must be false for anything else")
	}
}

// smtpSink is a minimal SMTP server: no TLS, records the conversation.
type smtpSink struct {
	ln   net.Listener
	mu   sync.Mutex
	helo string
	data string
	auth bool
}

func newSMTPSink(t *testing.T) *smtpSink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &smtpSink{ln: ln}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *smtpSink) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer func() { _ = c.Close() }()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			r := bufio.NewReader(c)
			say := func(l string) { _, _ = io.WriteString(c, l+"\r\n") }
			say("220 sink ESMTP")
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				cmd := strings.ToUpper(strings.TrimSpace(line))
				switch {
				case strings.HasPrefix(cmd, "EHLO"):
					s.mu.Lock()
					s.helo = strings.TrimSpace(line[5:])
					s.mu.Unlock()
					say("250-sink")
					say("250 AUTH PLAIN")
				case strings.HasPrefix(cmd, "AUTH"):
					s.mu.Lock()
					s.auth = true
					s.mu.Unlock()
					say("235 ok")
				case strings.HasPrefix(cmd, "DATA"):
					say("354 go")
					var b strings.Builder
					for {
						l, err := r.ReadString('\n')
						if err != nil || l == ".\r\n" {
							break
						}
						b.WriteString(l)
					}
					s.mu.Lock()
					s.data = b.String()
					s.mu.Unlock()
					say("250 queued")
				case strings.HasPrefix(cmd, "QUIT"):
					say("221 bye")
					return
				default:
					say("250 ok")
				}
			}
		}(c)
	}
}

func TestTheClientWritesAMailReceiversAccept(t *testing.T) {
	sink := newSMTPSink(t)
	c := NewClient(sink.ln.Addr().String(), "hub@alpha.example", "", "", nil)
	id, err := c.SendMessage(context.Background(), Message{To: "ops@example.org", Subject: "Node Één via MeshSat [ref ABCD-EFGH-JKLM]",
		Body: "need fuel\n.\nline", ReplyTo: "replies@alpha.example", InReplyTo: "<prev@example.org>"})
	if err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.helo != "alpha.example" {
		t.Fatalf("HELO %q, want the sender's domain, never localhost", sink.helo)
	}
	for _, want := range []string{"\r\nDate: ", "\r\nMessage-ID: " + id + "\r\n", "\r\nReply-To: replies@alpha.example\r\n",
		"\r\nIn-Reply-To: <prev@example.org>\r\n", "Subject: =?utf-8?q?", "\r\n..\r\n"} {
		if !strings.Contains(sink.data, want) {
			t.Errorf("message lacks %q:\n%s", want, sink.data)
		}
	}
}

func TestTheClientRefusesCleartextWhenItMatters(t *testing.T) {
	sink := newSMTPSink(t)
	c := NewClient(sink.ln.Addr().String(), "hub@alpha.example", "", "", nil)
	if _, err := c.SendMessage(context.Background(), Message{To: "ops@example.org", Subject: "s", Body: "b", RequireTLS: true}); err == nil {
		t.Fatal("a mail that requires TLS went to a server that offers none")
	}
	c = NewClient(sink.ln.Addr().String(), "hub@alpha.example", "user", "secret", nil)
	if _, err := c.SendMessage(context.Background(), Message{To: "ops@example.org", Subject: "s", Body: "b"}); err == nil {
		t.Fatal("an SMTP password was sent without TLS")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.data != "" || sink.auth {
		t.Fatal("something crossed a cleartext session that must not have")
	}
	if _, err := c.SendMessage(context.Background(), Message{To: "ops@example.org\r\nBcc: x@y.z", Subject: "s", Body: "b"}); err == nil {
		t.Fatal("a line break in a header was accepted")
	}
}

func TestATenantsServerIsNeverInsideTheCluster(t *testing.T) {
	sink := newSMTPSink(t)
	c := NewTenantClient(sink.ln.Addr().String(), "hub@alpha.example", "", "", nil)
	if _, err := c.SendMessage(context.Background(), Message{To: "ops@example.org", Subject: "s", Body: "b"}); err == nil {
		t.Fatal("a tenant's SMTP host on a loopback address was dialled")
	}
}

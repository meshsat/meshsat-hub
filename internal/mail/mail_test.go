package mail

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeRelay is a one-shot SMTP server that records the DATA it was given.
func fakeRelay(t *testing.T) (addr string, got chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got = make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		r := bufio.NewReader(c)
		w := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
		w("220 fake ESMTP")
		var body strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if inData {
				if line == "." {
					got <- body.String()
					w("250 Ok: queued")
					inData = false
					continue
				}
				body.WriteString(line + "\n")
				continue
			}
			switch {
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				w("250-fake")
				w("250 SIZE 10240000")
			case strings.HasPrefix(line, "MAIL FROM"), strings.HasPrefix(line, "RCPT TO"):
				w("250 Ok")
			case line == "DATA":
				w("354 End data with <CR><LF>.<CR><LF>")
				inData = true
			case line == "QUIT":
				w("221 Bye")
				return
			default:
				w("250 Ok")
			}
		}
	}()
	return ln.Addr().String(), got
}

func TestSendProducesAWellFormedMessage(t *testing.T) {
	addr, got := fakeRelay(t)
	s := New(addr, "billing@meshsat.net", "MeshSat Hub", 5*time.Second)
	if s == nil {
		t.Fatal("New returned nil for a configured relay")
	}
	if err := s.Send(context.Background(), "alice@example.com", "Your plan", "Hello Alice,\n\nBody line.\n"); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case msg := <-got:
		for _, want := range []string{
			`From: "MeshSat Hub" <billing@meshsat.net>`,
			"To: <alice@example.com>",
			"Subject: Your plan",
			"Content-Type: text/plain; charset=utf-8",
			"Auto-Submitted: auto-generated",
			"Hello Alice,",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("message missing %q\n---\n%s", want, msg)
			}
		}
		// A message with no Message-ID scores as spam at most receivers, and
		// one added by the relay would fall outside the DKIM signature.
		if !strings.Contains(msg, "Message-ID: <") || !strings.Contains(msg, "@meshsat.net>") {
			t.Errorf("no Message-ID in the sender's own domain:\n%s", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay never received the message")
	}
}

// A newline in a header would let a display name or a plan label add its own
// Bcc. Neither field is free text today, but both carry user-shaped data.
func TestHeaderInjectionIsRefused(t *testing.T) {
	addr, _ := fakeRelay(t)
	s := New(addr, "billing@meshsat.net", "MeshSat Hub", 2*time.Second)
	for _, tc := range []struct{ name, to, subject string }{
		{"subject", "alice@example.com", "Hi\r\nBcc: attacker@example.com"},
		{"recipient", "alice@example.com\r\nRCPT TO:<attacker@example.com>", "Hi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.Send(context.Background(), tc.to, tc.subject, "body"); err == nil {
				t.Fatal("a header with a newline was accepted")
			}
		})
	}
}

// A line that is just "." would end DATA early and truncate the message.
func TestBodyLinesAreDotStuffed(t *testing.T) {
	addr, got := fakeRelay(t)
	s := New(addr, "billing@meshsat.net", "MeshSat Hub", 5*time.Second)
	if err := s.Send(context.Background(), "a@b.example", "s", "before\n.\nafter"); err != nil {
		t.Fatalf("send: %v", err)
	}
	msg := <-got
	if !strings.Contains(msg, "before") || !strings.Contains(msg, "after") {
		t.Fatalf("body was truncated at the lone dot:\n%s", msg)
	}
}

func TestUnconfiguredIsNilAndSafe(t *testing.T) {
	if New("", "billing@meshsat.net", "MeshSat Hub", 0) != nil {
		t.Error("New returned a sender with no relay")
	}
	if New("host:25", "", "MeshSat Hub", 0) != nil {
		t.Error("New returned a sender with no from address")
	}
	var s *SMTP
	if err := s.Send(context.Background(), "a@b.example", "s", "b"); err != ErrNotConfigured {
		t.Errorf("nil sender: %v, want ErrNotConfigured", err)
	}
	// SendOrLog must swallow it: no message here is worth failing a payment,
	// an approval or a lapse over.
	SendOrLog(context.Background(), nil, "a@b.example", "s", "b", "test")
}

func TestGreetingHandlesWhatItIsGiven(t *testing.T) {
	for in, want := range map[string]string{
		"":                  "Hello,",
		"Alice Example":     "Hello Alice,",
		"alice@example.com": "Hello,",
		"  Bob  ":           "Hello Bob,",
	} {
		if got := greeting(in); got != want {
			t.Errorf("greeting(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLapseWarningNamesTheMomentAndCarriesTheClaimCode(t *testing.T) {
	subject, body := LapseWarning("Alice", "crew", time.Now().Add(72*time.Hour), "https://ko-fi.com/x", "AB2K9XYZ")
	if !strings.Contains(subject, "crew") {
		t.Errorf("subject does not name the plan: %q", subject)
	}
	for _, want := range []string{"AB2K9XYZ", "https://ko-fi.com/x", "an SOS is never affected"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// The reassurance matters: a lapse changes the ceiling, not the service.
	if !strings.Contains(body, "keeps working") {
		t.Errorf("body does not say existing kit keeps working:\n%s", body)
	}
}

// Every customer-facing time names an exact instant in a stated zone. The
// messages used to print "tomorrow" beside a date two days out, because the
// word was derived from elapsed hours -- readable, and wrong. A reader in
// another timezone could not tell which was meant (MESHSAT-1004).
func TestEveryStatedTimeIsAnExactMomentInAStatedZone(t *testing.T) {
	// A winter instant and a summer one, so both CET and CEST are exercised
	// and a missing zone database shows up as UTC rather than passing quietly.
	for _, when := range []time.Time{
		time.Date(2027, 1, 15, 22, 30, 5, 0, time.UTC),
		time.Date(2026, 9, 12, 21, 59, 59, 0, time.UTC),
	} {
		want := map[bool]string{true: "CEST", false: "CET"}[when.Month() == time.September]
		got := moment(when)
		if !strings.HasSuffix(got, " "+want) {
			t.Errorf("moment(%s) = %q, want it to end in %s -- is the tzdata import still there?", when, got, want)
		}
		// Weekday, day, month, year and clock all present.
		for _, part := range []string{when.In(nlTime).Format("Monday"), when.In(nlTime).Format("02-Jan-2006"), when.In(nlTime).Format("15:04:05")} {
			if !strings.Contains(got, part) {
				t.Errorf("moment(%s) = %q, missing %q", when, got, part)
			}
		}

		// And no message may state a time any other way.
		for name, pair := range map[string][2]string{
			"LapseWarning": func() [2]string { s, b := LapseWarning("Alice", "crew", when, "u", "C"); return [2]string{s, b} }(),
			"Lapsed":       func() [2]string { s, b := Lapsed("Alice", "crew", when, "u"); return [2]string{s, b} }(),
			"PlanChanged":  func() [2]string { s, b := PlanChanged("Alice", "crew", 24, when, "u"); return [2]string{s, b} }(),
		} {
			if !strings.Contains(pair[0]+pair[1], got) {
				t.Errorf("%s never states the exact moment %q:\nsubject: %s\nbody: %s", name, got, pair[0], pair[1])
			}
			for _, vague := range []string{"tomorrow", "today", " in 1 days", " in 2 days"} {
				if strings.Contains(strings.ToLower(pair[0]+pair[1]), vague) {
					t.Errorf("%s still uses the relative word %q instead of the moment", name, vague)
				}
			}
		}
	}
}

// --- HELO and attachments (MESHSAT-1019) ---------------------------------

// TestEHLOIsFullyQualified pins the fix for a live defect.
//
// Go's net/smtp announces itself as "localhost" unless told otherwise, and the
// relay this Hub submits to enforces reject_non_fqdn_helo_hostname. Hub
// replicas reach it from a different tunnel address per worker node, one of
// which happened to be allowlisted -- so plan-change, lapse and approval mail
// was delivered from one pod and rejected with a 504 from the others, with
// nothing but a line in the relay's log to say so.
func TestEHLOIsFullyQualified(t *testing.T) {
	s := New("relay:2525", "billing@meshsat.net", "MeshSat Hub", 0)
	if s.HELO != "meshsat.net" {
		t.Fatalf("HELO = %q, want the sender's domain", s.HELO)
	}
	if !strings.Contains(s.HELO, ".") {
		t.Fatal("the HELO name is not fully qualified; a strict relay refuses the whole conversation")
	}
	for _, from := range []string{"nodomain", "", "weird@localhost"} {
		if got := heloFor(from); !strings.Contains(got, ".") {
			t.Fatalf("heloFor(%q) = %q, which is not fully qualified", from, got)
		}
	}
}

func TestEHLOIsSentBeforeMailFrom(t *testing.T) {
	got := runFakeRelay(t, func(s *SMTP) error {
		return s.Send(context.Background(), "buyer@example.com", "Subject", "Body")
	})
	ehlo, mailFrom := -1, -1
	for i, line := range got {
		if strings.HasPrefix(line, "EHLO ") || strings.HasPrefix(line, "HELO ") {
			if ehlo < 0 {
				ehlo = i
			}
			if strings.Contains(line, "localhost") {
				t.Fatalf("the client announced itself as localhost: %q", line)
			}
		}
		if strings.HasPrefix(line, "MAIL FROM") && mailFrom < 0 {
			mailFrom = i
		}
	}
	if ehlo < 0 {
		t.Fatalf("no EHLO was sent: %v", got)
	}
	if mailFrom < 0 || ehlo > mailFrom {
		t.Fatalf("EHLO did not precede MAIL FROM: %v", got)
	}
}

func TestSendWithAttachesTheDocument(t *testing.T) {
	s := New("relay:2525", "billing@meshsat.net", "MeshSat Hub", 0)
	msg := s.messageWith("buyer@example.com", "Your MeshSat Hub refund", "Hello Buyer,\n\nBody.",
		Attachment{Filename: "MSHCN2026-0001.pdf", ContentType: "application/pdf",
			Content: []byte("%PDF-1.4 pretend document")})

	if !strings.Contains(msg, "Content-Type: multipart/mixed; boundary=") {
		t.Fatalf("not a multipart message:\n%s", msg)
	}
	if !strings.Contains(msg, `filename="MSHCN2026-0001.pdf"`) {
		t.Fatal("the attachment has no filename")
	}
	if !strings.Contains(msg, "Content-Transfer-Encoding: base64") {
		t.Fatal("the document is not base64 encoded")
	}
	if !strings.Contains(msg, "JVBERi0xLjQ") { // "%PDF-1.4" in base64
		t.Fatalf("the document itself is missing:\n%s", msg)
	}
	if !strings.Contains(msg, "Hello Buyer,") {
		t.Fatal("the words that explain the document are missing")
	}
	// A Message-ID is what keeps these out of spam folders; the multipart path
	// must not lose the headers the plain path gets right.
	if !strings.Contains(msg, "Message-ID: <") || !strings.Contains(msg, "MIME-Version: 1.0") {
		t.Fatalf("the multipart path dropped headers the plain path sets:\n%s", msg)
	}
	// The boundary must actually close.
	b := msg[strings.Index(msg, `boundary="`)+len(`boundary="`):]
	b = b[:strings.IndexByte(b, '"')]
	if strings.Count(msg, "--"+b) < 3 {
		t.Fatalf("the multipart boundary does not open both parts and close: %q", b)
	}
}

func TestAttachmentFilenameCannotInjectAHeader(t *testing.T) {
	s := New("relay:2525", "billing@meshsat.net", "MeshSat Hub", 0)
	msg := s.messageWith("buyer@example.com", "Subject", "Body",
		Attachment{Filename: "a\"\r\nBcc: attacker@example.com\r\nX: b.pdf", Content: []byte("x")})
	// The dangerous part is a NEW header line, not the word appearing inside
	// the quoted filename. Nothing may break out of the one line it belongs on.
	for _, line := range strings.Split(msg, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") || strings.HasPrefix(line, "X:") {
			t.Fatalf("a filename injected a header line %q in:\n%s", line, msg)
		}
	}
	if strings.Count(msg, "Content-Disposition:") != 1 {
		t.Fatalf("the filename split the disposition header:\n%s", msg)
	}
}

// runFakeRelay accepts one SMTP conversation and returns the client's lines.
func runFakeRelay(t *testing.T, send func(*SMTP) error) []string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	lines := make(chan []string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			lines <- nil
			return
		}
		defer func() { _ = conn.Close() }()
		var got []string
		br := bufio.NewReader(conn)
		_, _ = conn.Write([]byte("220 relay ready\r\n"))
		inData := false
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				break
			}
			line = strings.TrimRight(line, "\r\n")
			if inData {
				if line == "." {
					inData = false
					_, _ = conn.Write([]byte("250 queued\r\n"))
				}
				continue
			}
			got = append(got, line)
			switch {
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				_, _ = conn.Write([]byte("250-relay\r\n250 HELP\r\n"))
			case strings.HasPrefix(line, "DATA"):
				inData = true
				_, _ = conn.Write([]byte("354 go ahead\r\n"))
			case strings.HasPrefix(line, "QUIT"):
				_, _ = conn.Write([]byte("221 bye\r\n"))
				lines <- got
				return
			default:
				_, _ = conn.Write([]byte("250 ok\r\n"))
			}
		}
		lines <- got
	}()

	s := New(ln.Addr().String(), "billing@meshsat.net", "MeshSat Hub", 5*time.Second)
	if err := send(s); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case got := <-lines:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("the fake relay never finished the conversation")
		return nil
	}
}

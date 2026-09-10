package mail

import (
	"bufio"
	"context"
	"io"
	"mime/quotedprintable"
	"net"
	"regexp"
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
	if err := s.SendMessage(context.Background(), "alice@example.com",
		Message{Subject: "Your plan", Text: "Hello Alice,\n\nBody line.\n"}); err != nil {
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
			if err := s.SendMessage(context.Background(), tc.to,
				Message{Subject: tc.subject, Text: "body"}); err == nil {
				t.Fatal("a header with a newline was accepted")
			}
		})
	}
}

// A line that is just "." would end DATA early and truncate the message, so it
// is stuffed -- ONCE. net/smtp's DATA writer is a textproto.DotWriter and does
// the stuffing itself; this package used to do it as well, putting two layers
// on the wire of which a receiver strips one, so a body line of "." arrived as
// "..". Every part now goes through one writer that leaves the stuffing alone.
func TestALoneDotIsStuffedExactlyOnce(t *testing.T) {
	addr, got := fakeRelay(t)
	s := New(addr, "billing@meshsat.net", "MeshSat Hub", 5*time.Second)
	if err := s.SendMessage(context.Background(), "a@b.example",
		Message{Subject: "s", Text: "before\n.\nafter"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	msg := <-got
	if !strings.Contains(msg, "before") || !strings.Contains(msg, "after") {
		t.Fatalf("body was truncated at the lone dot:\n%s", msg)
	}
	for _, line := range strings.Split(msg, "\n") {
		if strings.HasPrefix(line, ".") && strings.TrimRight(line, "\r") != ".." {
			t.Fatalf("a lone dot went out as %q; one layer of stuffing is what the receiver undoes", line)
		}
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
	if err := s.SendMessage(context.Background(), "a@b.example", Message{Subject: "s", Text: "b"}); err != ErrNotConfigured {
		t.Errorf("nil sender: %v, want ErrNotConfigured", err)
	}
	// SendOrLog must swallow it: no message here is worth failing a payment,
	// an approval or a lapse over.
	SendOrLog(context.Background(), nil, "a@b.example", Message{Subject: "s", Text: "b"}, "test")
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

func TestLapseWarningNamesTheMomentAndSaysWhatChanges(t *testing.T) {
	m := LapseWarning("Alice", "crew", time.Now().Add(72*time.Hour), "https://ko-fi.com/x")
	if !strings.Contains(m.Subject, "crew") {
		t.Errorf("subject does not name the plan: %q", m.Subject)
	}
	// Both renderings, because a plain-text reader is a customer too and a
	// claim code that only exists in the HTML is a payment that never matches.
	for _, part := range []struct{ name, body string }{{"text", m.Text}, {"html", m.HTML}} {
		for _, want := range []string{"https://ko-fi.com/x", "an SOS is never affected", "keeps working"} {
			if !strings.Contains(part.body, want) {
				t.Errorf("%s part missing %q", part.name, want)
			}
		}
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
		for name, m := range map[string]Message{
			"LapseWarning": LapseWarning("Alice", "crew", when, "u"),
			"Lapsed":       Lapsed("Alice", "crew", when, "u"),
			"PlanChanged":  PlanChanged("Alice", "crew", 24, when, "u"),
		} {
			// The HTML too: a moment stated in one rendering and not the other
			// is how the two disagree in front of a customer.
			for _, part := range []struct{ which, body string }{{"text", m.Text}, {"html", m.HTML}} {
				if !strings.Contains(m.Subject+part.body, got) {
					t.Errorf("%s (%s) never states the exact moment %q:\nsubject: %s\nbody: %s",
						name, part.which, got, m.Subject, part.body)
				}
				for _, vague := range []string{"tomorrow", "today", " in 1 days", " in 2 days"} {
					if strings.Contains(strings.ToLower(m.Subject+part.body), vague) {
						t.Errorf("%s (%s) still uses the relative word %q instead of the moment",
							name, part.which, vague)
					}
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
		return s.SendMessage(context.Background(), "buyer@example.com", Message{Subject: "Subject", Text: "Body"})
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
	msg := s.render("buyer@example.com", Message{Subject: "Your MeshSat Hub refund", Text: "Hello Buyer,\n\nBody."},
		&Attachment{Filename: "MSHCN2026-0001.pdf", ContentType: "application/pdf",
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
	msg := s.render("buyer@example.com", Message{Subject: "Subject", Text: "Body"},
		&Attachment{Filename: "a\"\r\nBcc: attacker@example.com\r\nX: b.pdf", Content: []byte("x")})
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

// --- brand shell (MESHSAT-1019) -------------------------------------------

// allMessages is every message a customer can receive from the Hub. A new one
// added without both renderings fails the tests below rather than reaching an
// inbox as bare text beside a branded receipt.
func allMessages() map[string]Message {
	when := time.Date(2026, 10, 12, 21, 59, 59, 0, time.UTC)
	return map[string]Message{
		"Approved":           Approved("Alice Example", "https://hub.meshsat.net"),
		"PlanChanged":        PlanChanged("Alice", "crew", 24, when, "https://hub.meshsat.net"),
		"PlanChangedCustom":  PlanChanged("Alice", "custom", -1, when, "https://hub.meshsat.net"),
		"LapseWarning":       LapseWarning("Alice", "crew", when, "https://ko-fi.com/x"),
		"LapseWarningNoCode": LapseWarning("Alice", "crew", when, "https://ko-fi.com/x"),
		"Lapsed":             Lapsed("Alice", "crew", when, "https://ko-fi.com/x"),
		"Refunded":           Refunded("Alice", "9.00 EUR", "MSHCN2026-0001", "MSH2026-0001", when, "https://hub.meshsat.net"),
		"RefundedPartial":    Refunded("Alice", "4.00 EUR", "MSHCN2026-0002", "MSH2026-0001", time.Time{}, "https://hub.meshsat.net"),
		"RefundedNoDocument": RefundedNoDocument("Alice", "9.00 EUR", when, "https://hub.meshsat.net"),
	}
}

// The Hub's notice and the billing system's receipt arrive in the same inbox,
// minutes apart, about the same money. They used to look like two different
// companies: Invoice Ninja sends a branded shell, everything here was bare
// text. Both renderings now exist for every message and the HTML is the
// billing system's own wrapper.
func TestEveryMessageCarriesTheBrandShellAndAPlainTextTwin(t *testing.T) {
	for name, m := range allMessages() {
		if m.Subject == "" || m.Text == "" || m.HTML == "" {
			t.Errorf("%s: subject/text/html = %q/%d bytes/%d bytes; all three are required",
				name, m.Subject, len(m.Text), len(m.HTML))
			continue
		}
		for _, marker := range []string{
			`alt="MeshSat Hub"`,                // the lockup, from company 2
			"data:image/png;base64,",           // embedded, so no tracker and no remote fetch
			"background:#14120F",               // the header ground
			"MeshSat Hub &middot; meshsat.net", // the footer
		} {
			if !strings.Contains(m.HTML, marker) {
				t.Errorf("%s: HTML is not inside the brand shell, missing %q", name, marker)
			}
		}
		// The text part is the message, not a stub telling somebody to find a
		// better mail client.
		if strings.Contains(m.Text, "<") || strings.Contains(strings.ToLower(m.Text), "view this") {
			t.Errorf("%s: the text part carries markup or a fallback apology:\n%s", name, m.Text)
		}
		// Same sign-off in both, so they read as one message.
		if !strings.Contains(m.Text, "The MeshSat team") || !strings.Contains(m.HTML, "The MeshSat team") {
			t.Errorf("%s: the two renderings do not share the sign-off", name)
		}
	}
}

// Whatever the shell does, it must not fetch anything: a remote image is a
// tracker, and it is also the thing that renders as a broken box for the many
// readers whose client blocks images.
func TestTheShellFetchesNothing(t *testing.T) {
	for _, scheme := range []string{"http://", "https://", "//fonts.", "url("} {
		if strings.Contains(shell, scheme) {
			t.Errorf("the shell references a remote resource (%q); it must be self-contained", scheme)
		}
	}
	if strings.Count(shell, bodyPlaceholder) != 1 {
		t.Fatalf("the shell has %d body placeholders, want exactly 1", strings.Count(shell, bodyPlaceholder))
	}
	if strings.Contains(Wrap("<p>x</p>"), bodyPlaceholder) {
		t.Error("Wrap left the placeholder in place")
	}
}

// A display name comes from an identity provider and a plan label from a
// payment payload. Neither is ours, and an unescaped one is script in
// somebody's inbox.
func TestCustomerSuppliedTextCannotInjectMarkup(t *testing.T) {
	hostile := `Mallory<script>alert(1)</script>`
	m := Approved(hostile, "https://hub.meshsat.net")
	if strings.Contains(m.HTML, "<script>") {
		t.Fatalf("a display name reached the HTML unescaped:\n%s", m.HTML)
	}
	if !strings.Contains(m.HTML, "&lt;script&gt;") {
		t.Fatalf("the name was dropped rather than escaped:\n%s", m.HTML)
	}
	p := PlanChanged("Alice", `crew"><img src=x onerror=alert(1)>`, 24, time.Now(), "https://hub.meshsat.net")
	if strings.Contains(p.HTML, "<img src=x") {
		t.Fatalf("a plan label reached the HTML unescaped:\n%s", p.HTML)
	}
}

// multipart/alternative says "the same content twice, pick one". The order is
// not cosmetic: RFC 2046 makes the LAST part the richest, and clients choose
// accordingly, so a plain part written second would show markup to everyone.
func TestHTMLMessagesGoOutAsAlternativeWithTextFirst(t *testing.T) {
	s := New("relay:2525", "billing@meshsat.net", "MeshSat Hub", 0)
	msg := s.render("buyer@example.com", Approved("Alice", "https://hub.meshsat.net"), nil)

	if !strings.Contains(msg, "Content-Type: multipart/alternative; boundary=") {
		t.Fatalf("an HTML message did not go out as multipart/alternative:\n%s", head(msg))
	}
	plain := strings.Index(msg, "Content-Type: text/plain; charset=utf-8")
	rich := strings.Index(msg, "Content-Type: text/html; charset=utf-8")
	if plain < 0 || rich < 0 {
		t.Fatalf("one of the two parts is missing (plain=%d html=%d)", plain, rich)
	}
	if plain > rich {
		t.Fatal("the HTML part comes first, so a client picking the last alternative shows plain text")
	}
	b := boundaryOf(t, msg)
	if strings.Count(msg, "--"+b) < 3 {
		t.Fatalf("the alternative does not open both parts and close: %q", b)
	}
	if !strings.HasSuffix(strings.TrimRight(msg, "\r\n"), "--"+b+"--") {
		t.Fatalf("the message does not end with the closing boundary:\n%s", tail(msg))
	}
}

// A credit note is two renderings of the words AND a document. mixed is the
// outer wrapper and alternative the inner one; the other way round makes a
// reader choose between the letter and the PDF.
func TestCreditNoteNestsTheAlternativeInsideTheMixed(t *testing.T) {
	s := New("relay:2525", "billing@meshsat.net", "MeshSat Hub", 0)
	m := Refunded("Alice", "9.00 EUR", "MSHCN2026-0001", "MSH2026-0001",
		time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC), "https://hub.meshsat.net")
	msg := s.render("buyer@example.com", m, &Attachment{
		Filename: "Credit_MSHCN2026-0001.pdf", ContentType: "application/pdf",
		Content: []byte("%PDF-1.4 pretend document"),
	})

	mixed := strings.Index(msg, "Content-Type: multipart/mixed;")
	alt := strings.Index(msg, "Content-Type: multipart/alternative;")
	pdf := strings.Index(msg, "Content-Type: application/pdf")
	if mixed < 0 || alt < 0 || pdf < 0 {
		t.Fatalf("mixed=%d alternative=%d pdf=%d; all three are required:\n%s", mixed, alt, pdf, head(msg))
	}
	if mixed >= alt || alt >= pdf {
		t.Fatal("the nesting is wrong: mixed must contain the alternative, and the PDF must follow it")
	}
	if !strings.Contains(msg, "JVBERi0xLjQ") { // "%PDF-1.4" in base64
		t.Fatal("the document itself is missing")
	}
	if !strings.Contains(msg, "MSHCN2026-0001") {
		t.Fatal("the credit note number is in neither rendering")
	}
}

// The shell carries a 32 KB base64 logo on a single line. SMTP's limit is 998
// octets and a raw 8-bit send would blow straight through it -- the message
// would arrive mangled, or not at all.
func TestTheHTMLPartIsEncodedForSMTPLineLimits(t *testing.T) {
	s := New("relay:2525", "billing@meshsat.net", "MeshSat Hub", 0)
	msg := s.render("buyer@example.com", Approved("Alice", "https://hub.meshsat.net"), nil)
	if !strings.Contains(msg, "Content-Transfer-Encoding: quoted-printable") {
		t.Fatal("the HTML part is not encoded")
	}
	for i, line := range strings.Split(msg, "\r\n") {
		if len(line) > 998 {
			t.Fatalf("line %d is %d octets, over SMTP's 998 limit", i+1, len(line))
		}
	}
	// And it must decode back to exactly the shell the customer should see. A
	// hand-rolled encoder that only looks right is the failure mode here.
	body := msg[strings.Index(msg, "Content-Transfer-Encoding: quoted-printable"):]
	body = body[strings.Index(body, "\r\n\r\n")+4:]
	if i := strings.Index(body, "\r\n--"); i >= 0 {
		body = body[:i]
	}
	decoded, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(body)))
	if err != nil {
		t.Fatalf("the HTML part does not decode as quoted-printable: %v", err)
	}
	// Line endings become CRLF on the wire, which is the MIME rule, so compare
	// on the text rather than the terminators.
	norm := func(x string) string {
		return strings.TrimRight(strings.ReplaceAll(x, "\r\n", "\n"), "\n")
	}
	want := Approved("Alice", "https://hub.meshsat.net").HTML
	if norm(string(decoded)) != norm(want) {
		t.Errorf("the decoded HTML is not what was built (%d bytes decoded, %d built)",
			len(decoded), len(want))
	}
}

func boundaryOf(t *testing.T, msg string) string {
	t.Helper()
	i := strings.Index(msg, `boundary="`)
	if i < 0 {
		t.Fatalf("no boundary in:\n%s", head(msg))
	}
	b := msg[i+len(`boundary="`):]
	return b[:strings.IndexByte(b, '"')]
}

func head(s string) string {
	if len(s) > 1500 {
		return s[:1500] + "\n...[truncated]"
	}
	return s
}

func tail(s string) string {
	if len(s) > 400 {
		return "...[truncated]\n" + s[len(s)-400:]
	}
	return s
}

// The two renderings must not disagree in front of a customer. Every URL and
// every document or claim reference that appears in one has to appear in the
// other: a renewal link only in the HTML strands a plain-text reader, and a
// credit note number only in the text makes the branded copy look incomplete.
func TestTheTwoRenderingsAgreeOnEveryReference(t *testing.T) {
	token := regexp.MustCompile(`https://[^\s<"]+|MSHC?N?2026-\d{4}|\b[A-Z0-9]{8}\b`)
	for name, m := range allMessages() {
		body := bodyOf(t, m.HTML)
		for _, want := range token.FindAllString(m.Text, -1) {
			want = strings.TrimRight(want, ".,:;")
			if !strings.Contains(body, want) {
				t.Errorf("%s: %q is in the text part but not the HTML", name, want)
			}
		}
		for _, want := range token.FindAllString(body, -1) {
			want = strings.TrimRight(want, `".,:;`)
			if !strings.Contains(m.Text, want) {
				t.Errorf("%s: %q is in the HTML part but not the text", name, want)
			}
		}
	}
}

// bodyOf strips the shell, leaving only what the message itself contributed.
// Scanning the whole thing would match runs inside the base64 logo.
func bodyOf(t *testing.T, full string) string {
	t.Helper()
	pre, post, ok := strings.Cut(shell, bodyPlaceholder)
	if !ok {
		t.Fatal("the shell has no body placeholder")
	}
	return strings.TrimSuffix(strings.TrimPrefix(full, pre), post)
}

// A customer's name is whatever their identity provider holds, and half of
// Europe's are not ASCII. text/plain with no Content-Transfer-Encoding means
// 7bit, so an unencoded name with an accent is 8-bit octets in a part that
// claims to have none.
func TestANonASCIINameIsEncodedInBothParts(t *testing.T) {
	s := New("relay:2525", "billing@meshsat.net", "MeshSat Hub", 0)
	m := Approved("José Grüße", "https://hub.meshsat.net")
	if !strings.Contains(m.Text, "José") {
		t.Fatalf("the name did not survive into the text: %q", m.Text[:40])
	}
	msg := s.render("buyer@example.com", m, nil)

	for i, line := range strings.Split(msg, "\r\n") {
		for j := 0; j < len(line); j++ {
			if line[j] >= 0x80 {
				t.Fatalf("raw 8-bit octet on line %d: %q", i+1, line)
			}
		}
	}
	if strings.Count(msg, "Content-Transfer-Encoding: quoted-printable") != 2 {
		t.Fatalf("both parts should be encoded when the text is not ASCII:\n%s", head(msg))
	}
	// And an ordinary ASCII message stays readable in a raw spool file.
	plain := s.render("buyer@example.com", Message{Subject: "s", Text: "Hello Alice,\n\nBody."}, nil)
	if strings.Contains(plain, "Content-Transfer-Encoding") {
		t.Errorf("an all-ASCII plain message was encoded for no reason:\n%s", plain)
	}
	if !strings.Contains(plain, "Hello Alice,") {
		t.Error("the all-ASCII body is no longer legible in the raw message")
	}
}

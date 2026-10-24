package email

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/mail"
	"strings"
)

// What the mail services pass on, read for the "@" lane (MESHSAT-1685).
// Mailgun posts some headers as fields and all of them as a JSON list in
// "message-headers"; SendGrid's inbound parse posts the raw header block in
// "headers". A header is looked up in all three places.

// inboundHeader returns the first value of a header, or "".
func inboundHeader(r *http.Request, name string) string {
	if v := strings.TrimSpace(r.FormValue(name)); v != "" {
		return v
	}
	if raw := r.FormValue("message-headers"); raw != "" {
		var pairs [][]string
		if json.Unmarshal([]byte(raw), &pairs) == nil {
			for _, p := range pairs {
				if len(p) == 2 && strings.EqualFold(p[0], name) {
					return strings.TrimSpace(p[1])
				}
			}
		}
	}
	for line := range strings.SplitSeq(r.FormValue("headers"), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// inboundMessageID reads the mail's Message-Id.
func inboundMessageID(r *http.Request) string {
	for _, k := range []string{"Message-Id", "message-id", "message_id"} {
		if v := inboundHeader(r, k); v != "" {
			return v
		}
	}
	return ""
}

// autoReply: RFC 3834's Auto-Submitted, the older Precedence and X-Autoreply
// markers, and the subjects the common clients give an out-of-office reply.
// An out-of-office keeps the subject, reference and all, so without this
// every mail a device sends would cost one paid satellite message back.
func autoReply(r *http.Request, subject string) bool {
	if v := strings.ToLower(inboundHeader(r, "Auto-Submitted")); v != "" && v != "no" {
		return true
	}
	switch strings.ToLower(inboundHeader(r, "Precedence")) {
	case "bulk", "list", "junk", "auto_reply":
		return true
	}
	for _, h := range []string{"X-Autoreply", "X-Autorespond", "X-Autoresponder"} {
		if inboundHeader(r, h) != "" {
			return true
		}
	}
	if inboundHeader(r, "List-Id") != "" || inboundHeader(r, "List-Unsubscribe") != "" {
		return true
	}
	s := strings.ToLower(strings.TrimSpace(subject))
	for _, p := range []string{"auto:", "automatic reply", "autoreply", "out of office", "afwezig", "abwesenheit", "réponse automatique"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// authFailed: the mail service checked the sender's domain, something
// FAILED and nothing passed. A DKIM pass is enough on its own (a forwarded
// mail fails SPF and keeps its signature). No verdict, or only "none" and
// "neutral" (a small domain that publishes no policy), is not a failure: the
// conversation's reference and sender still have to match.
func authFailed(r *http.Request) bool {
	var verdicts []string
	for _, h := range []string{"X-Mailgun-Spf", "X-Mailgun-Dkim-Check-Result", "SPF", "dkim", "Authentication-Results"} {
		if v := strings.ToLower(inboundHeader(r, h)); v != "" {
			verdicts = append(verdicts, v)
		}
	}
	if len(verdicts) == 0 {
		return false
	}
	all := strings.Join(verdicts, " ")
	// DMARC is the verdict about the From line itself (an aligned DKIM or
	// SPF pass); a fail there outranks an unaligned pass of either.
	if strings.Contains(all, "dmarc=fail") {
		return true
	}
	if strings.Contains(all, "pass") {
		return false
	}
	return strings.Contains(all, "fail")
}

// armouredBlock returns the first complete armoured OpenPGP message in text.
func armouredBlock(text string) (string, bool) {
	const begin, end = "-----BEGIN PGP MESSAGE-----", "-----END PGP MESSAGE-----"
	i := strings.Index(text, begin)
	if i < 0 {
		return "", false
	}
	j := strings.Index(text[i:], end)
	if j < 0 {
		return "", false
	}
	return text[i : i+j+len(end)], true
}

// pgpPart finds the encrypted part of a PGP/MIME mail (RFC 3156), which
// Thunderbird, Proton and Mailvelope send by default: the mail service posts
// it as an attachment ("encrypted.asc", application/octet-stream), and the
// text body holds only "Version: 1".
func pgpPart(r *http.Request) string {
	if r.MultipartForm == nil {
		return ""
	}
	for _, files := range r.MultipartForm.File {
		for _, fh := range files {
			if fh.Size > 1<<20 {
				continue
			}
			if b := readPart(fh); b != "" {
				if block, ok := armouredBlock(b); ok {
					return block
				}
			}
		}
	}
	return ""
}

func readPart(fh *multipart.FileHeader) string {
	f, err := fh.Open()
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return ""
	}
	return string(b)
}

// mimeText is the text of a decrypted PGP/MIME payload, which is itself a
// MIME entity (often multipart/mixed with protected headers); the first
// text/plain part, or the input when it is not MIME at all.
func mimeText(decrypted string) string {
	msg, err := mail.ReadMessage(strings.NewReader(decrypted))
	if err != nil || msg.Header.Get("Content-Type") == "" {
		return decrypted
	}
	return partText(msg.Header.Get("Content-Type"), msg.Body, 0)
}

func partText(contentType string, body io.Reader, depth int) string {
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil || depth > 4 {
		return ""
	}
	if strings.HasPrefix(mt, "multipart/") {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				return ""
			}
			ct := p.Header.Get("Content-Type")
			if ct == "" {
				ct = "text/plain"
			}
			if t := partText(ct, p, depth+1); t != "" {
				return t
			}
		}
	}
	if mt != "text/plain" {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return ""
	}
	return string(b)
}

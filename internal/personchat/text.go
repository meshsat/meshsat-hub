package personchat

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// replyHeaders are the lines a mail client puts above the message it quotes.
// Everything from the first of them on is the Hub's own mail coming back, and
// on a satellite MT every byte of it is paid for. The common clients'
// phrasings in the languages the kits are used in; a client this misses sends
// its quote along, cut to the device's limit, which wastes bytes and loses
// nothing.
var replyHeaders = []*regexp.Regexp{
	regexp.MustCompile(`^On .{0,200}wrote:?$`),
	regexp.MustCompile(`^Op .{0,200}schreef.{0,40}:?$`),
	regexp.MustCompile(`^Am .{0,200}schrieb.{0,40}:?$`),
	regexp.MustCompile(`^Le .{0,200}a écrit\s?:?$`),
	regexp.MustCompile(`^El .{0,200}escribió:?$`),
	regexp.MustCompile(`^Στις .{0,200}έγραψε:?$`),
	regexp.MustCompile(`^-{2,}\s?Original Message\s?-{2,}$`),
	regexp.MustCompile(`^-{2,}\s?Oorspronkelijk bericht\s?-{2,}$`),
	regexp.MustCompile(`^_{10,}$`),
	regexp.MustCompile(`^From: .+`),
	regexp.MustCompile(`^Van: .+`),
	regexp.MustCompile(`^Von: .+`),
	regexp.MustCompile(`^Sent from my .+`),
}

// ReplyText is the new part of a mail reply: the lines above the quoted
// original, the client's reply header and the signature separator, with
// trailing blank lines dropped and runs of blank lines folded to one.
func ReplyText(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	var out []string
	for _, line := range strings.Split(body, "\n") {
		l := strings.TrimRight(line, " \t")
		if strings.HasPrefix(l, ">") || l == "--" || l == "-- " {
			break
		}
		header := false
		for _, re := range replyHeaders {
			if re.MatchString(strings.TrimSpace(l)) {
				header = true
				break
			}
		}
		if header {
			break
		}
		out = append(out, l)
	}
	var b strings.Builder
	blank := false
	for _, l := range out {
		if strings.TrimSpace(l) == "" {
			blank = b.Len() > 0
			continue
		}
		if blank {
			b.WriteString("\n")
			blank = false
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(l)
	}
	return strings.TrimSpace(b.String())
}

// Cut returns s cut to at most max bytes at a rune boundary, with "…" (three
// bytes) at the end when anything was cut. A plain body only: an end-to-end
// one is never cut.
func Cut(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ellipsis = "…"
	n := max - len(ellipsis)
	if n <= 0 {
		return ""
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return strings.TrimRight(s[:n], " \n") + ellipsis
}

// Printable drops what must not reach a device's screen or a person's phone
// from a plain body: control characters other than newline and tab, and the
// Unicode format and bidi controls a text can hide behind.
func Printable(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			return -1
		case r == 0x200e || r == 0x200f || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0xfeff:
			return -1
		case r == utf8.RuneError:
			return -1
		}
		return r
	}, s)
}

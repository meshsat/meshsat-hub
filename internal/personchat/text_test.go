package personchat

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReplyText(t *testing.T) {
	cases := map[string]string{
		"On my way, 20 min.\r\n\r\nOn Sat, 24 Oct 2026 at 10:00, MeshSat <hub@x.org> wrote:\r\n> water at the gate\r\n": "On my way, 20 min.",
		"ok\n\n\n\nsecond\n-- \nAlice\nOps":                                               "ok\n\nsecond",
		"Komen eraan\n\nOp za 24 okt 2026 om 10:00 schreef MeshSat <hub@x.org>:\n> water": "Komen eraan",
		"yes\n\n-----Original Message-----\nFrom: x":                                      "yes",
		"yes\n\nSent from my iPhone":                                                      "yes",
		"> only a quote":                                                                  "",
	}
	for in, want := range cases {
		if got := ReplyText(in); got != want {
			t.Errorf("ReplyText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCut(t *testing.T) {
	if Cut("short", 10) != "short" {
		t.Fatal("a short text is not cut")
	}
	got := Cut(strings.Repeat("é", 100), 51)
	if len(got) > 51 || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Fatalf("Cut must stay within the limit at a rune boundary and mark the cut: %q (%d)", got, len(got))
	}
}

func TestPrintable(t *testing.T) {
	in := "a\x00b\x1b[31mc\u202ed\ne\tf\u0085"
	if got := Printable(in); got != "ab[31mcd\ne\tf" {
		t.Fatalf("Printable = %q", got)
	}
}

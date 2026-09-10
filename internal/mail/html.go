package mail

import (
	_ "embed"
	"fmt"
	"html"
	"strings"
)

// The Hub's messages and the billing system's receipts arrive in the same
// inbox, minutes apart, about the same money. They used to look like they came
// from two different companies: Invoice Ninja sends a branded HTML shell with
// the MeshSat Hub lockup, and everything the Hub sent was bare plain text.
//
// shell.html is that same wrapper, byte for byte from company 2's
// `email_style_custom` with its one `$body` placeholder renamed. Keeping a copy
// rather than fetching it at send time is deliberate: a cosmetic asset must not
// make customer mail depend on the billing system being reachable. The logo
// travels as an embedded base64 PNG, so a message has zero remote resources and
// is tracker-free by construction -- and it renders with the images-off default
// that many clients still use.
//
//go:embed shell.html
var shell string

const bodyPlaceholder = "{{BODY}}"

// Brand values lifted from the same wrapper, so the two never drift.
const (
	textColour  = "#221F1B"
	mutedColour = "#6B6055"
	ruleColour  = "#E4DED6"
	linkColour  = "#C4470A" // the Hub's light-theme accent; 4.94:1 on white
)

// Wrap puts already-built body HTML inside the shared shell.
func Wrap(bodyHTML string) string {
	return strings.Replace(shell, bodyPlaceholder, bodyHTML, 1)
}

// para is one paragraph in the shell's own rhythm.
func para(inner string) string {
	return `<p style="margin:0 0 16px;">` + inner + `</p>`
}

// lastPara closes a message without trailing space, matching the receipt
// template's sign-off.
func lastPara(inner string) string {
	return `<p style="margin:0;">` + inner + `</p>`
}

// esc escapes text that came from a customer, a plan name or a payment payload.
// Every value interpolated into these messages goes through it: a display name
// is attacker-influenced, and an unescaped one is script in somebody's inbox.
func esc(s string) string { return html.EscapeString(s) }

// strong marks the one figure or moment a reader looks for.
func strong(s string) string { return "<strong>" + s + "</strong>" }

// link renders a URL the reader is meant to act on. The href is escaped as an
// attribute and the visible text shows the URL itself, because a branded button
// that hides where it goes is exactly what a phishing message looks like.
func link(url string) string {
	e := esc(url)
	return fmt.Sprintf(`<a href="%s" style="color:%s;text-decoration:underline;">%s</a>`, e, linkColour, e)
}

// button renders the one action a message is asking for.
//
// The style is lifted from the enrollment and recovery templates in
// k8s/scripts/authentik/templates/email/, which are the first branded messages
// a customer ever receives -- filled Signal Orange, white label, 4px radius. A
// third button style in the same inbox would undo the point of this file.
func button(url, label string) string {
	return `<p style="margin:0 0 8px;"><a href="` + esc(url) + `" style="display:inline-block;` +
		`background:` + linkColour + `;color:#FFFFFF;text-decoration:none;padding:12px 22px;` +
		`border-radius:4px;font-weight:600;">` + esc(label) + `</a></p>`
}

// urlUnder puts the destination in readable text below a button. A branded
// button that hides where it goes is what a phishing message looks like, and it
// is also what a plain-text-only reader would otherwise lose.
func urlUnder(url string) string {
	return `<p style="margin:0 0 20px;font-size:13px;color:` + mutedColour +
		`;word-break:break-all;">` + esc(url) + `</p>`
}

// factTable renders label/value rows in the same border and spacing the
// billing system uses for an invoice or credit note summary.
func factTable(rows [][2]string) string {
	var b strings.Builder
	b.WriteString(`<table role="presentation" border="0" cellpadding="0" cellspacing="0" width="100%" ` +
		`style="margin:0 0 22px;border:1px solid ` + ruleColour + `;">`)
	for i, r := range rows {
		border := "border-bottom:1px solid " + ruleColour + ";"
		if i == len(rows)-1 {
			border = "border-bottom:0;"
		}
		b.WriteString(`<tr><td style="padding:10px 16px;font-size:13px;color:` + mutedColour + `;` + border + `">`)
		b.WriteString(r[0])
		b.WriteString(`</td><td align="right" style="padding:10px 16px;font-size:14px;color:` + textColour + `;` + border + `">`)
		b.WriteString(`<strong>` + r[1] + `</strong>`)
		b.WriteString(`</td></tr>`)
	}
	b.WriteString(`</table>`)
	return b.String()
}

// note is the small-print line the shell uses for secondary information.
func note(inner string) string {
	return `<p style="margin:0 0 16px;font-size:13px;color:` + mutedColour + `;">` + inner + `</p>`
}

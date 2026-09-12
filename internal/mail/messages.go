package mail

import (
	"fmt"
	"strings"
	"time"

	// The zone database is embedded in the binary. The Hub runs in a scratch
	// container with no /usr/share/zoneinfo, where LoadLocation fails and
	// every customer-facing time would silently fall back to UTC -- an hour
	// or two off, with no way for the reader to tell.
	_ "time/tzdata"
)

// The Hub's transactional messages. Short, written for the person receiving
// them rather than for the system sending them, and built twice: once as plain
// text and once inside the shared brand shell. Terms promise notice before a
// change takes effect, so these are part of keeping that.
//
// Both renderings say the same thing. The text is not a fallback stub -- it is
// the message, and every fact in the HTML is in it, because a reader on a
// plain-text client is a customer like any other and a message that reads
// "please view this in a modern email client" is a failure.

// Approved tells someone their account request was accepted. Until this existed
// only the CLI approval path sent anything, and it sent from the shared identity
// provider's default address, which belongs to a different product.
func Approved(name, hubURL string) Message {
	text := fmt.Sprintf(`%s

Your MeshSat Hub account has been approved. You can sign in here:

  %s

Your workspace is created the first time you sign in. You start on the free
plan, which covers four devices and bridges together; you can see your usage
and move up a plan on the Settings page.

If you did not ask for an account, reply to this message and we will remove it.

The MeshSat team`, greeting(name), hubURL)

	html := para(esc(greeting(name))) +
		para("Your MeshSat Hub account has been approved.") +
		button(hubURL, "Sign in to MeshSat Hub") +
		urlUnder(hubURL) +
		para("Your workspace is created the first time you sign in. You start on the free plan, "+
			"which covers four devices and bridges together; you can see your usage and move up a "+
			"plan on the Settings page.") +
		note("If you did not ask for an account, reply to this message and we will remove it.") +
		lastPara("The MeshSat team")

	return Message{
		Subject: "Your MeshSat Hub account is ready",
		Text:    text,
		HTML:    Wrap(html),
	}
}

// PlanChanged confirms a payment was applied, so the customer has something
// from us and not only from the payment processor.
func PlanChanged(name, plan string, devices int, expires time.Time, hubURL string) Message {
	limit := fmt.Sprintf("%d devices and bridges together", devices)
	cell := fmt.Sprintf("%d together", devices)
	if devices < 0 {
		limit = "an unlimited number of devices and bridges"
		cell = "unlimited"
	}
	when := moment(expires)

	text := fmt.Sprintf(`%s

Thank you. Your payment has been applied and your plan is now %s, which covers
%s.

It runs to %s.

It renews on its own until you cancel it, and you can cancel or change your
card yourself from the Settings page at any time. Cancelling stops the next
renewal: you keep the plan until %s, which you have already paid for. Nothing
you have registered is ever deleted or stops reporting.

Your usage is on the Settings page:

  %s

Your receipt, with the VAT included in the price, is sent separately.

The MeshSat team`, greeting(name), plan, limit, when, when, hubURL)

	html := para(esc(greeting(name))) +
		para("Thank you. Your payment has been applied.") +
		factTable([][2]string{
			{"Plan", esc(plan)},
			{"Devices and bridges", esc(cell)},
			{"Runs to", esc(when)},
		}) +
		para("It renews on its own until you cancel it, and you can cancel or change your card "+
			"yourself from the Settings page at any time. Cancelling stops the next renewal: you "+
			"keep the plan until "+esc(when)+", which you have already paid for. Nothing you have "+
			"registered is ever deleted or stops reporting.") +
		para("Your usage is on the Settings page: "+link(hubURL)) +
		note("Your receipt, with the VAT included in the price, is sent separately.") +
		lastPara("The MeshSat team")

	return Message{
		Subject: fmt.Sprintf("Your MeshSat Hub plan is now %s", plan),
		Text:    text,
		HTML:    Wrap(html),
	}
}

// PaymentFailed tells a customer their renewal did not go through, while
// something can still be done about it.
//
// The tone matters more here than anywhere else in this file. Nothing is lost
// yet: the plan is untouched, every device keeps reporting, and an SOS is
// unaffected -- and a customer who reads this as "your service is off" may go
// and buy somebody else's. So it says what is still true before it says what
// is wrong, and it never uses the word suspended.
//
// retryAt is zero when the provider has stopped retrying, which is the one
// case where the customer has to act rather than wait.
func PaymentFailed(name, plan, amount string, retryAt, planEnds time.Time, hubURL string) Message {
	facts := [][2]string{
		{"Plan", esc(plan)},
		{"Amount", esc(amount)},
	}
	var whatNext, whatNextHTML string
	if retryAt.IsZero() {
		whatNext = "We have stopped trying, so this one needs you: update your card on the " +
			"Settings page and the payment will go through."
		whatNextHTML = "We have stopped trying, so this one needs you: update your card on the " +
			"Settings page and the payment will go through."
	} else {
		facts = append(facts, [2]string{"Next attempt", esc(moment(retryAt))})
		whatNext = "We will try again automatically. If the card has changed, you can update it " +
			"on the Settings page and there is nothing else to do."
		whatNextHTML = whatNext
	}
	untilText, untilHTML := "", ""
	if !planEnds.IsZero() {
		facts = append(facts, [2]string{"Plan runs to", esc(moment(planEnds))})
		untilText = fmt.Sprintf("\n\nYour plan runs to %s either way.", moment(planEnds))
		untilHTML = "Your plan runs to " + esc(moment(planEnds)) + " either way."
	}

	text := fmt.Sprintf(`%s

Your payment of %s for the %s plan did not go through.

Nothing has changed about your account. Every device and bridge you have
registered is still working and still reporting, and an SOS is never affected
by billing.%s

%s

  %s

The MeshSat team`, greeting(name), amount, plan, untilText, whatNext, hubURL)

	html := para(esc(greeting(name))) +
		para("Your payment for the "+esc(plan)+" plan did not go through.") +
		factTable(facts) +
		para("<strong>Nothing has changed about your account.</strong> Every device and bridge you "+
			"have registered is still working and still reporting, and an SOS is never affected "+
			"by billing.")
	if untilHTML != "" {
		html += para(untilHTML)
	}
	html += para(whatNextHTML) +
		button("Update your card", hubURL) +
		lastPara("The MeshSat team")

	return Message{
		Subject: "Your MeshSat Hub payment did not go through",
		Text:    text,
		HTML:    Wrap(html),
	}
}

// LapseWarning goes out before a paid plan ends, while the customer can still
// do something about it. Nothing sent one before: a lapsed customer found out
// when a device registration was refused.
func LapseWarning(name, plan string, expires time.Time, upgradeURL string) Message {
	when := moment(expires)
	text := fmt.Sprintf(`%s

Your %s plan ends on %s.

Nothing is deleted and nothing stops reporting. Everything you have registered
keeps working, and an SOS is never affected by billing. What changes is that you
go back to the free plan's ceiling of four devices and bridges, so you will not
be able to register another one until you move back up.

To keep the plan, renew here:

  %s

The MeshSat team`, greeting(name), plan, when, upgradeURL)

	html := para(esc(greeting(name))) +
		para("Your "+esc(plan)+" plan is ending.") +
		factTable([][2]string{
			{"Plan", esc(plan)},
			{"Ends", esc(when)},
		}) +
		para("Nothing is deleted and nothing stops reporting. Everything you have registered keeps "+
			"working, and an SOS is never affected by billing. What changes is that you go back to "+
			"the free plan's ceiling of four devices and bridges, so you will not be able to "+
			"register another one until you move back up.") +
		button(upgradeURL, "Renew your plan") +
		urlUnder(upgradeURL) +
		lastPara("The MeshSat team")

	return Message{
		Subject: fmt.Sprintf("Your MeshSat Hub %s plan ends %s", plan, when),
		Text:    text,
		HTML:    Wrap(html),
	}
}

// Lapsed is the notice on the day it actually drops.
func Lapsed(name, was string, ended time.Time, upgradeURL string) Message {
	when := moment(ended)

	text := fmt.Sprintf(`%s

Your %s plan ended on %s.

Your account is back on the free plan. Nothing was deleted: every device and
bridge you have registered is still registered and still reporting, and an SOS
is never affected by billing. The free plan covers four devices and bridges
together, so while you are over that you cannot register another one -- the
ones you have are unaffected.

To start again:

  %s

The MeshSat team`, greeting(name), was, when, upgradeURL)

	html := para(esc(greeting(name))) +
		para("Your "+esc(was)+" plan has ended and your account is back on the free plan.") +
		factTable([][2]string{
			{"Plan that ended", esc(was)},
			{"Ended", esc(when)},
			{"Now on", "free"},
		}) +
		para("Nothing was deleted: every device and bridge you have registered is still registered "+
			"and still reporting, and an SOS is never affected by billing. The free plan covers "+
			"four devices and bridges together, so while you are over that you cannot register "+
			"another one &mdash; the ones you have are unaffected.") +
		button(upgradeURL, "Start again") +
		urlUnder(upgradeURL) +
		lastPara("The MeshSat team")

	return Message{
		Subject: "Your MeshSat Hub plan has ended",
		Text:    text,
		HTML:    Wrap(html),
	}
}

// Refunded tells a customer their money went back and carries the credit note.
//
// It exists because nothing else would say so. The payment processor's refund
// emails are off by deliberate setting, and the billing system's own credit
// mail goes out under the wrong company's sender, so before this a refunded
// customer got their money and no document from anywhere (MESHSAT-1019).
//
// planEnds is the zero time when the refund did not move the plan, which is the
// case for a partial refund: the customer kept part of what they bought.
func Refunded(name, amount, creditNumber, invoiceNumber string, planEnds time.Time, hubURL string) Message {
	doc := "Your credit note"
	if creditNumber != "" {
		doc = "Your credit note " + creditNumber
	}
	ref := ""
	if invoiceNumber != "" {
		ref = fmt.Sprintf(" It cancels %s on invoice %s and reverses the VAT that was included in the price.",
			amount, invoiceNumber)
	}

	text := fmt.Sprintf(`%s

We have refunded %s to you.%s

%s is attached to this email as a PDF. Keep it with the original invoice: the
two together are the complete record.

The money goes back the way it came, through the payment provider you paid
with. Banks usually take a few working days to show it.
%s

Your usage is on the Settings page:

  %s

If something looks wrong, reply to this email and a person will read it.

The MeshSat team`, greeting(name), amount, ref, doc, planSentence(planEnds), hubURL)

	rows := [][2]string{{"Refunded", esc(amount)}}
	if creditNumber != "" {
		rows = append(rows, [2]string{"Credit note", esc(creditNumber)})
	}
	if invoiceNumber != "" {
		rows = append(rows, [2]string{"Cancels invoice", esc(invoiceNumber)})
	}
	if !planEnds.IsZero() {
		rows = append(rows, [2]string{"Plan now runs to", esc(moment(planEnds))})
	}

	lead := "We have refunded " + strong(esc(amount)) + " to you."
	if invoiceNumber != "" {
		lead += " It cancels " + esc(amount) + " on invoice " + esc(invoiceNumber) +
			" and reverses the VAT that was included in the price."
	}

	html := para(esc(greeting(name))) +
		para(lead) +
		factTable(rows) +
		para(esc(doc)+" is attached to this email as a PDF. Keep it with the original invoice: "+
			"the two together are the complete record.") +
		para("The money goes back the way it came, through the payment provider you paid with. "+
			"Banks usually take a few working days to show it.") +
		para(planSentenceHTML(planEnds)) +
		para("Your usage is on the Settings page: "+link(hubURL)) +
		note("If something looks wrong, reply to this email and a person will read it.") +
		lastPara("The MeshSat team")

	return Message{
		Subject: "Your MeshSat Hub refund",
		Text:    text,
		HTML:    Wrap(html),
	}
}

// RefundedNoDocument is the notice for a refund of a payment that never had an
// invoice -- one given back while its receipt was still parked or still queued.
// There is nothing to credit, so there is no credit note, and saying so plainly
// is better than sending nothing at all.
func RefundedNoDocument(name, amount string, planEnds time.Time, hubURL string) Message {
	text := fmt.Sprintf(`%s

We have refunded %s to you.

No invoice had been issued for that payment yet, so there is no credit note to
send: the payment and the refund cancel out and nothing is owed either way.
%s

Your usage is on the Settings page:

  %s

If something looks wrong, reply to this email and a person will read it.

The MeshSat team`, greeting(name), amount, planSentence(planEnds), hubURL)

	rows := [][2]string{{"Refunded", esc(amount)}}
	if !planEnds.IsZero() {
		rows = append(rows, [2]string{"Plan now runs to", esc(moment(planEnds))})
	}

	html := para(esc(greeting(name))) +
		para("We have refunded "+strong(esc(amount))+" to you.") +
		factTable(rows) +
		para("No invoice had been issued for that payment yet, so there is no credit note to send: "+
			"the payment and the refund cancel out and nothing is owed either way.") +
		para(planSentenceHTML(planEnds)) +
		para("Your usage is on the Settings page: "+link(hubURL)) +
		note("If something looks wrong, reply to this email and a person will read it.") +
		lastPara("The MeshSat team")

	return Message{
		Subject: "Your MeshSat Hub refund",
		Text:    text,
		HTML:    Wrap(html),
	}
}

// planSentence says what the refund did to the plan. A partial refund leaves it
// alone -- the customer kept part of what they bought -- and saying so is not
// filler: "your plan is unchanged" is the sentence that stops somebody
// wondering whether their devices are about to stop reporting.
func planSentence(planEnds time.Time) string {
	if planEnds.IsZero() {
		return `
Your plan is unchanged.`
	}
	return fmt.Sprintf(`
This refund takes back the time that payment bought, so your plan now runs to
%s. Nothing is deleted and nothing stops reporting: every device and bridge you
have registered keeps working, and an SOS is never affected by billing.`, moment(planEnds))
}

func planSentenceHTML(planEnds time.Time) string {
	if planEnds.IsZero() {
		return "Your plan is unchanged."
	}
	// No emphasis on the moment here: the fact table above already carries it,
	// and bolding the same string twice on one screen is noise, not emphasis.
	return "This refund takes back the time that payment bought, so your plan now runs to " +
		esc(moment(planEnds)) + ". Nothing is deleted and nothing stops reporting: every " +
		"device and bridge you have registered keeps working, and an SOS is never affected by billing."
}

// nlTime is the zone every customer-facing moment is stated in: the service is
// operated from the Netherlands and billed there, so one stated zone is better
// than a bare date that means a different day either side of us.
var nlTime = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		return time.UTC
	}
	return loc
}()

// moment renders an instant a reader in any timezone can act on without doing
// arithmetic: "Thursday 10-Sep-2026 23:59:59 CEST". These messages used to say
// "tomorrow" and print a bare date beside it, and the two disagreed -- the word
// came from dividing elapsed hours by 24, so a plan ending the day after
// tomorrow rendered as "tomorrow". Naming the moment removes both problems.
func moment(t time.Time) string {
	return t.In(nlTime).Format("Monday 02-Jan-2006 15:04:05 MST")
}

func greeting(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, "@") {
		return "Hello,"
	}
	if i := strings.IndexByte(name, ' '); i > 0 {
		name = name[:i]
	}
	return "Hello " + name + ","
}

// DonationReceipt is the giver's copy of a donation document.
//
// It comes from the Hub rather than from the billing system, which is the same
// split the credit note makes, for a different reason. Invoice Ninja has ONE
// payment template per company and it is written for a subscription: it says
// the document "shows the VAT included in the price", that "your MeshSat Hub
// subscription is active for the period this payment covers", and it points at
// the Settings page. All three are false for a gift -- the document carries no
// tax at all, a donation buys no tier, and an anonymous giver has no account to
// have settings in. The first real donation went out under that template.
//
// There is deliberately no link to the Hub in here. Anyone can donate without
// an account, so for most readers a sign-in destination is noise.
func DonationReceipt(name, amount, invoiceNumber string) Message {
	doc := "Your receipt"
	if invoiceNumber != "" {
		doc = "Your receipt " + invoiceNumber
	}

	text := fmt.Sprintf(`%s

Thank you. We have received your donation of %s.

%s is attached to this email as a PDF.

There is no VAT on it. A donation buys nothing, and a voluntary contribution
with no counter-performance is outside the scope of BTW -- so the document
shows no tax because none is owed, not because it was left off.

It grants no plan and needs no account either. Nothing about your MeshSat
changes because of it.

If something looks wrong, reply to this email and a person will read it.

The MeshSat team`, greeting(name), amount, doc)

	rows := [][2]string{{"Donation", esc(amount)}}
	if invoiceNumber != "" {
		rows = append(rows, [2]string{"Receipt", esc(invoiceNumber)})
	}

	html := para(esc(greeting(name))) +
		para("Thank you. We have received your donation of "+strong(esc(amount))+".") +
		factTable(rows) +
		para(esc(doc)+" is attached to this email as a PDF.") +
		para("There is no VAT on it. A donation buys nothing, and a voluntary contribution "+
			"with no counter-performance is outside the scope of BTW, so the document shows "+
			"no tax because none is owed, not because it was left off.") +
		para("It grants no plan and needs no account either. Nothing about your MeshSat "+
			"changes because of it.") +
		note("If something looks wrong, reply to this email and a person will read it.") +
		lastPara("The MeshSat team")

	subject := "Your MeshSat donation receipt"
	if invoiceNumber != "" {
		subject += " " + invoiceNumber
	}

	return Message{
		Subject: subject,
		Text:    text,
		HTML:    Wrap(html),
	}
}

// SignupRequest is one account request, carrying what the person deciding
// actually needs to see.
//
// Declared here rather than taken from internal/authentik so that this package
// keeps importing nothing but the standard library. A mail template that drags
// in an identity-provider client is a template that cannot be tested without
// one.
type SignupRequest struct {
	Name         string
	Email        string
	Organisation string
	Country      string
	Callsign     string
	Hardware     string
	IntendedUse  string
	// When the request arrived. Rendered as an exact moment, never as "2 days
	// ago": see moment's comment for what relative words cost here before.
	When time.Time
}

// PendingSignups tells the platform's administrators that somebody is waiting
// for a decision on an account.
//
// # Why this message exists
//
// Nothing told the operator. Approval has been in the Hub since MESHSAT-978 and
// the applicant gets an email the moment it happens -- but the step BEFORE that,
// a stranger finishing enrolment and landing in meshsat-pending, reached a
// YouTrack issue and a Matrix message and no inbox. So a request sat in the Beta
// requests panel until somebody happened to open Settings, and the person who
// had just been asked to verify their address waited without knowing for what.
//
// This is a notice, not a receipt: it is addressed to the operator and it names
// the outstanding work, so a message lost in transit is corrected by the next one
// rather than leaving a request invisible forever.
//
// unverified is the count of enrolments that have NOT confirmed their address.
// They are named but not listed, because they cannot be approved yet -- the
// identity provider refuses it, and so does ak.Approve -- so the operator has
// nothing to act on for them. They are worth a line because an enrolment that
// never verifies is a signal about the signup flow rather than about one person.
func PendingSignups(reqs []SignupRequest, unverified int, hubURL string) Message {
	settings := strings.TrimRight(hubURL, "/") + "/#/settings"

	var subject string
	switch n := len(reqs); n {
	case 0:
		// Only reached with unverified requests and no actionable ones. The
		// watcher does not send in that case, but a message is never built
		// half-formed: a subject line is not optional.
		subject = "MeshSat Hub: an account request has not been verified"
	case 1:
		subject = "MeshSat Hub: 1 account request is waiting for you"
	default:
		subject = fmt.Sprintf("MeshSat Hub: %d account requests are waiting for you", n)
	}

	var text strings.Builder
	var body strings.Builder

	lead := "Somebody has asked for a MeshSat Hub account and is waiting for a decision."
	if len(reqs) > 1 {
		lead = fmt.Sprintf("%d people have asked for MeshSat Hub accounts and are waiting for a decision.", len(reqs))
	}
	text.WriteString("Hello,\n\n" + lead + "\n")
	body.WriteString(para("Hello,") + para(esc(lead)))

	for _, r := range reqs {
		rows := [][2]string{{"Name", esc(fallback(r.Name, "(not given)"))}}
		text.WriteString("\n  " + fallback(r.Name, "(no name given)") + "\n")
		add := func(label, value string) {
			if strings.TrimSpace(value) == "" {
				return
			}
			rows = append(rows, [2]string{label, esc(value)})
			text.WriteString("    " + label + ": " + value + "\n")
		}
		add("Email", r.Email)
		add("Organisation", r.Organisation)
		add("Country", r.Country)
		add("Callsign", r.Callsign)
		add("Hardware", r.Hardware)
		add("Intended use", r.IntendedUse)
		if !r.When.IsZero() {
			add("Requested", moment(r.When))
		}
		body.WriteString(factTable(rows))
	}

	if unverified > 0 {
		s := fmt.Sprintf("%d further enrolment has not confirmed its address yet, so it cannot be "+
			"approved until it does.", unverified)
		if unverified > 1 {
			s = fmt.Sprintf("%d further enrolments have not confirmed their addresses yet, so they "+
				"cannot be approved until they do.", unverified)
		}
		text.WriteString("\n" + s + "\n")
		body.WriteString(note(esc(s)))
	}

	text.WriteString(`
Approve or reject them on the Settings page, under Beta requests:

  ` + settings + `

Approving activates the account, grants a role and emails them, so you do not
have to tell them yourself; their workspace is created the first time they sign
in. Rejecting is silent -- nothing is sent, so say so yourself if they deserve
an answer.

The MeshSat team`)

	body.WriteString(button(settings, "Review the requests") +
		urlUnder(settings) +
		para("Approving activates the account, grants a role and emails them, so you do not have "+
			"to tell them yourself; their workspace is created the first time they sign in.") +
		// Said plainly because it is a trap: an operator who believes both
		// decisions notify will leave a rejected applicant waiting for an answer
		// that is never coming. Reject sends nothing -- filed as MESHSAT-1082
		// rather than papered over here.
		note("Rejecting is silent &mdash; nothing is sent, so say so yourself if they deserve an answer.") +
		lastPara("The MeshSat team"))

	return Message{
		Subject: subject,
		Text:    text.String(),
		HTML:    Wrap(body.String()),
	}
}

// fallback keeps an empty field from rendering as a blank row, which reads as a
// bug in the message rather than as a field the person left empty.
func fallback(s, or string) string {
	if strings.TrimSpace(s) == "" {
		return or
	}
	return s
}

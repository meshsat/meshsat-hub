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

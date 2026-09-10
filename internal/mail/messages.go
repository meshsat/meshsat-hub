package mail

import (
	"fmt"
	"strings"
	"time"
)

// The Hub's transactional messages. Plain text, short, and written for the
// person receiving them rather than for the system sending them. Terms promise
// notice before a change takes effect, so these are part of keeping that.

// Approved tells someone their account request was accepted. Until this existed
// only the CLI approval path sent anything, and it sent from the shared identity
// provider's default address, which belongs to a different product.
func Approved(name, hubURL string) (subject, body string) {
	return "Your MeshSat Hub account is ready", fmt.Sprintf(`%s

Your MeshSat Hub account has been approved. You can sign in here:

  %s

Your workspace is created the first time you sign in. You start on the free
plan, which covers four devices and bridges together; you can see your usage
and move up a plan on the Settings page.

If you did not ask for an account, reply to this message and we will remove it.

The MeshSat team`, greeting(name), hubURL)
}

// PlanChanged confirms a payment was applied. Sent on a Ko-fi grant, so the
// customer has something from us and not only from the payment processor.
func PlanChanged(name, plan string, devices int, expires time.Time, hubURL string) (subject, body string) {
	limit := fmt.Sprintf("%d devices and bridges together", devices)
	if devices < 0 {
		limit = "an unlimited number of devices and bridges"
	}
	return fmt.Sprintf("Your MeshSat Hub plan is now %s", plan), fmt.Sprintf(`%s

Thank you. Your payment has been applied and your plan is now %s, which covers
%s.

It runs to %s. Ko-fi tells us about payments, not cancellations, so your plan
simply runs to the date it is paid to. Renewals stack, so paying early is never
punished.

Your usage is on the Settings page:

  %s

Your receipt, with the VAT included in the price, is sent separately.

The MeshSat team`, greeting(name), plan, limit, expires.Format("2 January 2006"), hubURL)
}

// LapseWarning goes out before a paid plan ends, while the customer can still
// do something about it. Nothing sent one before: a lapsed customer found out
// when a device registration was refused.
func LapseWarning(name, plan string, expires time.Time, upgradeURL, claimCode string) (subject, body string) {
	days := int(time.Until(expires).Hours() / 24)
	when := fmt.Sprintf("in %d days", days)
	switch {
	case days <= 0:
		when = "today"
	case days == 1:
		when = "tomorrow"
	}
	code := ""
	if claimCode != "" {
		code = fmt.Sprintf(`

Put your claim code %s in the Ko-fi message so the payment reaches your
account.`, claimCode)
	}
	return fmt.Sprintf("Your MeshSat Hub %s plan ends %s", plan, when), fmt.Sprintf(`%s

Your %s plan ends %s, on %s.

Nothing is deleted and nothing stops reporting. Everything you have registered
keeps working, and an SOS is never affected by billing. What changes is that you
go back to the free plan's ceiling of four devices and bridges, so you will not
be able to register another one until you move back up.

To keep the plan, renew here:

  %s%s

The MeshSat team`, greeting(name), plan, when, expires.Format("2 January 2006"), upgradeURL, code)
}

// Lapsed is the notice on the day it actually drops.
func Lapsed(name, was, upgradeURL string) (subject, body string) {
	return "Your MeshSat Hub plan has ended", fmt.Sprintf(`%s

Your %s plan has ended and your account is back on the free plan.

Nothing was deleted. Every device and bridge you have registered is still
registered and still reporting, and an SOS is never affected by billing. The
free plan covers four devices and bridges together, so while you are over that
you cannot register another one -- the ones you have are unaffected.

To start again:

  %s

The MeshSat team`, greeting(name), was, upgradeURL)
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

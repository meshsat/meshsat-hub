package api

import (
	"html/template"
	"net/http"
	"strings"
)

// The public site a donation link is followed from. A giver with no account has
// nowhere useful to go inside the Hub, so the way back is the way they came.
const siteURL = "https://meshsat.net"

// donatePage is where Stripe returns somebody who donated without an account.
//
// It is a plain server-rendered document rather than a route in the SPA because
// the person it is for has no account: every SPA route carries requiresAuth, so
// returning them there bounced a giver who had just paid to a sign-in wall
// (/#/login?redirect=/?donation=thanks). They are not a user and must not be
// handed an application.
//
// No JavaScript, no webfont and no build step: the only thing it fetches is the
// brand mark, which is already public. Both themes are covered by
// prefers-color-scheme; there is no toggle because there is no session to
// remember one in.
type donatePageData struct {
	Title   string
	Heading string
	Lead    string
	Note    string
	Site    string
}

// donatePageTmpl renders the page. Every field is a compile-time constant
// below, so nothing here is attacker-controlled -- html/template is used anyway
// so that stays true if somebody later adds a field that is not.
var donatePageTmpl = template.Must(template.New("donate").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex">
<title>{{.Title}}</title>
<style>
:root{
  --bg:#F7F5F2; --fg:#1A1714; --muted:#6B6055;
  --rule:#E4DED6; --accent:#C4470A;
}
@media (prefers-color-scheme:dark){
  :root{
    --bg:#14120F; --fg:#F2EDE6; --muted:#A89985;
    --rule:#2E2A25; --accent:#F25C05;
  }
}
*{box-sizing:border-box}
html{-webkit-text-size-adjust:100%}
body{
  margin:0; min-height:100dvh;
  display:flex; align-items:center; justify-content:center;
  padding:2rem 1.5rem;
  background:var(--bg); color:var(--fg);
  font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;
  font-size:17px; line-height:1.6;
}
main{width:100%; max-width:34rem}
.mark{display:block; margin:0 auto 2.25rem; width:40px; height:auto}
/* Only one mark is ever shown; the other is for the opposite theme. */
.on-dark{display:none}
@media (prefers-color-scheme:dark){
  .on-light{display:none}
  .on-dark{display:block}
}
h1{
  margin:0 0 .75rem;
  font-size:2rem; line-height:1.15; font-weight:600;
  letter-spacing:-.02em; text-wrap:balance;
}
.lead{margin:0; font-size:1.0625rem}
hr{
  border:0; border-top:1px solid var(--rule);
  margin:1.75rem 0 1.5rem;
}
.note{margin:0; color:var(--muted); font-size:.9375rem}
.back{margin:2rem 0 0; font-size:.9375rem}
a{color:var(--accent); text-underline-offset:.2em}
a:hover{text-decoration:none}
a:focus-visible{outline:2px solid var(--accent); outline-offset:3px; border-radius:2px}
</style>
</head>
<body>
<main>
  <img class="mark on-light" src="/meshsat-mark-light.png" width="40" height="40" alt="MeshSat">
  <img class="mark on-dark" src="/meshsat-mark-dark.png" width="40" height="40" alt="MeshSat">
  <h1>{{.Heading}}</h1>
  <p class="lead">{{.Lead}}</p>
  <hr>
  <p class="note">{{.Note}}</p>
  <p class="back"><a href="{{.Site}}">Back to meshsat.net</a></p>
</main>
</body>
</html>
`))

func (h *BillingHandler) renderDonatePage(w http.ResponseWriter, status int, d donatePageData) {
	d.Site = siteURL
	var b strings.Builder
	if err := donatePageTmpl.Execute(&b, d); err != nil {
		// The template is a constant and its data are constants, so this cannot
		// fail in practice. Say something true rather than serve a blank page.
		http.Error(w, "Your donation went through. A receipt is on its way by email.",
			http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Transactional and per-person: never cached by a proxy or a back button.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(b.String()))
}

// DonateThanks is where Stripe sends a giver who completed an anonymous
// donation.
//
// It promises the receipt rather than showing it. The document is produced
// asynchronously by the receipt outbox on the lease holder and does not exist
// yet when this page renders, and there is no session here to show it to.
//
// @Summary      Donation complete
// @Description  The page Stripe returns an anonymous giver to after a successful donation.
// @Tags         billing
// @Produce      html
// @Success      200  {string}  string  "thank-you page"
// @Router       /donate/thanks [get]
func (h *BillingHandler) DonateThanks(w http.ResponseWriter, r *http.Request) {
	h.renderDonatePage(w, http.StatusOK, donatePageData{
		Title:   "Donation received · MeshSat Hub",
		Heading: "Thank you.",
		Lead: "Your donation went through. A receipt is on its way to the email address " +
			"you gave Stripe, and it should arrive within a few minutes.",
		Note: "A donation is support, not a purchase. It carries no VAT, it unlocks nothing, " +
			"and it needs no account. Nothing else happens, which is the point of it.",
	})
}

// DonateCancelled is where Stripe sends somebody who closed the payment page.
//
// Nothing went wrong here: a person changed their mind. The page leads with the
// only thing they are likely to be worried about.
//
// @Summary      Donation cancelled
// @Description  The page Stripe returns an anonymous giver to after they abandon a donation.
// @Tags         billing
// @Produce      html
// @Success      200  {string}  string  "cancelled page"
// @Router       /donate/cancelled [get]
func (h *BillingHandler) DonateCancelled(w http.ResponseWriter, r *http.Request) {
	h.renderDonatePage(w, http.StatusOK, donatePageData{
		Title:   "Nothing was charged · MeshSat Hub",
		Heading: "Nothing was charged.",
		Lead: "You closed the payment page before it finished, so no money moved and " +
			"there is nothing to undo.",
		Note: "If you meant to donate, the button on meshsat.net will start it again. " +
			"MeshSat works the same either way.",
	})
}

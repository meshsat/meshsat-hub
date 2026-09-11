package api

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"

	"github.com/meshsat/meshsat-hub/internal/stripe"
)

// The donation page.
//
// It used to be a 303 straight to Stripe. That worked and looked like nothing:
// a white hosted page with a small logo, a number, and a blue button. Stripe's
// Checkout is inside their PCI scope and they constrain it hard on purpose --
// you get a logo, two colours and a font, never a layout -- so there was no
// amount of configuration that would have made that page say what the money is
// for.
//
// Embedded Checkout moves the line: Stripe renders only the payment form, we
// render everything around it. The white space becomes the part that explains
// what somebody is giving to, which is the only part a donor actually needs.
//
// WHAT THIS COSTS, because it is not free and the earlier claim that it was
// should not stand: the page loads Stripe.js from js.stripe.com and puts the
// publishable key in the browser. That means a Content-Security-Policy on this
// route that admits a third-party origin, on a codebase whose first rule is
// security. So the loosened policy is set HERE, on this one handler, and the
// rest of the Hub keeps `script-src 'self'`. The inline mount script runs from
// a per-request nonce rather than 'unsafe-inline', so an injected <script> on
// this page still cannot execute.

// SetPublishableKey supplies the browser-side Stripe key. Without it the page
// degrades to the hosted redirect rather than rendering a form that cannot
// mount -- a donor should never meet a broken payment box.
func (h *BillingHandler) SetPublishableKey(k string) { h.publishable = k }

// donateEmbedCSP is this route's policy, and only this route's.
//
// Every origin is here because Stripe needs it, not because it was convenient:
// js.stripe.com serves the library, the frames are the card and wallet fields
// that must stay cross-origin for Stripe to keep the card number out of our
// DOM, and api/checkout.stripe.com are what the form talks to. Nothing else is
// admitted, `default-src` stays 'self', and frame-ancestors stays 'none' so
// this page cannot itself be framed.
const donateEmbedCSP = "default-src 'self'; " +
	"script-src 'self' https://js.stripe.com 'nonce-%s'; " +
	"frame-src https://js.stripe.com https://checkout.stripe.com https://hooks.stripe.com; " +
	"connect-src 'self' https://api.stripe.com https://checkout.stripe.com https://merchant-ui-api.stripe.com; " +
	"img-src 'self' data: https://*.stripe.com; " +
	"style-src 'self' 'unsafe-inline'; " +
	"font-src 'self'; " +
	"base-uri 'self'; form-action 'self'; frame-ancestors 'none'; object-src 'none'"

// cspWithNonce fills the one placeholder in this route's policy.
func cspWithNonce(nonce string) string {
	return fmt.Sprintf(donateEmbedCSP, nonce)
}

type donateEmbedData struct {
	Nonce        string
	Publishable  string
	ClientSecret string
	Site         string
}

var donateEmbedTmpl = template.Must(template.New("donate-embed").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex">
<title>Support MeshSat</title>
<style>
:root{
  --bg:#F7F5F2; --fg:#1A1714; --muted:#6B6055;
  --rule:#E4DED6; --accent:#C4470A; --panel:#FFFFFF;
}
@media (prefers-color-scheme:dark){
  :root{
    --bg:#14120F; --fg:#F2EDE6; --muted:#A89985;
    --rule:#2E2A25; --accent:#F25C05; --panel:#1C1915;
  }
}
*{box-sizing:border-box}
html{-webkit-text-size-adjust:100%}
body{
  margin:0; min-height:100dvh; padding:3rem 1.5rem;
  background:var(--bg); color:var(--fg);
  font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;
  font-size:17px; line-height:1.6;
}
.wrap{
  width:100%; max-width:64rem; margin:0 auto;
  display:grid; gap:3rem 4rem; align-items:start;
  grid-template-columns:1fr;
}
@media (min-width:56rem){ .wrap{grid-template-columns:1fr 26rem} }
.mark{display:block; width:40px; height:auto; margin:0 0 2rem}
.on-dark{display:none}
@media (prefers-color-scheme:dark){ .on-light{display:none} .on-dark{display:block} }
h1{margin:0 0 1rem; font-size:2.25rem; line-height:1.1; font-weight:600; letter-spacing:-.02em; text-wrap:balance}
.lead{margin:0 0 2rem; font-size:1.0625rem}
h2{
  margin:0 0 .5rem; font-size:.8125rem; font-weight:600;
  letter-spacing:.08em; text-transform:uppercase; color:var(--muted);
}
ul{margin:0 0 2rem; padding:0; list-style:none}
li{
  padding:.75rem 0; border-top:1px solid var(--rule);
  display:grid; grid-template-columns:7.5rem 1fr; gap:1rem;
}
li:last-child{border-bottom:1px solid var(--rule)}
li b{font-weight:600; font-variant-numeric:tabular-nums}
li span{color:var(--muted); font-size:.9375rem}
.note{margin:0 0 1rem; color:var(--muted); font-size:.9375rem}
.panel{
  background:var(--panel); border:1px solid var(--rule);
  border-radius:10px; padding:1.25rem; min-height:22rem;
}
a{color:var(--accent); text-underline-offset:.2em}
a:hover{text-decoration:none}
a:focus-visible{outline:2px solid var(--accent); outline-offset:3px; border-radius:2px}
.foot{margin:2.5rem 0 0; font-size:.9375rem; color:var(--muted)}
</style>
</head>
<body>
<div class="wrap">
  <main>
    <img class="mark on-light" src="/meshsat-mark-light.png" width="40" height="40" alt="MeshSat">
    <img class="mark on-dark" src="/meshsat-mark-dark.png" width="40" height="40" alt="MeshSat">
    <h1>Keep the network answering</h1>
    <p class="lead">
      MeshSat carries messages when the usual networks are gone &mdash; over Iridium, LoRa mesh
      and Reticulum, from a kit somebody can carry. It is open source and free to run.
      What it cannot make free is the airtime.
    </p>

    <h2>Where it goes</h2>
    <ul>
      <li><b>Airtime</b><span>Every satellite message costs money per byte, whoever sends it. This is the bill that never stops.</span></li>
      <li><b>Hardware</b><span>Field kits get built and broken on purpose, so the supported-device list only claims what has actually been run.</span></li>
      <li><b>Servers</b><span>The Hub, the broker and the relays that stay up so a kit has something to reach.</span></li>
    </ul>

    <p class="note">
      This is a gift, not a purchase. It unlocks nothing, upgrades nothing and starts no
      subscription &mdash; and because it buys nothing in return, it carries no VAT. You will
      get a receipt.
    </p>
    <p class="foot">
      MeshSat is supported by
      <a href="https://www.sidnfonds.nl/projecten/meshsat-keeping-people-connected-when-the-network-is-not" rel="noopener">SIDN fonds</a>.
      <br><a href="{{.Site}}">Back to meshsat.net</a>
    </p>
  </main>

  <aside>
    <div class="panel"><div id="checkout"></div></div>
  </aside>
</div>

<script src="https://js.stripe.com/v3/"></script>
<script nonce="{{.Nonce}}">
  (function () {
    var stripe = Stripe({{.Publishable}});
    stripe.initEmbeddedCheckout({ clientSecret: {{.ClientSecret}} })
      .then(function (checkout) { checkout.mount('#checkout'); })
      .catch(function () {
        // Never leave a dead box where a payment form should be.
        document.getElementById('checkout').innerHTML =
          '<p>The payment form could not be loaded. Nothing was charged. ' +
          '<a href="/donate?hosted=1">Try the standard checkout</a>.</p>';
      });
  })();
</script>
</body>
</html>
`))

// nonceFor returns a fresh CSP nonce. A failure to read randomness is not
// survivable here: the alternative is 'unsafe-inline' on a payment page.
func nonceFor() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(b), nil
}

// DonatePage renders the donation page with Stripe's form embedded in it.
//
// Falls back to the hosted redirect whenever the embedded path cannot be relied
// on -- no publishable key, ?hosted=1 from the error branch above, or Stripe
// declining to create the session. A donor meets a working payment page either
// way; the difference is only how much of it is ours.
func (h *BillingHandler) DonatePage(w http.ResponseWriter, r *http.Request) {
	if h.donatio == "" || h.client == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("Donations are not available at the moment. Nothing was charged.\n"))
		return
	}
	if h.publishable == "" || r.URL.Query().Get("hosted") == "1" {
		h.DonateRedirect(w, r)
		return
	}
	nonce, err := nonceFor()
	if err != nil {
		slog.Error("billing: no randomness for a CSP nonce; falling back to hosted checkout", "error", err)
		h.DonateRedirect(w, r)
		return
	}
	s, err := h.client.Donation(r.Context(), stripe.DonationRequest{
		PriceID: h.donatio,
		// Stripe substitutes the id, and /donate/thanks reads it back to show
		// the giver what they gave rather than a bare thank-you.
		ReturnURL: h.hubURL + "/donate/thanks?session_id={CHECKOUT_SESSION_ID}",
	})
	if err != nil || s.ClientSecret == "" {
		slog.Error("billing: could not start an embedded donation; using hosted checkout", "error", err)
		h.DonateRedirect(w, r)
		return
	}
	var b bytes.Buffer
	if err := donateEmbedTmpl.Execute(&b, donateEmbedData{
		Nonce:        nonce,
		Publishable:  h.publishable,
		ClientSecret: s.ClientSecret,
		Site:         siteURL,
	}); err != nil {
		slog.Error("billing: rendering the donation page failed; using hosted checkout", "error", err)
		h.DonateRedirect(w, r)
		return
	}
	// This route's policy replaces the global one, which forbids js.stripe.com.
	w.Header().Set("Content-Security-Policy", cspWithNonce(nonce))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b.Bytes())
	slog.Info("billing: embedded donation page served", "session", s.ID)
}

// donateEmbedTmplSource returns the rendered page for a fixed input, so a test
// can assert what the page actually tells a donor.
func donateEmbedTmplSource() string {
	var b strings.Builder
	_ = donateEmbedTmpl.Execute(&b, donateEmbedData{Nonce: "n", Publishable: "pk", ClientSecret: "cs", Site: siteURL})
	return b.String()
}

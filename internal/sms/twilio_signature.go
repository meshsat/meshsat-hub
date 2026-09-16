package sms

import (
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- Twilio's published scheme is HMAC-SHA1; see validateTwilioSignature
	"encoding/base64"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// twilioSignature computes the value Twilio puts in X-Twilio-Signature.
//
// Twilio's published scheme: take the full URL the request was sent to,
// including its query string, append every POST parameter as name immediately
// followed by value with the names sorted alphabetically, HMAC-SHA1 the result
// and base64 the digest.
//
// HMAC-SHA1 is Twilio's choice, not ours, and it is not the broken use of SHA-1:
// collision attacks on the hash do not yield forgeries against HMAC. gosec's
// G505 fires on the import regardless, hence the annotation above.
//
// The key is the account's AUTH TOKEN. It is deliberately NOT cfg.SMSAuthToken:
// when HUB_SMS_API_KEY_SID is set, that field holds the API Key SECRET and is
// passed to NewClientWithAPIKey for sending (cmd/meshsat-hub/main.go). Twilio
// signs inbound requests with the account credential whichever credential the
// application sends with, so inbound validation carries its own value.
func twilioSignature(authToken, fullURL string, post url.Values) string {
	mac := hmac.New(sha1.New, []byte(authToken))
	mac.Write([]byte(signedString(fullURL, post)))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// signedString builds the exact byte string Twilio's scheme signs. It is split
// out so a test can assert the construction itself, which is the part that is
// easy to get subtly wrong and invisible in a digest comparison.
func signedString(fullURL string, post url.Values) string {
	var b strings.Builder
	b.WriteString(fullURL)

	keys := make([]string, 0, len(post))
	for k := range post {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		// Inbound SMS parameters are single-valued. Sorting the values keeps a
		// repeated parameter deterministic rather than dependent on map order.
		vs := append([]string(nil), post[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			b.WriteString(k)
			b.WriteString(v)
		}
	}
	return b.String()
}

// validateTwilioSignature reports whether sig is a valid X-Twilio-Signature for
// this request under authToken.
//
// publicBase is the Hub's configured public origin (HUB_PUBLIC_URL). Twilio
// signed the URL it was configured with, which is the public one, so a
// reconstruction from request headers would be wrong behind the edge. Header
// reconstruction is only the fallback for a Hub with no public URL configured.
//
// A header-derived URL is not a forgery vector either way: getting it wrong can
// only cause a false REJECT, since producing a matching signature still needs
// the auth token.
func validateTwilioSignature(authToken string, r *http.Request, sig string) bool {
	if authToken == "" || sig == "" {
		return false
	}
	given, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return false
	}
	for _, u := range candidateURLs(r) {
		want, err := base64.StdEncoding.DecodeString(twilioSignature(authToken, u, r.PostForm))
		if err != nil {
			continue
		}
		if hmac.Equal(given, want) {
			return true
		}
	}
	return false
}

// candidateURLs returns the URLs this request may have been signed against.
//
// The configured public origin comes first. The header-derived form is tried as
// well so that a Hub reached directly -- a local run, or a second hostname --
// still validates rather than silently refusing every message.
func candidateURLs(r *http.Request) []string {
	uri := r.URL.RequestURI()
	out := make([]string, 0, 2)

	if base := strings.TrimSuffix(publicBaseURL, "/"); base != "" {
		out = append(out, base+uri)
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	if r.Host != "" {
		out = append(out, scheme+"://"+r.Host+uri)
	}
	return out
}

// publicBaseURL is the Hub's configured public origin, set once at startup by
// SetPublicBaseURL. It is a package variable rather than a handler field because
// the signature helpers are used by the OOB reply path as well.
var publicBaseURL string

// SetPublicBaseURL records the Hub's public origin for inbound signature
// validation. Call it before serving; it is not safe for concurrent writes.
func SetPublicBaseURL(u string) { publicBaseURL = u }

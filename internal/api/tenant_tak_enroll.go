package api

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	qrcode "github.com/skip2/go-qrcode"

	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/takenroll"
)

// Enrolment: turning "this person should see the map" into a file their TAK
// client can import (MESHSAT-1040).
//
// # The shape is bridge_provision.go's, on purpose
//
// Credentials are generated server-side, stashed under a single-use nonce, and
// claimed once at an UNAUTHENTICATED endpoint, because the thing doing the
// claiming is a phone that has no Hub account. The QR carries a short URI and
// never the secrets. Every failure answers with ONE refusal string, so an
// unauthenticated caller cannot tell "no such enrolment" from "wrong nonce" and
// cannot enumerate anything. Inventing a second shape for the same problem is how
// two half-reviewed security surfaces end up in one codebase.
//
// # What is different, and why
//
// bridge_provision stashes its bundle in plain text. This one stashes the
// phone's PRIVATE KEY, which has to survive the gap between asking the operator
// for a signature and the customer claiming the package -- it cannot live in one
// replica's memory, because either replica may serve the claim. So it is
// ENCRYPTED with a key derived from the nonce, and the nonce is never stored:
// only its SHA-256. A database dump, a backup or a stolen replica therefore holds
// an undecryptable blob rather than an enrollable identity. The same reasoning as
// the operator wrapping each tenant's CA key.
//
// # The certificate is not ours to issue
//
// The Hub asks the operator for it through a custom resource and the operator
// forces the common name to the username. So this code cannot mint an identity
// for somebody else's user even if it tried.

// TAKCertOutcome is how far an enrolment certificate has got.
type TAKCertOutcome int

const (
	// TAKCertReady means the certificate is signed and collected.
	TAKCertReady TAKCertOutcome = iota
	// TAKCertPending means the operator has not signed yet. Normal for a few
	// seconds: it reconciles on a ticker.
	TAKCertPending
	// TAKCertRefused is terminal and carries a reason.
	TAKCertRefused
)

// TAKCertMaterial is a signed enrolment certificate.
type TAKCertMaterial struct {
	CertPEM   string
	CACertPEM string
	Serial    string
	NotAfter  time.Time
}

// TAKCerts asks the operator to sign a phone's certificate. Implemented by an
// adapter over takhosted.CertKeeper in cmd/meshsat-hub, for the reason given at
// the top of tenant_tak.go: this package must not import the Kubernetes client,
// because internal/routing imports this package and routing is on an ingest path.
type TAKCerts interface {
	// NewKey generates the phone's key and CSR. The private half is returned to
	// the caller and never kept by the implementation.
	NewKey(username string) (csrPEM, keyPEM []byte, err error)
	Request(ctx context.Context, label, username string, csrPEM []byte) error
	Collect(ctx context.Context, label, username string) (TAKCertMaterial, TAKCertOutcome, string, error)
	Discard(ctx context.Context, label, username string) error
}

// SetTAKCerts attaches the certificate machinery. Enrolment is refused until it
// is set, which is what a Hub running without hosted TAK wants.
//
// frontTrustPEM is the chain of the certificate the TAK front presents, and it
// arrives HERE rather than through its own setter so the two cannot be set
// independently: a handler holding one without the other would mint enrolments
// whose truststore is empty, and the phone would fail the handshake with nothing
// on our side to show why.
func (h *TenantTAKHandler) SetTAKCerts(c TAKCerts, frontTrustPEM []byte) {
	h.certs = c
	h.frontTrust = frontTrustPEM
}

// enrolTTL is how long a claim is good for. Fifteen minutes: long enough to walk
// to the phone and scan the code, short enough that an unclaimed enrolment is not
// a standing credential. bridge_provision allows thirty; a person's certificate
// deserves less room than a bridge's first boot.
const enrolTTL = 15 * time.Minute

// enrolClaimRefused is the ONLY thing an unauthenticated caller is ever told.
// Distinguishing the cases would turn this endpoint into an oracle for which
// enrolments exist.
const enrolClaimRefused = "invalid or expired enrolment token"

// enrolStash is what survives between minting and claiming. It holds no plaintext
// secret: KeyCipher is AES-256-GCM under a key derived from the nonce.
type enrolStash struct {
	TenantID  string    `json:"tenant_id"`
	Username  string    `json:"username"`
	Label     string    `json:"label"`
	KeyCipher string    `json:"key_cipher"`
	CreatedAt time.Time `json:"created_at"`
}

func enrolStashKey(claimID string) string { return "tak_enroll:" + claimID }

// stashKeyFor derives the encryption key for one stash.
//
// A plain SHA-256 over the nonce, the claim id and a label rather than HKDF: the
// nonce is already 128 uniformly random bits from crypto/rand, so a full
// extract-and-expand buys nothing here, and the claim id in the input stops one
// nonce decrypting a different stash. The label keeps this key distinct from any
// other use of the same nonce.
func stashKeyFor(nonce, claimID string) []byte {
	sum := sha256.Sum256([]byte("meshsat tak enrolment key\x00" + claimID + "\x00" + nonce))
	return sum[:]
}

func sealKey(keyPEM []byte, nonce, claimID string) (string, error) {
	block, err := aes.NewCipher(stashKeyFor(nonce, claimID))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	// Same wire shape as the rest of this codebase: [nonce][ciphertext+tag].
	return base64.StdEncoding.EncodeToString(gcm.Seal(iv, iv, keyPEM, nil)), nil
}

func openKey(cipherText, nonce, claimID string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(cipherText)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(stashKeyFor(nonce, claimID))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, errors.New("api: sealed enrolment key is too short")
	}
	return gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("api: no randomness: %w", err)
	}
	return hex.EncodeToString(b), nil
}

type enrolmentResponse struct {
	// URL is what the QR encodes and what a phone fetches. ABSOLUTE, because the
	// thing that follows it is a handset that has no idea which host this came
	// from. It carries the claim id and the nonce, which together are the only
	// credential -- so it is a secret, and it is shown once.
	URL string `json:"url"`
	// ITAKURL is the SAME enrolment, fetching the iTAK package instead. iTAK reads
	// a flat archive with no manifest, so the ATAK package does not import into it
	// at all -- and a claim is single use, so a customer who tries the wrong one on
	// an iPhone burns the enrolment (MESHSAT-1084).
	//
	// Returned as a field rather than left for the UI to build, so the shape of a
	// claim URL is decided in exactly one place. Both point at the same claim: using
	// either one spends it.
	ITAKURL string `json:"itak_url"`
	// ExpiresAt is when the claim stops working.
	ExpiresAt time.Time `json:"expires_at"`
	// Username is the account the package will authenticate as.
	Username string `json:"username"`
}

// mintEnrolment does everything both enrolment endpoints need: check the tenant
// really can enrol this person, ask the operator for a certificate, and stash the
// sealed key under a fresh claim id and nonce.
//
// Extracted rather than duplicated because the JSON endpoint and the QR endpoint
// must mint IDENTICALLY. Two copies of this would drift, and the half that
// drifted would be the one nobody tests by hand -- a QR is scanned by a phone, so
// a wrong URL in it shows up as "the phone just does not connect".
//
// Returns (nil, status, message) on refusal, so the caller can answer in its own
// content type.
//
// NOTE it supersedes: each call asks the operator to replace any outstanding
// request for this username and overwrites the row's token hash, so the previous
// link stops working. One live enrolment per person, which is what you want --
// two valid links for one identity is two chances to leak it.
func (h *TenantTAKHandler) mintEnrolment(r *http.Request, username string) (*enrolmentResponse, int, string) {
	ctx := r.Context()
	tenantID := hubauth.TenantIDFromContext(ctx)

	// Both, together: without the front's chain the package would carry an empty
	// truststore and the phone would fail the handshake. Same message for both, so
	// the answer says nothing about how this Hub is configured.
	if h.certs == nil || len(h.frontTrust) == 0 {
		return nil, http.StatusServiceUnavailable, "hosted TAK is not available on this Hub"
	}
	if !takUsernamePattern.MatchString(username) {
		return nil, http.StatusBadRequest, "that is not a TAK username"
	}

	inst, err := h.store.GetTAKInstance(ctx, tenantID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, http.StatusNotFound, "this account has no TAK server yet"
	case err != nil:
		return nil, http.StatusInternalServerError, "could not read the TAK status"
	}
	if inst.Phase != "Ready" {
		// Enrolling against a server that is not up yields a package whose
		// connection fails with nothing to explain it.
		return nil, http.StatusConflict, "the TAK server is not ready yet"
	}

	user, err := h.store.GetTAKUser(ctx, tenantID, username)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, http.StatusNotFound, "no such TAK user"
	case err != nil:
		return nil, http.StatusInternalServerError, "could not read the TAK user"
	}
	if !user.Active {
		return nil, http.StatusConflict, "that TAK user is suspended"
	}

	csrPEM, keyPEM, err := h.certs.NewKey(username)
	if err != nil {
		h.log.Error("tak enrolment: generating a key failed", "tenant", tenantID, "error", err)
		return nil, http.StatusInternalServerError, "could not start the enrolment"
	}
	if err := h.certs.Request(ctx, inst.Label, username, csrPEM); err != nil {
		h.log.Error("tak enrolment: asking for a certificate failed",
			"tenant", tenantID, "username", username, "error", err)
		return nil, http.StatusServiceUnavailable, "could not ask for a certificate"
	}

	claimID, err := randomHex(16)
	if err != nil {
		return nil, http.StatusInternalServerError, "could not start the enrolment"
	}
	nonce, err := randomHex(16)
	if err != nil {
		return nil, http.StatusInternalServerError, "could not start the enrolment"
	}
	sealed, err := sealKey(keyPEM, nonce, claimID)
	if err != nil {
		return nil, http.StatusInternalServerError, "could not start the enrolment"
	}

	now := time.Now().UTC()
	blob, err := json.Marshal(enrolStash{
		TenantID: tenantID, Username: username, Label: inst.Label,
		KeyCipher: sealed, CreatedAt: now,
	})
	if err != nil {
		return nil, http.StatusInternalServerError, "could not start the enrolment"
	}
	if err := h.store.SetSystemConfig(ctx, enrolStashKey(claimID), string(blob)); err != nil {
		return nil, http.StatusInternalServerError, "could not start the enrolment"
	}

	// The hash, never the token: the token is shown once and storing it would put
	// a live credential in every backup. The expiry is on the row so the UI can
	// say "waiting to be claimed" without holding anything secret.
	sum := sha256.Sum256([]byte(nonce))
	expires := now.Add(enrolTTL)
	user.EnrollTokenHash = hex.EncodeToString(sum[:])
	user.EnrollExpiresAt = &expires
	if err := h.store.UpdateTAKUser(ctx, tenantID, user); err != nil {
		return nil, http.StatusInternalServerError, "could not start the enrolment"
	}

	claim := fmt.Sprintf("https://%s/api/tak/enroll/%s/%s", h.publicHost, claimID, nonce)
	return &enrolmentResponse{
		URL:       claim,
		ITAKURL:   claimURLFor(claim, "itak"),
		ExpiresAt: expires,
		Username:  username,
	}, 0, ""
}

// Enrol mints a one-time enrolment for one of the tenant's TAK users.
//
// @Summary      Mint a one-time TAK enrolment package link
// @Tags         tenant
// @Produce      json
// @Param        username  path  string  true  "TAK username"
// @Success      201  {object}  enrolmentResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/tenant/tak/users/{username}/enrollment [post]
func (h *TenantTAKHandler) Enrol(w http.ResponseWriter, r *http.Request) {
	username := strings.ToLower(chi.URLParam(r, "username"))
	out, status, msg := h.mintEnrolment(r, username)
	if out == nil {
		writeError(w, status, msg)
		return
	}
	h.logAudit(r, hubauth.TenantIDFromContext(r.Context()), "tak_enrolment_minted", "username="+username)
	// no-store: the body carries a live credential, and a proxy or a browser
	// cache holding it would outlive the fifteen minutes on purpose.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, out)
}

// EnrolQR is the same enrolment as a QR code, for pointing a phone at it.
//
// The `client` parameter decides WHICH package the scan will fetch, and it has to
// be decided here rather than offered as a toggle afterwards: this endpoint MINTS,
// so re-rendering the QR in the other flavour would invalidate the code already on
// screen. See claimFlavour for why two flavours exist at all (MESHSAT-1084).
//
// @Summary      Mint a one-time TAK enrolment as a QR code
// @Tags         tenant
// @Produce      image/png
// @Param        username  path   string  true   "TAK username"
// @Param        size      query  int     false  "QR size in pixels (default 512)"
// @Param        client    query  string  false  "atak (default) or itak"
// @Success      200  {file}  image/png
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/tenant/tak/users/{username}/enrollment/qr [post]
func (h *TenantTAKHandler) EnrolQR(w http.ResponseWriter, r *http.Request) {
	username := strings.ToLower(chi.URLParam(r, "username"))

	size := 512
	if v := r.URL.Query().Get("size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 128 && n <= 2048 {
			size = n
		}
	}

	out, status, msg := h.mintEnrolment(r, username)
	if out == nil {
		writeError(w, status, msg)
		return
	}
	png, err := qrcode.Encode(claimURLFor(out.URL, r.URL.Query().Get("client")), qrcode.Medium, size)
	if err != nil {
		h.log.Error("tak enrolment: rendering the QR failed", "username", username, "error", err)
		writeError(w, http.StatusInternalServerError, "could not render the QR code")
		return
	}
	h.logAudit(r, hubauth.TenantIDFromContext(r.Context()), "tak_enrolment_minted",
		"username="+username+" form=qr")
	w.Header().Set("Content-Type", "image/png")
	// The image IS the credential -- it encodes the claim URL -- so it must not be
	// cached any more than the JSON form is.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Enrolment-Expires", out.ExpiresAt.Format(time.RFC3339))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(png); err != nil {
		h.log.Warn("tak enrolment: the QR was not fully delivered", "username", username, "error", err)
	}
}

// claimURLFor points a claim URL at the right package for the phone holding it.
//
// ATAK and WinTAK read a zip with MANIFEST/manifest.xml beside the preferences;
// iTAK reads a FLAT zip with no manifest at all. They are different archive
// shapes, not variants of one, so the ATAK package simply does not import into
// iTAK -- and because a claim is single use, a customer who tries it on an iPhone
// burns the enrolment and has to mint another.
//
// Claim already served both and selected on this parameter. What was missing was
// any way to ask for it: the TAK page promised "ATAK, WinTAK or iTAK" and every
// link and QR it produced was the ATAK one (MESHSAT-1084).
//
// Anything that is not iTAK means ATAK, including an empty or misspelt value. The
// default has to be the one that works for most phones rather than an error,
// because this runs after the enrolment has been minted.
func claimURLFor(base, client string) string {
	if !strings.EqualFold(strings.TrimSpace(client), "itak") {
		return base
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "client=itak"
}

// Claim hands over the enrolment package. UNAUTHENTICATED: the claim id and nonce
// are the credential, because the caller is a phone with no Hub account.
//
// @Summary      Claim a TAK enrolment package
// @Tags         tak
// @Produce      application/zip
// @Param        claimID  path   string  true   "claim id"
// @Param        nonce    path   string  true   "one-time nonce"
// @Param        client   query  string  false  "atak (default) or itak"
// @Success      200  {file}    binary  "the data package"
// @Success      202  {object}  map[string]string  "not signed yet, retry"
// @Failure      404  {object}  map[string]string
// @Failure      410  {object}  map[string]string
// @Router       /api/tak/enroll/{claimID}/{nonce} [get]
func (h *TenantTAKHandler) Claim(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claimID := chi.URLParam(r, "claimID")
	nonce := chi.URLParam(r, "nonce")

	// Shape first, so a malformed path never reaches the store.
	if len(claimID) != 32 || len(nonce) != 32 || !isHex(claimID) || !isHex(nonce) {
		writeError(w, http.StatusNotFound, enrolClaimRefused)
		return
	}
	if h.certs == nil {
		writeError(w, http.StatusNotFound, enrolClaimRefused)
		return
	}

	raw, err := h.store.GetSystemConfig(ctx, enrolStashKey(claimID))
	if err != nil || raw == "" {
		// Includes the claimed case: a claim blanks the value rather than
		// deleting a row, so a second attempt lands here.
		writeError(w, http.StatusNotFound, enrolClaimRefused)
		return
	}
	var stash enrolStash
	if err := json.Unmarshal([]byte(raw), &stash); err != nil {
		h.log.Error("tak enrolment: stash does not parse", "claim", claimID)
		writeError(w, http.StatusNotFound, enrolClaimRefused)
		return
	}

	if time.Since(stash.CreatedAt) > enrolTTL {
		h.clearEnrolment(ctx, claimID, stash)
		writeError(w, http.StatusGone, "this enrolment has expired")
		return
	}

	user, err := h.store.GetTAKUser(ctx, stash.TenantID, stash.Username)
	if err != nil {
		writeError(w, http.StatusNotFound, enrolClaimRefused)
		return
	}
	// Constant time, and against the hash on the row rather than anything in the
	// stash: the row is what an operator can revoke by clearing it.
	sum := sha256.Sum256([]byte(nonce))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(user.EnrollTokenHash)) != 1 {
		writeError(w, http.StatusNotFound, enrolClaimRefused)
		return
	}
	if !user.Active {
		writeError(w, http.StatusNotFound, enrolClaimRefused)
		return
	}

	material, outcome, message, err := h.certs.Collect(ctx, stash.Label, stash.Username)
	switch {
	case err != nil:
		// Only infrastructure failures arrive with an error; pending and refused
		// are expected answers carried in the outcome. Keeping them apart is what
		// lets this say 503 for a sick cluster and 202 for "wait a moment".
		h.log.Error("tak enrolment: collecting a certificate failed",
			"tenant", stash.TenantID, "username", stash.Username, "error", err)
		writeError(w, http.StatusServiceUnavailable, "could not collect the certificate")
		return
	case outcome == TAKCertRefused:
		h.log.Warn("tak enrolment: the operator refused a certificate",
			"tenant", stash.TenantID, "username", stash.Username, "message", message)
		h.clearEnrolment(ctx, claimID, stash)
		writeError(w, http.StatusConflict, "the certificate was refused: "+message)
		return
	case outcome == TAKCertPending:
		// Not an error the customer caused, and the stash is deliberately LEFT
		// ALONE: the operator reconciles on a ticker, so the next attempt in a few
		// seconds is the one that succeeds.
		w.Header().Set("Retry-After", "10")
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status": "the certificate is being signed, try again in a few seconds",
		})
		return
	}

	keyPEM, err := openKey(stash.KeyCipher, nonce, claimID)
	if err != nil {
		// The nonce matched the hash but does not decrypt: the stash was written
		// by a different mint, or tampered with. Uniform refusal either way.
		h.log.Error("tak enrolment: sealed key did not open", "claim", claimID)
		writeError(w, http.StatusNotFound, enrolClaimRefused)
		return
	}

	// Description is left to takenroll's default ("MeshSat Hub"), which is what a
	// phone shows in its server list. One name for every tenant is deliberate: the
	// instance label is opaque precisely so it never reaches a handset.
	//
	// Two different chains go in, and swapping them is the mistake that was made
	// here once: CACertPEM is the TENANT's CA, which issued this phone's own
	// certificate, while ServerTrustPEM is the FRONT's chain, which is what the
	// phone verifies the server against.
	atak, itak, err := takenroll.Build(takenroll.Input{
		Host:           h.publicHost,
		Port:           h.port,
		Username:       stash.Username,
		KeyPEM:         keyPEM,
		CertPEM:        []byte(material.CertPEM),
		CACertPEM:      []byte(material.CACertPEM),
		ServerTrustPEM: h.frontTrust,
	})
	if err != nil {
		h.log.Error("tak enrolment: building the package failed",
			"tenant", stash.TenantID, "username", stash.Username, "error", err)
		writeError(w, http.StatusInternalServerError, "could not build the package")
		return
	}
	bundle := atak
	if strings.EqualFold(r.URL.Query().Get("client"), "itak") {
		bundle = itak
	}

	// Single use. Everything that could produce this package again is cleared
	// BEFORE the bytes go out: if the write fails, the customer retries and gets
	// a fresh enrolment, which is better than a token that works twice.
	h.clearEnrolment(ctx, claimID, stash)

	user.CertSerial = material.Serial
	notAfter := material.NotAfter
	user.CertNotAfter = &notAfter
	user.EnrollTokenHash = ""
	user.EnrollExpiresAt = nil
	if err := h.store.UpdateTAKUser(ctx, stash.TenantID, user); err != nil {
		h.log.Error("tak enrolment: could not record the issued certificate",
			"tenant", stash.TenantID, "username", stash.Username, "error", err)
	}
	h.log.Info("tak enrolment: package claimed",
		"tenant", stash.TenantID, "username", stash.Username,
		"serial", material.Serial, "file", bundle.Describe())

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+bundle.Filename+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(bundle.Data); err != nil {
		h.log.Warn("tak enrolment: the package was not fully delivered",
			"username", stash.Username, "error", err)
	}
}

// clearEnrolment makes a claim unusable. Blanks the stash rather than deleting a
// row, because store has GetSystemConfig and SetSystemConfig and no delete --
// the same thing bridge_provision does.
func (h *TenantTAKHandler) clearEnrolment(ctx context.Context, claimID string, stash enrolStash) {
	if err := h.store.SetSystemConfig(ctx, enrolStashKey(claimID), ""); err != nil {
		h.log.Error("tak enrolment: could not clear a used stash", "claim", claimID, "error", err)
	}
	if err := h.certs.Discard(ctx, stash.Label, stash.Username); err != nil {
		h.log.Warn("tak enrolment: could not discard the certificate request",
			"username", stash.Username, "error", err)
	}
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

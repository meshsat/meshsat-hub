package takoperator

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"regexp"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// usernamePattern mirrors the CRD. Lowercase alphanumeric with NO hyphens,
// because OpenTAKServer 1.7.13 answers HTTP 400 to a username containing one —
// found while building the end-to-end test in MESHSAT-1046, where
// `meshsat-proxy` was refused and `meshsatproxy` accepted. Checked here as well
// as in the schema, because a CRD is one apply away from being edited.
var usernamePattern = regexp.MustCompile(`^[a-z0-9]{3,32}$`)

// reconcileCertRequests issues every request that has not been decided yet.
//
// A request is decided exactly once. An already-Issued request is never
// re-signed: the Hub generated one key pair and asked for one certificate, and
// minting a second for the same key would leave two valid credentials where the
// Hub records one serial.
func (r *Reconciler) reconcileCertRequests(ctx context.Context) error {
	reqs, err := r.Client.ListCertRequests(ctx, r.Namespace)
	if err != nil {
		return fmt.Errorf("list certificate requests: %w", err)
	}
	var firstErr error
	for i := range reqs {
		req := reqs[i]
		if req.Status.Phase == CertIssued || req.Status.Phase == CertDenied {
			continue
		}
		if err := r.issue(ctx, &req); err != nil {
			r.Log.Warn("takoperator: certificate request failed",
				"name", req.Name, "label", req.Spec.Label, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return nil
}

// issue decides one request.
//
// Denial is terminal and carries a reason, because the alternative — retrying a
// malformed CSR every thirty seconds forever — hides the problem from whoever
// has to fix it. Only infrastructure failures are left Pending to be retried.
func (r *Reconciler) issue(ctx context.Context, req *TakCertificateRequest) error {
	deny := func(why string) error {
		return r.Client.SetCertRequestStatus(ctx, r.Namespace, req.Name, TakCertificateRequestStatus{
			Phase:              CertDenied,
			ObservedGeneration: req.Generation,
			Message:            why,
		})
	}

	switch {
	case !labelPattern.MatchString(req.Spec.Label):
		return deny("label is not ten lowercase alphanumerics")
	case !usernamePattern.MatchString(req.Spec.Username):
		return deny("username must be 3 to 32 lowercase alphanumerics with no hyphens, " +
			"because OpenTAKServer refuses anything else")
	case req.Spec.Purpose != PurposeEUD && req.Spec.Purpose != PurposeHub:
		return deny("purpose must be eud or hub")
	case req.Spec.CSRPEM == "":
		return deny("no certificate request supplied")
	}

	// The instance must exist. Without this check the Hub could obtain a
	// certificate signed by a CA for a tenant that has no instance — harmless
	// today, but it would let a bug in the Hub mint credentials for a label it
	// guessed rather than one it owns.
	instances, err := r.Client.ListInstances(ctx, r.Namespace)
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}
	var owner *TakInstance
	for i := range instances {
		if instances[i].Spec.Label == req.Spec.Label {
			owner = &instances[i]
			break
		}
	}
	if owner == nil {
		return deny("no TakInstance has label " + req.Spec.Label)
	}
	if owner.DeletionTimestamp != nil {
		return deny("the instance is being torn down")
	}

	ca, err := r.ensureCA(ctx, req.Spec.Label)
	if err != nil {
		// Infrastructure, not the requester's fault: leave it Pending so the
		// next pass retries.
		return fmt.Errorf("load CA for %s: %w", req.Spec.Label, err)
	}

	days := EUDValidDays
	if req.Spec.Purpose == PurposeHub {
		days = HubValidDays
	}

	// The username is the ONLY thing that becomes the common name. Whatever
	// subject, SANs or extensions the CSR carries are discarded — this is what
	// stops a phone asking to be called `administrator`, which upstream
	// OpenTAKServer's own enrollment would happily sign.
	issued, err := ca.IssueFromCSR([]byte(req.Spec.CSRPEM), req.Spec.Username, days)
	if err != nil {
		return deny("certificate request refused: " + err.Error())
	}

	notAfter := metav1.NewTime(issued.NotAfter)
	if err := r.Client.SetCertRequestStatus(ctx, r.Namespace, req.Name, TakCertificateRequestStatus{
		Phase:              CertIssued,
		ObservedGeneration: req.Generation,
		CertPEM:            string(issued.CertPEM),
		CACertPEM:          string(ca.CertPEM()),
		Serial:             issued.Serial,
		NotAfter:           &notAfter,
	}); err != nil {
		return fmt.Errorf("record issued certificate: %w", err)
	}
	r.Log.Info("takoperator: certificate issued",
		"label", req.Spec.Label, "purpose", req.Spec.Purpose,
		"username", req.Spec.Username, "serial", issued.Serial,
		"days", days)
	return nil
}

// renewWindow is how long before expiry a server certificate is replaced. Thirty
// days is long enough that a broken renewal is noticed by a person rather than by
// a customer.
const renewWindow = 30 * 24 * time.Hour

// expiringSoon reports whether a PEM certificate is missing, unparseable, or
// within renewWindow of expiry. Unparseable counts as expiring: the safe
// response to material we cannot read is to replace it.
func expiringSoon(certPEM []byte) bool {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return true
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return true
	}
	return time.Until(cert.NotAfter) < renewWindow
}

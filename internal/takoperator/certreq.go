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

// hubRequestTTL is how long a DECIDED hub identity request may sit uncollected
// before the operator treats it as abandoned.
//
// The Hub collects within one refresh cycle (five minutes) and deletes the object
// as it does, so anything still here an hour later belongs to a replica that no
// longer exists. An hour is far beyond any legitimate window and well short of the
// seven-day certificate it carries.
const hubRequestTTL = time.Hour

// abandonedHubRequest reports whether a request is an orphan nobody will collect.
//
// # Why this is needed at all (MESHSAT-1076)
//
// Identity requests are named per replica, so each pod has its own object. That is
// what stops two replicas deleting each other's certificates -- but it means a
// replaced pod leaves its object behind, and a Deployment is rolled on every
// deploy. Without reaping, the namespace accumulates one dead request per pod per
// tenant forever.
//
// Deliberately narrow:
//   - hub purpose only. An EUD request belongs to a customer's enrolment, which
//     has its own fifteen-minute lifecycle and is deleted on claim; reaping those
//     on a timer would be a second mechanism arguing with the first.
//   - decided only. A Pending request is the operator's own backlog, and deleting
//     work it has not done yet would lose a certificate somebody is waiting for.
//
// Pure, so the age arithmetic can be tested without a cluster -- the mistake this
// guards against is an off-by-a-timezone reap, which already happened once in this
// estate with `time.mktime` reading a UTC timestamp as local.
func abandonedHubRequest(req *TakCertificateRequest, now time.Time) bool {
	if req == nil || req.Spec.Purpose != PurposeHub {
		return false
	}
	if req.Status.Phase != CertIssued && req.Status.Phase != CertDenied {
		return false
	}
	if req.CreationTimestamp.IsZero() {
		// No age to judge by. Leaving it is the safe direction: an uncollected
		// request is clutter, a deleted one somebody needed is an outage.
		return false
	}
	return now.Sub(req.CreationTimestamp.Time) > hubRequestTTL
}

// certRequestDenyReason is the terminal-refusal check, pure so that it can be
// tested without a cluster -- the same shape userreq.go already uses.
//
// THE USERNAME RULE IS PER PURPOSE, and that is the whole point of this function
// existing. It used to be one pattern for every request, which made the Hub's own
// identity unrequestable: HubIdentityCN is "meshsat-hub", it contains a hyphen,
// and the pattern forbids hyphens. The hyphen is deliberate -- userreq.go relies
// on it to keep a tenant from managing the Hub's account through the customer API,
// and the instance's nginx gateway admits exactly CN=meshsat-hub -- so the fix is
// not to rename the Hub but to stop applying a phone's rule to it.
//
// What that bug looked like in production, with the front enabled: the API server
// refused every hub request with 422, the Hub logged "no upstream identity for
// tenant", the directory refreshed to `tenants:0 skipped:1`, and no phone in any
// tenant could have connected. The front was listening and correct; there was
// simply nothing in it. Nothing failed loudly enough to notice without reading
// the Hub's log, which is why this function now has tests.
func certRequestDenyReason(req *TakCertificateRequest) string {
	switch {
	case !labelPattern.MatchString(req.Spec.Label):
		return "label is not ten lowercase alphanumerics"
	case req.Spec.Purpose != PurposeEUD && req.Spec.Purpose != PurposeHub:
		return "purpose must be eud or hub"
	case req.Spec.Purpose == PurposeHub && req.Spec.Username != HubIdentityCN:
		// Narrower than the phone rule, not looser: a hub certificate is admitted
		// by every tenant's admin gateway, so the only name it may ever carry is
		// the Hub's own.
		return "a hub certificate is only ever issued for " + HubIdentityCN
	case req.Spec.Purpose == PurposeEUD && !usernamePattern.MatchString(req.Spec.Username):
		return "username must be 3 to 32 lowercase alphanumerics with no hyphens, " +
			"because OpenTAKServer refuses anything else"
	case req.Spec.CSRPEM == "":
		return "no certificate request supplied"
	}
	return ""
}

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
	now := time.Now()
	for i := range reqs {
		req := reqs[i]
		if req.Status.Phase == CertIssued || req.Status.Phase == CertDenied {
			// Decided. Sweep it up if nobody is ever going to collect it.
			if abandonedHubRequest(&req, now) {
				if err := r.Client.Delete(ctx, InstanceGV, r.Namespace, CertReqResource, req.Name); err != nil {
					r.Log.Warn("takoperator: could not reap an abandoned identity request",
						"name", req.Name, "error", err)
				} else {
					r.Log.Info("takoperator: reaped an abandoned identity request",
						"name", req.Name, "replica", req.Labels["tak.meshsat.net/replica"],
						"age", now.Sub(req.CreationTimestamp.Time).Round(time.Minute).String())
				}
			}
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
	// firstErr, not nil: this used to collect the first failure and then discard
	// it, so a certificate nobody could issue never reached ReconcileOnce and the
	// pass logged success. A phone whose certificate never arrives is silent on
	// somebody's map, which is the kind of failure that has to be loud.
	return firstErr
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

	if why := certRequestDenyReason(req); why != "" {
		return deny(why)
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

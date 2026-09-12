package takoperator

// OpenTAKServer accounts, created by the operator because the Hub may not read
// the administrator password.
//
// A certificate is necessary and NOT sufficient: EudHandlerSSL.setup reads the
// client certificate's common name and calls datastore.find_user(username=<CN>),
// then drops the connection with "User <x> does not exist" when nothing matches.
// The certificates table is never consulted. So issuing a phone a certificate
// does not admit it -- an account of exactly that name has to exist.
//
// The Hub cannot create one itself. Every /api/user/ route on an instance needs an
// administrator session, the password is generated per instance into
// tak-<label>-config, and the Hub's Role grants no Secrets in meshsat-tak. That is
// not an oversight to fix: the same Secret carries secret_key and password_salt,
// and RBAC cannot grant a single key, so reading it would hand the internet-facing
// process the instance's session-signing key. The operator already holds the
// password and already logs in with it during bootstrapAdmin.
//
// So the Hub asks, the operator acts -- the same contract as TakCertificateRequest,
// and the same direction as the rest of this API: the Hub decides what a customer
// wants, the operator reconciles it.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Actions a TakUserRequest spec may ask for, and the phases the operator reports.
const (
	// ActionEnsure creates the account if it is absent and leaves it able to
	// connect. Idempotent: the intent is "this person can connect", not "a row
	// was created just now".
	ActionEnsure = "Ensure"
	// ActionDeactivate switches an account off without deleting it. The EUD rows,
	// the position history and the audit trail all reference the user, and a
	// tenant removing a teammate is not asking to rewrite its map's history.
	ActionDeactivate = "Deactivate"

	// UserPending is the state of a request nobody has acted on yet.
	UserPending = "Pending"
	// UserApplied means the instance now matches the spec.
	UserApplied = "Applied"
	// UserDenied is terminal and carries a reason. Only the requester can fix it,
	// so retrying every thirty seconds forever would hide the problem instead.
	UserDenied = "Denied"
)

// TakUserRequest asks the operator to create or deactivate one OpenTAKServer
// account inside a tenant's instance.
//
// The spec carries NO PASSWORD, deliberately. The operator generates one and the
// Hub never learns it: a password here would be a live credential in etcd and in
// every Velero backup, and nothing needs it, because ATAK authenticates with its
// certificate and the account's password exists only to satisfy OpenTAKServer's
// own user row.
type TakUserRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TakUserRequestSpec   `json:"spec"`
	Status TakUserRequestStatus `json:"status,omitempty"`
}

// TakUserRequestSpec names the instance, the account and the desired state.
//
// label and username are the account's identity and the CRD holds them immutable;
// action is desired state and may be patched, so switching an account off does not
// mean deleting and recreating an object.
type TakUserRequestSpec struct {
	Label    string `json:"label"`
	Username string `json:"username"`
	Action   string `json:"action"`
}

// TakUserRequestStatus is written only by the operator. The Hub's Role grants no
// status verb on this kind, so a Hub that tried would get a 403.
type TakUserRequestStatus struct {
	Phase              string `json:"phase,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	Message            string `json:"message,omitempty"`
}

// TakUserRequestList is a list response.
type TakUserRequestList struct {
	metav1.TypeMeta `json:",inline"`
	Metadata        metav1.ListMeta  `json:"metadata,omitempty"`
	Items           []TakUserRequest `json:"items"`
}

// ErrAdminLoginRefused means the instance would not accept the administrator
// password the operator holds.
//
// Deliberately an error rather than a denial: on a fresh instance bootstrapAdmin
// may not have run yet, so the right answer is to leave the request Pending and
// try again on a later pass, not to tell the customer their request was refused.
var ErrAdminLoginRefused = errors.New(
	"takoperator: the instance refused the stored administrator password; " +
		"bootstrapAdmin may not have run yet")

// reconcileUserRequests brings every account request into line with its spec.
//
// A request is acted on when nothing has decided it yet, OR when its spec has
// moved on since the last decision. That second case is what makes this
// declarative: the object is desired state, not a job, so patching action from
// Ensure to Deactivate has to be noticed. Comparing observedGeneration with
// metadata.generation is how, and it is why status carries it.
//
// Denied is terminal. Only the requester can fix a malformed username, and
// retrying one every thirty seconds forever buries the reason instead of
// surfacing it.
func (r *Reconciler) reconcileUserRequests(ctx context.Context) error {
	reqs, err := r.Client.ListUserRequests(ctx, r.Namespace)
	if err != nil {
		return fmt.Errorf("list user requests: %w", err)
	}
	var firstErr error
	for i := range reqs {
		req := reqs[i]
		if req.Status.Phase == UserDenied {
			continue
		}
		if req.Status.Phase == UserApplied && req.Status.ObservedGeneration == req.Generation {
			continue
		}
		if err := r.applyUser(ctx, &req); err != nil {
			r.Log.Warn("takoperator: TAK account request failed",
				"name", req.Name, "label", req.Spec.Label, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// applyUser decides one request.
//
// The same split as issue(): a malformed request is DENIED with a reason, and an
// infrastructure failure is returned so the phase stays Pending and the next pass
// retries. Getting that backwards either hides a typo forever or turns a
// momentary outage into a permanent refusal a customer has to ask about.
func (r *Reconciler) applyUser(ctx context.Context, req *TakUserRequest) error {
	deny := func(why string) error {
		return r.Client.SetUserRequestStatus(ctx, r.Namespace, req.Name, TakUserRequestStatus{
			Phase:              UserDenied,
			ObservedGeneration: req.Generation,
			Message:            why,
		})
	}

	if why := userRequestRefusal(req.Spec); why != "" {
		return deny(why)
	}

	// The instance must exist and must not be going away. Without this the
	// operator would log in to an address derived from a label nobody owns.
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
		return fmt.Errorf("load CA for %s: %w", req.Spec.Label, err)
	}
	adminPw, err := r.adminPassword(ctx, req.Spec.Label)
	if err != nil {
		return err
	}
	cl, err := instanceAdminClient(ca, req.Spec.Label, r.Namespace)
	if err != nil {
		return err
	}
	defer cl.CloseIdleConnections()

	msg, refused, err := applyUserWith(ctx, cl,
		instanceAdminURL(req.Spec.Label, r.Namespace), adminPw,
		req.Spec.Username, req.Spec.Action)
	if err != nil {
		return err
	}
	if refused {
		return deny(msg)
	}
	if err := r.Client.SetUserRequestStatus(ctx, r.Namespace, req.Name, TakUserRequestStatus{
		Phase:              UserApplied,
		ObservedGeneration: req.Generation,
		Message:            msg,
	}); err != nil {
		return fmt.Errorf("record the applied account: %w", err)
	}
	r.Log.Info("takoperator: TAK account applied",
		"label", req.Spec.Label, "username", req.Spec.Username,
		"action", req.Spec.Action, "result", msg)
	return nil
}

// userRequestRefusal returns the reason a request can never be applied, or "" when
// it is acceptable.
//
// Pure, and separate from applyUser, so that every refusal -- above all the
// administrator one, which is a security check -- is testable without a Kubernetes
// API. This package has no fake API server, so a guard left inside the reconcile
// method would be a guard nothing exercises.
func userRequestRefusal(spec TakUserRequestSpec) string {
	switch {
	case !labelPattern.MatchString(spec.Label):
		return "label is not ten lowercase alphanumerics"
	case !usernamePattern.MatchString(spec.Username):
		return "username must be 3 to 32 lowercase alphanumerics with no hyphens, " +
			"because OpenTAKServer refuses anything else"
	case spec.Username == otsAdminUsername:
		// This one matters. "administrator" passes the username pattern, so without
		// this check a tenant could ask the operator to create or deactivate the
		// very account whose password the operator holds. HubIdentityCN needs no
		// check of its own: it contains a hyphen, which the pattern already
		// refuses.
		return "the administrator account cannot be managed through this API"
	case spec.Action != ActionEnsure && spec.Action != ActionDeactivate:
		return "action must be Ensure or Deactivate"
	}
	return ""
}

// applyUserWith is the HTTP half, separated from the Kubernetes and TLS plumbing
// so it can be tested against an ordinary test server -- the same split as
// bootstrapAdminWith.
//
// refused true means the instance rejected the request in a way only the
// requester can fix, so the caller denies it. An error means try again later.
func applyUserWith(ctx context.Context, cl *http.Client, base, adminPassword, username, action string) (
	message string, refused bool, err error) {
	tok, err := otsLogin(ctx, cl, base, adminPassword)
	if err != nil {
		return "", false, fmt.Errorf("logging in to the instance: %w", err)
	}
	if tok == "" {
		return "", false, ErrAdminLoginRefused
	}

	switch action {
	case ActionDeactivate:
		raw, status, err := otsPost(ctx, cl, base+"/api/user/deactivate", tok,
			map[string]string{"username": username})
		if err != nil {
			return "", false, err
		}
		switch {
		case status == http.StatusOK:
			return "account deactivated", false, nil
		case status == http.StatusBadRequest && absentUser(raw):
			// Desired state already holds: no account means nobody can connect.
			// Treating this as applied is what makes a retry safe after a partial
			// failure, and what makes deactivating an already-removed teammate a
			// no-op rather than an error somebody has to read.
			return "no such account, nothing to deactivate", false, nil
		case status == http.StatusBadRequest:
			return "deactivating the account: " + firstLine(raw), true, nil
		default:
			return "", false, fmt.Errorf("takoperator: deactivate returned %d: %s", status, firstLine(raw))
		}

	case ActionEnsure:
		// The operator generates the password and nobody else ever holds it. It is
		// not returned, not logged and not written to the status: a TAK account
		// authenticates with its certificate, and this value exists only because
		// OpenTAKServer requires a user row to have one. Base64url from
		// randomValue, which cannot produce the ':' or '@' that upstream refuses.
		pw, err := randomValue("admin_password")
		if err != nil {
			return "", false, err
		}
		raw, status, err := otsPost(ctx, cl, base+"/api/user/add", tok, map[string]string{
			"username":         username,
			"password":         pw,
			"confirm_password": pw,
		})
		if err != nil {
			return "", false, err
		}
		switch {
		case status == http.StatusOK:
			return "account created", false, nil
		case status == http.StatusBadRequest && existingUser(raw):
			// Idempotent: the intent is "this person can connect", not "a row was
			// created just now", so a second pass over an existing account is
			// success.
			//
			// KNOWN LIMITATION, stated rather than hidden: this does NOT re-enable
			// an account that was deactivated. OpenTAKServer's deactivate route is
			// verified, an activate route is not -- it is absent from the Hub's own
			// client too -- and inventing an endpoint is exactly how the inert
			// .admin_password file happened. So re-admitting a removed teammate
			// currently means a new username until that route is confirmed against
			// a live instance.
			return "account already exists", false, nil
		case status == http.StatusBadRequest:
			return "creating the account: " + firstLine(raw), true, nil
		default:
			return "", false, fmt.Errorf("takoperator: user add returned %d: %s", status, firstLine(raw))
		}
	}
	// Unreachable: applyUser validates the action first. Returned rather than
	// panicking, because a future action added to the CRD and not here should
	// surface as a refusal naming itself.
	return "unsupported action " + action, true, nil
}

// existingUser and absentUser read OpenTAKServer's 400 bodies, which carry the
// distinction its status code does not: upstream answers 400 both for "that name
// is taken" and for "that name is not allowed", and the caller needs to tell a
// successful idempotent call from a refusal.
func existingUser(raw []byte) bool {
	return strings.Contains(strings.ToLower(firstLine(raw)), "already exists")
}

func absentUser(raw []byte) bool {
	s := strings.ToLower(firstLine(raw))
	return strings.Contains(s, "does not exist") || strings.Contains(s, "not found")
}

package takhosted

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// AccountKeeper asks the operator for the OpenTAKServer accounts a tenant's
// phones authenticate as.
//
// # Why the Hub asks instead of acting
//
// A certificate is necessary and NOT sufficient. EudHandlerSSL.setup reads a
// client certificate's common name and calls datastore.find_user(username=<CN>),
// dropping the connection with "User <x> does not exist" when nothing matches;
// the certificates table is never consulted. So a phone with a perfectly good
// certificate is refused until an account of exactly that name exists.
//
// The Hub cannot create one. Every /api/user/ route needs an administrator
// session, that password is generated per instance into tak-<label>-config, and
// the Hub's Role grants no Secrets in meshsat-tak -- nor should it, since the same
// Secret carries secret_key and password_salt and Kubernetes RBAC cannot grant a
// single key. The operator holds the password already. So the Hub writes a
// TakUserRequest and the operator reconciles it.
//
// # Nothing here blocks
//
// The operator works on a ticker, so a fresh request is Pending for a few
// seconds. Ensure returns ErrAccountPending immediately and the caller reports
// "provisioning" rather than holding an HTTP request open -- the same choice
// IdentityKeeper makes, and for the same reason.
type AccountKeeper struct {
	client *Client
	log    *slog.Logger
}

// Errors a caller distinguishes.
var (
	// ErrAccountPending means the request exists but the operator has not applied
	// it yet. Not a failure: ask again shortly.
	ErrAccountPending = errors.New("takhosted: the TAK account is still being provisioned")
	// ErrAccountDenied is terminal and carries the operator's reason. Retrying
	// changes nothing; the request itself has to change.
	ErrAccountDenied = errors.New("takhosted: the TAK account request was refused")
)

// NewAccountKeeper wires one up.
func NewAccountKeeper(c *Client, log *slog.Logger) *AccountKeeper {
	if log == nil {
		log = slog.Default()
	}
	return &AccountKeeper{client: c, log: log}
}

// AccountState is what the Hub can say about an account without asking the
// instance.
type AccountState struct {
	Action  string
	Phase   string
	Message string
	// Current is false when the operator has not caught up with the latest spec,
	// so Phase describes the PREVIOUS desired state and must not be read as the
	// answer to the question just asked.
	Current bool
}

// Ensure asks for an account that can connect. Idempotent: the intent is "this
// person can connect", and applying the same desired state twice is one
// server-side apply that changes nothing.
func (k *AccountKeeper) Ensure(ctx context.Context, label, username string) error {
	return k.apply(ctx, label, username, ActionEnsure)
}

// Deactivate switches an account off without deleting it, so the EUD rows, the
// position history and the audit trail that reference the user survive.
//
// Note what this is NOT: deleting the request object. That would remove the Hub's
// record of the desired state and leave the account exactly as it was -- able to
// connect.
func (k *AccountKeeper) Deactivate(ctx context.Context, label, username string) error {
	return k.apply(ctx, label, username, ActionDeactivate)
}

// State reports what the operator has done with an account, for a surface that
// wants to show a person where their request got to.
//
// A request that does not exist is not an error: it means nobody has asked, which
// is the ordinary state of most accounts.
func (k *AccountKeeper) State(ctx context.Context, label, username string) (AccountState, error) {
	if err := checkAccountArgs(label, username); err != nil {
		return AccountState{}, err
	}
	req, err := k.client.GetUserRequest(ctx, userRequestName(label, username))
	if IsNotFound(err) {
		return AccountState{}, nil
	}
	if err != nil {
		return AccountState{}, fmt.Errorf("takhosted: reading the account request: %w", err)
	}
	return stateOf(req), nil
}

// apply writes the desired state and reports where the operator has got to.
func (k *AccountKeeper) apply(ctx context.Context, label, username, action string) error {
	if k == nil || k.client == nil {
		return errors.New("takhosted: account keeper has no client")
	}
	if err := checkAccountArgs(label, username); err != nil {
		return err
	}

	name := userRequestName(label, username)

	// Read first, so an account already in the wanted state costs one GET and no
	// write. Without this, every poll would patch the object and -- because a
	// no-op apply does not bump generation -- the saving is real but the noise in
	// managedFields is not worth it.
	req, err := k.client.GetUserRequest(ctx, name)
	switch {
	case err == nil:
		st := stateOf(req)
		if req.Spec.Action == action && st.Current {
			switch st.Phase {
			case UserApplied:
				return nil
			case UserDenied:
				return fmt.Errorf("%w: %s", ErrAccountDenied, st.Message)
			}
		}
	case IsNotFound(err):
		// Nobody has asked yet.
	default:
		return fmt.Errorf("takhosted: reading the account request: %w", err)
	}

	if err := k.client.CreateUserRequest(ctx, TakUserRequest{
		Metadata: objectMeta{Name: name},
		Spec:     UserReqSpec{Label: label, Username: username, Action: action},
	}); err != nil {
		return fmt.Errorf("takhosted: asking for the TAK account: %w", err)
	}
	k.log.Info("takhosted: asked the operator for a TAK account",
		"username", username, "action", action)

	// Deliberately pending rather than a read-back loop. The operator reconciles
	// on its own ticker, and holding a customer's HTTP request open waiting for
	// another process to tick is how a web request becomes a timeout.
	return ErrAccountPending
}

// stateOf reads a request's status, refusing to treat a stale one as current.
//
// The generation check is the whole point. Patching action from Ensure to
// Deactivate leaves the old "Applied" status in place until the operator acts, so
// a reader that ignored observedGeneration would report a teammate switched off
// while they were still connecting.
func stateOf(req *TakUserRequest) AccountState {
	if req == nil {
		return AccountState{}
	}
	return AccountState{
		Action:  req.Spec.Action,
		Phase:   req.Status.Phase,
		Message: req.Status.Message,
		// A generation of zero means the API server has not told us, which happens
		// only in a fabricated object; treat it as not current rather than
		// accidentally equal.
		Current: req.Metadata.Generation != 0 &&
			req.Status.ObservedGeneration == req.Metadata.Generation,
	}
}

// checkAccountArgs refuses what the instance or the CRD would refuse anyway, so
// the error names the rule instead of arriving as a 400 or an admission failure.
func checkAccountArgs(label, username string) error {
	if label == "" {
		return errors.New("takhosted: tenant has no TAK instance label")
	}
	if !otsUsernamePattern.MatchString(username) {
		return fmt.Errorf("%w: %q", ErrBadUsername, username)
	}
	return nil
}

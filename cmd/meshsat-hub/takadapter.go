package main

import (
	"context"
	"errors"
	"strings"

	"github.com/meshsat/meshsat-hub/internal/api"
	"github.com/meshsat/meshsat-hub/internal/takhosted"
)

// Adapting internal/takhosted to the interfaces internal/api declares.
//
// internal/api must not import internal/takhosted. internal/routing imports
// internal/api, and routing is on an ingest path, so that import would put the
// Kubernetes custom-resource client and its in-cluster REST config behind every
// satellite message the Hub receives -- the same coupling that already forces
// internal/quota's invariant test to scan source text instead of the dependency
// graph (MESHSAT-992).
//
// So the API names the two things it needs, and this file -- in the one package
// that legitimately knows about both -- supplies them. The translation here is
// the whole reason the seam exists: takhosted reports with sentinel errors, and
// the API wants an outcome it can turn into a status code.

// takhosted.Provisioner already has the shape the API asked for.
var _ api.TAKProvisioner = (*takhosted.Provisioner)(nil)

// takAccounts turns the account keeper's sentinels into API outcomes.
type takAccounts struct{ keeper *takhosted.AccountKeeper }

var _ api.TAKAccounts = takAccounts{}

func (a takAccounts) Ensure(ctx context.Context, label, username string) (api.TAKAccountOutcome, string, error) {
	return translateAccountResult(a.keeper.Ensure(ctx, label, username))
}

func (a takAccounts) Deactivate(ctx context.Context, label, username string) (api.TAKAccountOutcome, string, error) {
	return translateAccountResult(a.keeper.Deactivate(ctx, label, username))
}

// translateAccountResult maps one error into an outcome.
//
// The distinction that matters: pending and refused are both "not done", but only
// one of them is the customer's to fix. Pending means the operator reconciles on a
// ticker and has not got there yet, so the API answers 202 and the page says
// "being created". Refused is terminal and carries a reason, so the API answers
// 422 and shows it. Collapsing the two would either nag about a transient state or
// hide a real refusal behind a spinner forever.
func translateAccountResult(err error) (api.TAKAccountOutcome, string, error) {
	switch {
	case err == nil:
		return api.TAKAccountApplied, "", nil

	case errors.Is(err, takhosted.ErrAccountPending):
		return api.TAKAccountPending, "", nil

	case errors.Is(err, takhosted.ErrAccountDenied):
		return api.TAKAccountRefused, customerReason(err), nil

	case errors.Is(err, takhosted.ErrBadUsername):
		// Caught by the handler's own pattern check already, so reaching here means
		// the two disagree. Still a refusal rather than a retry: a name the
		// instance will not accept does not become acceptable later.
		return api.TAKAccountRefused, customerReason(err), nil

	default:
		// Anything else is infrastructure: the handler turns a non-nil error into a
		// 503 and a "try again shortly", and the outcome is not read.
		return api.TAKAccountPending, "", err
	}
}

// customerReason strips the package prefixes off a message that is about to be
// shown to a person. "takhosted: the TAK account request was refused: username
// must be 3 to 32..." is a log line; the customer needs the last clause.
func customerReason(err error) string {
	msg := err.Error()
	for _, cut := range []string{
		"takhosted: the TAK account request was refused: ",
		"takhosted: ",
	} {
		msg = strings.TrimPrefix(msg, cut)
	}
	return msg
}

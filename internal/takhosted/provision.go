package takhosted

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Provisioner turns "this customer wants TAK" into a TakInstance the operator
// will build, and records the Hub's own row for it.
//
// The Hub creates instances and the operator reacts -- never the other way round.
// The operator's RBAC deliberately grants it no create or delete on these
// resources, because an operator that could create instances could provision for
// a tenant nobody asked about.
type Provisioner struct {
	client *Client
	store  store.Store
	log    *slog.Logger
}

// NewProvisioner wires one up.
func NewProvisioner(c *Client, s store.Store, log *slog.Logger) *Provisioner {
	if log == nil {
		log = slog.Default()
	}
	return &Provisioner{client: c, store: s, log: log}
}

// labelBytes is five, because five bytes hex-encode to exactly the ten characters
// the CRD demands. Hex rather than base64url deliberately: the pattern is
// ^[a-z0-9]{10}$, and base64url produces uppercase letters, '-' and '_', none of
// which it accepts -- and the label also becomes a Postgres database name
// (tak_<label>) and part of object names, where those characters are worse than
// merely rejected.
const labelBytes = 5

// labelMintAttempts bounds the collision retry. Five random bytes is a trillion
// values against a handful of instances, so one attempt is essentially always
// enough; the loop exists so a collision is handled rather than returned as a
// confusing unique-index violation from the store.
const labelMintAttempts = 8

// stateRunning is what a new instance asks for. It must match the CRD's enum.
const stateRunning = "Running"

// phaseProvisioning is what the Hub records until the operator reports otherwise.
// The directory refresher overwrites every status field from the custom resource
// on its next pass, so this value is a placeholder with a short life, not a claim.
const phaseProvisioning = "Provisioning"

// EnableTAK makes sure the tenant has a hosted TAK server and returns its label.
//
// Idempotent by design, because this sits behind a customer-facing POST that a
// double-click or a retry will send twice: a tenant that already has an instance
// gets the same label back and nothing is created.
func (p *Provisioner) EnableTAK(ctx context.Context, tenantID string) (string, error) {
	if p == nil || p.client == nil || p.store == nil {
		return "", errors.New("takhosted: provisioner is not configured")
	}
	if tenantID == "" {
		return "", errors.New("takhosted: cannot enable TAK without a tenant")
	}

	switch existing, err := p.store.GetTAKInstance(ctx, tenantID); {
	case err == nil && existing != nil:
		return existing.Label, nil
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return "", fmt.Errorf("takhosted: reading the tenant's TAK instance: %w", err)
	}

	label, err := p.mintLabel(ctx)
	if err != nil {
		return "", err
	}

	// The custom resource first. It is the operator's instruction and the only
	// thing that actually builds a server, so a row written before it could leave
	// the API reporting a server that will never appear.
	if err := p.client.ApplyInstance(ctx, TakInstance{
		Metadata: objectMeta{Name: instanceObjectName(label)},
		Spec:     InstanceSpec{TenantID: tenantID, Label: label, State: stateRunning},
	}); err != nil {
		return "", fmt.Errorf("takhosted: creating the TAK instance: %w", err)
	}

	if err := p.store.UpsertTAKInstance(ctx, &store.TAKInstance{
		TenantID: tenantID,
		Label:    label,
		State:    stateRunning,
		Phase:    phaseProvisioning,
	}); err != nil {
		// The instance exists and the operator will build it; only the Hub's cache
		// of that fact is missing, and the refresher writes it from the custom
		// resource on its next pass. Failing the customer's request here would be
		// wrong: their server IS coming.
		p.log.Warn("takhosted: TAK instance created but its row was not written",
			"tenant", tenantID, "error", err)
	}

	p.log.Info("takhosted: hosted TAK enabled for a tenant", "tenant", tenantID)
	return label, nil
}

// mintLabel picks an unused label.
//
// Random, and never derived from the tenant id or slug: the label appears in the
// CA subject, which travels to every phone in the tenant, and in object names
// anyone with namespace access can list. It must say nothing about who the
// customer is.
func (p *Provisioner) mintLabel(ctx context.Context) (string, error) {
	existing, err := p.store.ListTAKInstances(ctx)
	if err != nil {
		// Without the list a collision cannot be ruled out, and the label carries a
		// UNIQUE index plus a database name. Refusing is better than writing one
		// that might already belong to another tenant.
		return "", fmt.Errorf("takhosted: listing TAK instances to pick a label: %w", err)
	}
	taken := make(map[string]bool, len(existing))
	for _, inst := range existing {
		if inst != nil {
			taken[inst.Label] = true
		}
	}

	for i := 0; i < labelMintAttempts; i++ {
		label, err := mintLabelValue()
		if err != nil {
			return "", err
		}
		if !taken[label] {
			return label, nil
		}
	}
	return "", fmt.Errorf("takhosted: could not find an unused TAK label in %d attempts",
		labelMintAttempts)
}

// mintLabelValue produces one candidate label and touches nothing else.
//
// Split out from mintLabel deliberately: the property that matters -- that every
// value it can produce satisfies the CRD's ^[a-z0-9]{10}$ -- is then testable
// without a store, a cluster or a fixture.
func mintLabelValue() (string, error) {
	b := make([]byte, labelBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("takhosted: generating a TAK label: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// instanceObjectName is the TakInstance's object name, which must agree with the
// operator's own InstanceName. They are separate constants in separate packages
// because this one may not import that one, so a test keeps them honest.
func instanceObjectName(label string) string { return "tak-" + label }

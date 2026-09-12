package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"

	"github.com/meshsat/meshsat-hub/internal/api"
	"github.com/meshsat/meshsat-hub/internal/audit"
	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/config"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/takfront"
	"github.com/meshsat/meshsat-hub/internal/takhosted"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// defaultTAKFrontAddr is the standard TAK SSL port. It matches the public port
// so the edge configuration reads straight across.
const defaultTAKFrontAddr = ":8089"

// defaultTAKPort is the same number as an integer, for the API to report.
const defaultTAKPort = 8089

// takPublicPort is the port a phone connects to.
//
// Taken from the front's own listen address because the two are deliberately the
// same number: the edge forwards 8089 to 8089 so the haproxy configuration and
// the enrollment package read straight across. A malformed address falls back to
// the standard port rather than reporting zero, since a customer seeing "port 0"
// learns nothing and the front would not have started anyway.
func takPublicPort(addr string) int {
	if addr == "" {
		addr = defaultTAKFrontAddr
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return defaultTAKPort
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n > 65535 {
		return defaultTAKPort
	}
	return n
}

// defaultTAKPublicHost is the fallback name a phone dials.
const defaultTAKPublicHost = "hub.meshsat.net"

// takPublicHost is the hostname that goes into an enrolment package.
//
// It must be the PUBLIC name, never the instance's in-cluster Service address:
// the connectString in a .pref is read by a handset on the internet, and an
// in-cluster name resolves to nothing there. Derived from the Hub's own public
// URL because that is the name the customer already reaches us by, and the edge
// forwards 8089 on the same hostnames.
//
// Falls back rather than returning empty: an empty connectString produces a
// package that fails on the phone with nothing at all to explain it.
func takPublicHost(publicURL string) string {
	if publicURL == "" {
		return defaultTAKPublicHost
	}
	u, err := url.Parse(publicURL)
	if err != nil || u.Hostname() == "" {
		return defaultTAKPublicHost
	}
	return u.Hostname()
}

// startTAKFront brings up the hosted TAK front (MESHSAT-1037).
//
// Everything here fails CLOSED. The front presents one server certificate to
// every phone in every tenant, and it cannot be generated on the fly: each
// tenant's truststore carries that certificate's chain, so a Hub that invented
// its own would be refused by every phone already enrolled. No certificate means
// no front, said plainly, rather than a listener that accepts connections it
// cannot complete.
//
// Ordering matters in two places and both are load-bearing:
//
//   - PublishEmpty runs BEFORE Serve. takfront.Server.Serve reads
//     dir.Load().Len() on its first line and SetDirectory(nil) is a silent no-op,
//     so a front started with no snapshot panics the moment it listens -- and a
//     cluster with no TAK tenants yet is the ordinary starting state.
//   - The refresher and the server are plain goroutines, not leader singletons.
//     The front is a door: every replica accepts phones and must resolve any
//     tenant's certificate issuer.
//
// The outbound forwarder registered at the end is the one exception to that: it
// writes into tenants' servers, so it runs on the lease holder alone.
//
// cfg is taken by VALUE, because main.go holds it that way and every other
// consumer there receives it the same way. A pointer here would be the only one,
// and it bought nothing.
func startTAKFront(
	ctx context.Context,
	cfg config.Config,
	dataStore store.Store,
	auditSvc *audit.Service,
	tenantStatus *tenancy.StatusCache,
	msgBus bus.MessageBus,
	purgeJob *tenancy.PurgeJob,
	takHandler *api.TenantTAKHandler,
	upstreams *takhosted.Upstreams,
) error {
	if cfg.TAKFrontCertFile == "" || cfg.TAKFrontKeyFile == "" {
		return errors.New("HUB_TAK_FRONT_CERT_FILE and HUB_TAK_FRONT_KEY_FILE must both be set: " +
			"the front presents ONE certificate to every tenant's phones and cannot mint its own")
	}
	serverCert, err := tls.LoadX509KeyPair(cfg.TAKFrontCertFile, cfg.TAKFrontKeyFile)
	if err != nil {
		return fmt.Errorf("loading the TAK front certificate: %w", err)
	}

	crClient, err := takhosted.NewInClusterClient(cfg.TAKNamespace)
	if err != nil {
		return fmt.Errorf("building the Kubernetes client for the TAK custom resources: %w", err)
	}

	// Closing an account has to destroy its TAK server and that server's
	// database, not only the Hub's rows. Attached here because the custom-resource
	// client is built here, and only when hosted TAK is actually configured: a Hub
	// without it purges exactly as it did before.
	if purgeJob != nil {
		purgeJob.SetTAKInstances(crClient)
	}

	// The customer-facing surface: turning TAK on, and the accounts a tenant's
	// phones authenticate as. Attached here for the same reason -- the
	// custom-resource client lives in this function -- and through the adapter in
	// takadapter.go, because internal/api must not import internal/takhosted.
	if takHandler != nil {
		takHandler.SetTAK(
			takhosted.NewProvisioner(crClient, dataStore, slog.Default()),
			takAccounts{keeper: takhosted.NewAccountKeeper(crClient, slog.Default())},
		)
		// Enrolment packages (MESHSAT-1040). Attached here for the same reason and
		// with the same consequence: without hosted TAK configured, minting an
		// enrolment answers 503 rather than panicking on a nil keeper.
		//
		// The front's certificate chain goes with it, read from the same file the
		// listener above was built from, so the truststore a phone is given is by
		// construction the chain of the certificate it will actually be shown.
		// Deriving it from serverCert's DER would work too and would drop the
		// root, which the file has and a truststore wants.
		frontTrustPEM, err := os.ReadFile(cfg.TAKFrontCertFile)
		if err != nil {
			return fmt.Errorf("reading the TAK front certificate for the enrolment truststore: %w", err)
		}
		takHandler.SetTAKCerts(
			takCerts{keeper: takhosted.NewCertKeeper(crClient, slog.Default())},
			frontTrustPEM,
		)
	}

	// Tenant status is a STRING, so the adapter compares explicitly against
	// active. Writing it as `status != suspended` would admit a DELETED tenant,
	// which is the kind of polarity mistake that reads fine and serves a tenant
	// that asked to be closed.
	tenantActive := func(ctx context.Context, tenantID string) (bool, error) {
		st, err := tenantStatus.Status(ctx, tenantID)
		if err != nil {
			return false, err
		}
		return st == store.TenantActive, nil
	}

	authz := takhosted.NewAuthorizer(dataStore, tenantActive)
	rec := takfront.NewRecorder(auditSvc, slog.Default())

	srv, err := takfront.NewServer(takfront.Config{
		Certificate: serverCert,
		Logger:      slog.Default(),
	}, authz, rec)
	if err != nil {
		return fmt.Errorf("building the TAK front: %w", err)
	}

	keeper := takhosted.NewIdentityKeeper(crClient, slog.Default())
	refresher := takhosted.NewDirectoryRefresher(crClient, keeper, srv.SetDirectory, slog.Default())
	// The refresher is the ONLY writer of the tak_instances status columns: it
	// holds the custom resource, so it is the only thing that has phase, host and
	// the CA certificate together. UpsertTAKInstance overwrites all three
	// unconditionally, so a second partial writer would blank the CA and drop
	// every tenant out of the directory.
	refresher.SetStore(dataStore)

	// Before Serve, always. See the function comment.
	if err := refresher.PublishEmpty(); err != nil {
		return fmt.Errorf("publishing the initial (empty) tenant directory: %w", err)
	}

	addr := cfg.TAKFrontAddr
	if addr == "" {
		addr = defaultTAKFrontAddr
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s for the TAK front: %w", addr, err)
	}

	go func() {
		if err := srv.Serve(ctx, ln); err != nil {
			slog.Error("takfront: serve stopped", "error", err)
		}
	}()
	// Per replica, not a singleton.
	go refresher.Run(ctx)

	// Hand the hosted-instance lookup to the shared upstream resolver.
	//
	// The outbound forwarder itself is registered by startTAKOutbound, OUTSIDE this
	// function and outside HUB_TAK_FRONT_ENABLED, because a tenant who brings their
	// own TAK server (MESHSAT-1065) needs no front at all -- and the front needs a
	// server certificate that does not exist yet. Until this line runs, only
	// tenants' own servers resolve, which is correct rather than degraded.
	if upstreams != nil {
		upstreams.SetHosted(refresher.TenantByID)
	}

	slog.Info("takfront: listening", "addr", addr, "tak_namespace", takhosted.TakNamespace())
	return nil
}

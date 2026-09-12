package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"

	"github.com/meshsat/meshsat-hub/internal/api"
	"github.com/meshsat/meshsat-hub/internal/audit"
	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/config"
	"github.com/meshsat/meshsat-hub/internal/leader"
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
	singletons *leader.Singletons,
	purgeJob *tenancy.PurgeJob,
	takHandler *api.TenantTAKHandler,
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

	// The outbound half: a tenant's own kit positions pushed into that tenant's
	// OpenTAKServer, so a customer's satellite devices appear on their ATAK map
	// beside their phones.
	//
	// This one IS a leader singleton. It writes, and two replicas forwarding the
	// same position would draw every device on the map twice.
	//
	// srv.DialTenant rather than a dial of its own: that method carries
	// takfront's upstream TLS policy -- verify the chain against the tenant's own
	// CA while skipping only the name check -- and deliberately does not spend a
	// slot from the tenant's MaxConnsPerTenant phone budget. A reimplementation
	// here would be the place those two properties quietly diverge.
	//
	// Registered unconditionally: Run checks the bus for itself and says so if it
	// is absent, and it runs when leadership is acquired rather than now, which is
	// a different moment from this one.
	fwd := takhosted.NewForwarder(msgBus, dataStore, srv.DialTenant, refresher.TenantByID, slog.Default())
	singletons.Add("takhosted-outbound", fwd.Run)

	slog.Info("takfront: listening", "addr", addr, "tak_namespace", takhosted.TakNamespace())
	return nil
}

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/meshsat/meshsat-hub/internal/audit"
	"github.com/meshsat/meshsat-hub/internal/config"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/takfront"
	"github.com/meshsat/meshsat-hub/internal/takhosted"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// defaultTAKFrontAddr is the standard TAK SSL port. It matches the public port
// so the edge configuration reads straight across.
const defaultTAKFrontAddr = ":8089"

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
// cfg is taken by VALUE, because main.go holds it that way and every other
// consumer there receives it the same way. A pointer here would be the only one,
// and it bought nothing.
func startTAKFront(
	ctx context.Context,
	cfg config.Config,
	dataStore store.Store,
	auditSvc *audit.Service,
	tenantStatus *tenancy.StatusCache,
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

	slog.Info("takfront: listening", "addr", addr, "tak_namespace", takhosted.TakNamespace())
	return nil
}

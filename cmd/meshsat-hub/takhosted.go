package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/meshsat/meshsat-hub/internal/api"
	"github.com/meshsat/meshsat-hub/internal/config"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/takhosted"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// takReplicaID is who this process is, for the per-replica identity requests.
//
// The pod name on Kubernetes, the hostname otherwise, and an empty string if
// neither is available -- in which case NewIdentityKeeper generates a random id
// rather than letting two replicas share one. It must never silently collide:
// that is MESHSAT-1076, where two replicas deleted each other's certificates
// forever and the Hub could reach no tenant's server.
//
// Deliberately the same source as mqttClientID and leaderInstanceID, so a reader
// finds one answer to "how does a replica know which one it is" rather than three.
func takReplicaID() string {
	if pod := os.Getenv("POD_NAME"); pod != "" {
		return pod
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return ""
}

// startTAKHosted brings up hosted TAK (MESHSAT-1037): the per-tenant
// OpenTAKServer instances the operator builds, and the Hub's way to each.
//
// What it starts, on EVERY replica (none of it is a leader singleton: the
// outbound forwarder is, and a new leader must find every tenant already
// resolved instead of waiting a refresh):
//
//   - the custom-resource client, and through it the customer's switch
//     (POST /api/tenant/tak) and the purge's teardown of a closed tenant's
//     server and database;
//   - the IdentityKeeper, which asks the operator for this replica's own
//     certificate for each instance and keeps it current;
//   - the DirectoryRefresher, which turns each Ready instance into something
//     the forwarder can dial, writes the tak_instances rows the console reads,
//     and is handed to the shared upstream resolver.
//
// Until 11 Nov 2026 all of this was started by the public TAK front on :8089,
// next to its listener, so turning the front off would also have stopped the
// Hub reaching every hosted server. The front closed with the public port
// (MESHSAT-1466; the owner ruled the next day that hosted servers stay, behind
// the Hub), and this is the half that was not the front. Phones no longer connect
// to the Hub at all: kits and the MeshSat apps reach TAK through it over MQTT.
//
// Not fatal on failure, by the caller's choice: a Hub that refused to start
// because the cluster's API server was briefly unreachable would take satellite
// ingest, SMS and the dashboard down with TAK.
//
// cfg is taken by VALUE, as every other consumer in main.go receives it.
func startTAKHosted(
	ctx context.Context,
	cfg config.Config,
	dataStore store.Store,
	purgeJob *tenancy.PurgeJob,
	tenantEvict *tenancy.Evictor,
	takHandler *api.TenantTAKHandler,
	upstreams *takhosted.Upstreams,
) error {
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

	// The customer-facing switch. Attached here for the same reason; without it
	// POST /api/tenant/tak answers 503 rather than panicking on a nil provisioner.
	if takHandler != nil {
		takHandler.SetTAK(takhosted.NewProvisioner(crClient, dataStore, slog.Default()))
	}

	// The replica id is part of every identity request's object name, so two
	// replicas never share one certificate (MESHSAT-1076). Same source as the MQTT
	// client id and the leader identity: the pod name from the downward API.
	keeper := takhosted.NewIdentityKeeper(crClient, takReplicaID(), slog.Default())
	// A held identity survives until the certificate's own renewal window, so
	// without this a purged tenant kept a usable hosted-TAK identity resident for
	// weeks (MESHSAT-1109).
	tenantEvict.Register(keeper)

	// No directory sink: the snapshot used to be handed to the front's listener.
	refresher := takhosted.NewDirectoryRefresher(crClient, keeper, nil, slog.Default())
	// The refresher is the ONLY writer of the tak_instances status columns: it
	// holds the custom resource, so it is the only thing that has phase, host and
	// the CA certificate together. UpsertTAKInstance overwrites all three
	// unconditionally, so a second partial writer would blank the CA and drop
	// every tenant out of the directory.
	refresher.SetStore(dataStore)
	if err := refresher.PublishEmpty(); err != nil {
		return fmt.Errorf("installing the initial (empty) tenant directory: %w", err)
	}
	go refresher.Run(ctx)

	// Hand the hosted-instance lookup to the shared upstream resolver. The
	// outbound forwarder itself is registered by startTAKOutbound, outside this
	// function and outside HUB_TAK_HOSTED_ENABLED, because a tenant who brings
	// their own TAK server (MESHSAT-1065) needs none of this. Until this line
	// runs, only tenants' own servers resolve, which is correct rather than
	// degraded.
	if upstreams != nil {
		upstreams.SetHosted(refresher.TenantByID)
	}

	slog.Info("takhosted: started", "tak_namespace", takhosted.TakNamespace())
	return nil
}

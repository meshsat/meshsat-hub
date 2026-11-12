package main

import (
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
// So the API names the one thing it needs, and this file -- in the one package
// that legitimately knows about both -- checks that it is supplied. (The TAK
// account and enrolment-certificate adapters that also lived here went with
// public port 8089, MESHSAT-1466.)

// takhosted.Provisioner already has the shape the API asked for.
var _ api.TAKProvisioner = (*takhosted.Provisioner)(nil)

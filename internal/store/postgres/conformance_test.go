package postgres

import (
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/storetest"
)

// TestConformance runs the shared store contract against Postgres.
func TestConformance(t *testing.T) {
	testDSN(t)
	storetest.Run(t, func(t *testing.T) store.Store { return testDB(t) })
}

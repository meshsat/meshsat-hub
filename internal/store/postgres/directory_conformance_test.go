package postgres

import (
	"context"
	"testing"

	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
	"github.com/meshsat/meshsat-hub/internal/directory"
	"github.com/meshsat/meshsat-hub/internal/directory/directorytest"
)

// TestDirectoryConformance runs the directory contract on Postgres, on the
// schema migration 40 creates (MESHSAT-1584 C7). The SQLite run is in
// internal/directory.
func TestDirectoryConformance(t *testing.T) {
	testDSN(t)
	directorytest.Run(t, func(t *testing.T) *directory.SQLStore {
		return directory.NewPostgresStore(testDB(t).rawDB)
	})
}

// TestDirectoryRotationCasesOnThisKeyStore runs the rotation cases (r20
// C7-R7 fence, r21 R21-T1 generations) on Postgres with this store's own
// system_config as the key store, plain and behind the sealedconfig keeper
// production uses. The in-memory key store runs are the Rotation* cases of
// TestDirectoryConformance.
func TestDirectoryRotationCasesOnThisKeyStore(t *testing.T) {
	testDSN(t)
	for _, sealed := range []bool{false, true} {
		name := "plain"
		if sealed {
			name = "sealed"
		}
		rig := func(t *testing.T) (*directory.SQLStore, directorytest.KeyStore) {
			db := testDB(t)
			if !sealed {
				return directory.NewPostgresStore(db.rawDB), db
			}
			wrap, err := hubcrypto.GenerateKey()
			if err != nil {
				t.Fatal(err)
			}
			return directory.NewPostgresStore(db.rawDB), directorytest.SealedKeyStore(db, wrap)
		}
		t.Run(name, func(t *testing.T) {
			t.Run("Fence", func(t *testing.T) { directorytest.RotationFence(t, rig) })
			t.Run("OneGeneration", func(t *testing.T) { directorytest.RotationOneGeneration(t, rig) })
			t.Run("NextGeneration", func(t *testing.T) { directorytest.RotationNextGeneration(t, rig) })
		})
	}
}

// TestDirectoryRowsLeaveWithTheirTenant: the directory tables carry
// tenant_id, so the catalogue-driven export lists them and the purge empties
// them for that tenant only, foreign keys and all.
func TestDirectoryRowsLeaveWithTheirTenant(t *testing.T) {
	testDSN(t)
	db := testDB(t)
	ctx := context.Background()
	s := directory.NewPostgresStore(db.rawDB)
	for _, tenant := range []string{"t-gone", "t-stays"} {
		c := &directory.Contact{ID: "c", TenantID: tenant, DisplayName: "C",
			Addresses: []directory.Address{{Kind: directory.KindSMS, Value: "+31600000000"}},
			Keys:      []directory.ContactKey{{Kind: "X25519_LT_PUB", Public: []byte{1}}}}
		if err := s.PutContact(ctx, c); err != nil {
			t.Fatal(err)
		}
		if err := s.PutGroup(ctx, &directory.Group{ID: "g", TenantID: tenant, Name: "G", MemberIDs: []string{"c"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SeedPrecedenceDefaults(ctx, tenant); err != nil {
			t.Fatal(err)
		}
	}
	tables := []string{"directory_contacts", "directory_addresses", "directory_contact_keys", "directory_groups",
		"directory_group_members", "directory_dispatch_policy", "directory_versions"}
	export, err := db.ExportTenant(ctx, "t-gone")
	if err != nil {
		t.Fatal(err)
	}
	for _, tbl := range tables {
		if len(export[tbl]) == 0 {
			t.Errorf("export of t-gone has no %s rows", tbl)
		}
	}
	if err := db.PurgeTenant(ctx, "t-gone"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	for _, tenant := range []string{"t-gone", "t-stays"} {
		for _, tbl := range tables {
			var n int
			// #nosec G202 -- table names from the fixed list above
			if err := db.rawDB.QueryRowContext(ctx, `SELECT count(*) FROM `+tbl+` WHERE tenant_id = $1`, tenant).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if (tenant == "t-gone") != (n == 0) {
				t.Errorf("%s: %d rows of %s after purging t-gone", tbl, n, tenant)
			}
		}
	}
}

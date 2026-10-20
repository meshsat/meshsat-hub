package bridge

import (
	"context"
	"github.com/meshsat/meshsat-hub/internal/directory"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	"testing"
)

func TestReviewR19MixedTenantMustPushFinalDeletion(t *testing.T) {
	r := newPushRig(t)
	ctx := context.Background()
	if err := r.dir.PutContact(ctx, &directory.Contact{ID: "c1", TenantID: "default", DisplayName: "Alice"}); err != nil {
		t.Fatal(err)
	}
	r.birth("default", "new-kit", true, 0, protocol.CapDirectoryV2)
	r.birth("default", "old-kit", false, 0)
	r.tick()
	sent := r.take()
	if len(sent) != 1 {
		t.Fatalf("initial pushes %d", len(sent))
	}
	if err := r.dir.DeleteContact(ctx, "default", "c1"); err != nil {
		t.Fatal(err)
	}
	r.tick()
	sent = r.take()
	t.Logf("pushes after final contact deletion: %d", len(sent))
	if len(sent) != 1 {
		t.Fatal("online new Bridge never receives revocation in a mixed-version tenant")
	}
}

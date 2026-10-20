package directory

import (
	"context"
	"testing"
)

// The review's reproduction of r19 C7-R5, with the claim argument gone:
// storing the pending key by compare-and-set is the claim now, so the
// review's claims database has nothing left to stand in for.
func TestReviewR19RotationRecoversFailedPendingKeyWrite(t *testing.T) {
	s, kv, anchor := rotationRig(t)
	ctx := context.Background()
	kv.failKey = nextKeyConfig
	if _, err := RotateSigningKey(ctx, kv, s, anchor, []string{"t1"}); err == nil {
		t.Fatal("failure not injected")
	}
	kv.failKey = ""
	_, err := RotateSigningKey(ctx, kv, s, anchor, []string{"t1"})
	t.Logf("retry after storage recovery: %v", err)
	if err != nil {
		t.Fatal("healthy retry cannot resume rotation after claim but before pending-key persistence")
	}
}

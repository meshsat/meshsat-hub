package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
)

type egressFake struct {
	owner map[string]string
	birth map[string]string
	err   error
}

func (f egressFake) DeviceBridgeID(_ context.Context, _ string, imei string) (string, error) {
	return f.owner[imei], f.err
}

func (f egressFake) GetBridge(_ context.Context, _ string, id string) (*store.Bridge, error) {
	if b, ok := f.birth[id]; ok {
		return &store.Bridge{BridgeID: id, LastBirth: b}, nil
	}
	return nil, errors.New("no bridge")
}

// The declaration is the owning bridge's last birth, per modem; anything
// missing or unreadable is no declaration, and the payload is read as before.
func TestEgressDeclarations(t *testing.T) {
	birth := `{"protocol":"meshsat/v1","bridge_id":"kit-a","interfaces":[` +
		`{"name":"iridium_imt","type":"iridium_imt","status":"online","imei":"300434060000001","egress_sealed":true},` +
		`{"name":"iridium","type":"iridium","status":"online","imei":"300434060000002"}]}`
	d := NewEgressDeclarations(egressFake{
		owner: map[string]string{"300434060000001": "kit-a", "300434060000002": "kit-a", "300434060000003": "kit-gone", "300434060000004": "kit-bad"},
		birth: map[string]string{"kit-a": birth, "kit-bad": "{not json"},
	})
	ctx := context.Background()
	for imei, want := range map[string]bool{
		"300434060000001": true,  // declared sealing
		"300434060000002": false, // listed, not declared
		"300434060000003": false, // owner without a stored birth
		"300434060000004": false, // stored birth unreadable
		"300434060000009": false, // no owner
		"":                false,
	} {
		if got := d.Sealed(ctx, "default", imei); got != want {
			t.Errorf("%q: %v, want %v", imei, got, want)
		}
	}
	if NewEgressDeclarations(egressFake{err: errors.New("store down")}).Sealed(ctx, "default", "300434060000001") {
		t.Error("a store error must not declare")
	}
	var none *EgressDeclarations
	if none.Sealed(ctx, "default", "300434060000001") {
		t.Error("a nil reader declared")
	}
}

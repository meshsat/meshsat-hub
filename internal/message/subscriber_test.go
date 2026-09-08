package message

import (
	"context"
	"errors"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
)

type captureStore struct {
	store.Store
	inserted []*store.Message
	dupIDs   map[string]bool
}

func (c *captureStore) InsertMessage(_ context.Context, _ string, m *store.Message) error {
	if c.dupIDs[m.ID] {
		return store.ErrDuplicate
	}
	if c.dupIDs == nil {
		c.dupIDs = map[string]bool{}
	}
	c.dupIDs[m.ID] = true
	c.inserted = append(c.inserted, m)
	return nil
}

func TestHandleMODecoded_UsesPublisherID(t *testing.T) {
	cs := &captureStore{}
	s := NewSubscriber(nil, cs, nil)
	payload := []byte(`{"id":"mo-300434060000001-42","imei":"300434060000001","momsn":42,"channel":"iridium","text":"hello"}`)
	s.handleMODecoded("meshsat/300434060000001/mo/decoded", payload)
	s.handleMODecoded("meshsat/300434060000001/mo/decoded", payload) // second replica / redelivery
	if len(cs.inserted) != 1 {
		t.Fatalf("expected 1 insert, got %d", len(cs.inserted))
	}
	if cs.inserted[0].ID != "mo-300434060000001-42" || cs.inserted[0].MOMSN != 42 {
		t.Errorf("publisher id not used: %+v", cs.inserted[0])
	}
}

func TestHandleMODecoded_FallbackIDIsDeterministic(t *testing.T) {
	cs := &captureStore{}
	s := NewSubscriber(nil, cs, nil)
	payload := []byte(`{"imei":"300434060000001","channel":"iridium","text":"no id"}`)
	s.handleMODecoded("meshsat/300434060000001/mo/decoded", payload)
	s.handleMODecoded("meshsat/300434060000001/mo/decoded", payload)
	if len(cs.inserted) != 1 {
		t.Fatalf("expected 1 insert for identical payloads, got %d", len(cs.inserted))
	}
	if cs.inserted[0].ID == "" || cs.inserted[0].ID[:3] != "mo-" {
		t.Errorf("fallback id shape wrong: %q", cs.inserted[0].ID)
	}
	if !errors.Is(cs.InsertMessage(context.Background(), "default", cs.inserted[0]), store.ErrDuplicate) {
		t.Error("capture store should report the duplicate")
	}
}

func (c *captureStore) LookupDeviceTenant(_ context.Context, _ string) (string, error) {
	return "", store.ErrNotFound
}

func (c *captureStore) LookupBridgeTenant(_ context.Context, _ string) (string, error) {
	return "", store.ErrNotFound
}

package dedup

import (
	"testing"
	"time"
)

func TestMemoryDedup_FirstCall(t *testing.T) {
	d := NewMemoryDedup(1 * time.Minute)
	defer d.Close()

	if !d.IsNew("key-1") {
		t.Error("first call should return true")
	}
}

func TestMemoryDedup_Duplicate(t *testing.T) {
	d := NewMemoryDedup(1 * time.Minute)
	defer d.Close()

	d.IsNew("key-1")
	if d.IsNew("key-1") {
		t.Error("second call with same key should return false")
	}
}

func TestMemoryDedup_DifferentKeys(t *testing.T) {
	d := NewMemoryDedup(1 * time.Minute)
	defer d.Close()

	d.IsNew("key-1")
	if !d.IsNew("key-2") {
		t.Error("different key should return true")
	}
}

func TestMemoryDedup_ExpiresAfterTTL(t *testing.T) {
	d := NewMemoryDedup(100 * time.Millisecond)
	defer d.Close()

	d.IsNew("key-1")
	time.Sleep(150 * time.Millisecond)

	if !d.IsNew("key-1") {
		t.Error("key should be new again after TTL expiry")
	}
}

func TestMemoryDedup_Pruner(t *testing.T) {
	d := NewMemoryDedup(100 * time.Millisecond)
	defer d.Close()

	d.IsNew("key-1")
	d.IsNew("key-2")
	d.IsNew("key-3")

	// Wait for TTL + pruner cycle
	time.Sleep(200 * time.Millisecond)

	d.mu.Lock()
	remaining := len(d.seen)
	d.mu.Unlock()

	if remaining > 0 {
		t.Errorf("pruner should have cleaned expired keys, got %d remaining", remaining)
	}
}

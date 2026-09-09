package bridge

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// refuseAll is a tenant that has no room left on its plan.
type refuseAll struct{ asked int }

func (r *refuseAll) AllowAnother(context.Context, string) (bool, string) {
	r.asked++
	return false, "the free plan covers 4 devices and bridges together, and you have 4."
}

// A birth for an unknown device from a tenant at its plan's ceiling does not
// create the device row -- and does nothing else differently. The bridge stays
// online, the health path is untouched, and nothing about the message is
// dropped. A ceiling is a limit on what a tenant may add, never on what the
// Hub will listen to.
func TestDeviceBirth_OverCapDoesNotProvisionButStillProcesses(t *testing.T) {
	ms := newMockStore()
	mb := newMockBus()
	sub := NewSubscriber(mb, ms, nil)
	q := &refuseAll{}
	sub.SetQuota(q)
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}

	birth := protocol.DeviceBirth{
		Protocol:  protocol.ProtocolVersion,
		DeviceID:  "iridium_0",
		BridgeID:  "mule01",
		Type:      "iridium_imt",
		Label:     "RockBLOCK 9704",
		IMEI:      "300258060902280",
		Timestamp: time.Now(),
	}
	payload, _ := json.Marshal(birth)
	mb.deliver("meshsat/bridge/mule01/device/iridium_0/birth", payload)

	if q.asked != 1 {
		t.Errorf("quota asked %d times, want 1", q.asked)
	}
	if ms.createDeviceCalls != 0 {
		t.Errorf("createDevice calls = %d, want 0 for a tenant at its ceiling", ms.createDeviceCalls)
	}
	if _, ok := ms.devices["300258060902280"]; ok {
		t.Error("a device was registered past the tenant's ceiling")
	}
}

// The same birth for a device the tenant already owns never asks about the
// quota at all: it is not a registration, and a tenant that has gone over its
// cap must not stop seeing the kit it already has.
func TestDeviceBirth_ExistingDeviceIsNotAQuotaQuestion(t *testing.T) {
	ms := newMockStore()
	ms.devices["300258060902280"] = &store.Device{IMEI: "300258060902280", Label: "Existing"}
	mb := newMockBus()
	sub := NewSubscriber(mb, ms, nil)
	q := &refuseAll{}
	sub.SetQuota(q)
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}

	birth := protocol.DeviceBirth{
		Protocol:  protocol.ProtocolVersion,
		DeviceID:  "iridium_0",
		BridgeID:  "mule01",
		Type:      "iridium_imt",
		IMEI:      "300258060902280",
		Timestamp: time.Now(),
	}
	payload, _ := json.Marshal(birth)
	mb.deliver("meshsat/bridge/mule01/device/iridium_0/birth", payload)

	if q.asked != 0 {
		t.Errorf("quota asked %d times for a device the tenant already owns, want 0", q.asked)
	}
	if bridgeID := ms.deviceBridgeMap["300258060902280"]; bridgeID != "mule01" {
		t.Errorf("device bridge = %q, want %q -- an over-cap tenant still associates its own kit", bridgeID, "mule01")
	}
}

// TestQuotaCheckOnlyOnDeviceBirth is the companion to
// TestQuotaIsNotOnAnyIngestPath in internal/quota.
//
// This package handles both registration (a birth for a device nobody has
// seen) and ingest (health, telemetry, HeMB symbols, satellite-decoded
// traffic). The subscription ceiling belongs to the first and must never touch
// the second, so this asserts by parsing the file that exactly one function
// asks the question, and which one.
func TestQuotaCheckOnlyOnDeviceBirth(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "subscriber.go", nil, 0)
	if err != nil {
		t.Fatalf("parse subscriber.go: %v", err)
	}

	callers := map[string]int{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "AllowAnother" || sel.Sel.Name == "allowAnother" {
				callers[fn.Name.Name]++
			}
			return true
		})
	}

	// allowAnother is the nil-safe wrapper; it is allowed to call the
	// interface, and handleDeviceBirth is allowed to call the wrapper.
	delete(callers, "allowAnother")

	if len(callers) != 1 || callers["handleDeviceBirth"] != 1 {
		t.Errorf("the quota is asked in %v; it may only be asked once, in handleDeviceBirth.\n"+
			"Everything else in this package carries field traffic, and a subscription "+
			"ceiling on that traffic would let a lapsed plan drop an SOS.", callers)
	}
}

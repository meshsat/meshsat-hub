package integration

import (
	"fmt"
	"net"
	"testing"
	"time"

	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"

	"github.com/meshsat/meshsat-hub/internal/bus/paho"
)

// A subscription made while the broker is DOWN must deliver once it comes up,
// with no restart and no retry loop in the caller.
//
// This is MESHSAT-1129's acceptance criterion, and it is deliberately an
// integration test against a real broker rather than a fake: the property
// depends on Paho's own reconnect machinery calling SetOnConnectHandler, which
// a fake client cannot demonstrate.
//
// What the bug was. Connect() failure at startup is a warning retried in the
// background, so an unconnected bus is an ordinary state. bus.Subscribe used to
// call the broker, fail, and return WITHOUT registering the handler, leaving
// resubscribe nothing to replay. Every call site in main.go was additionally
// wrapped in `if msgBus.IsConnected()`. So a pod that started during a broker
// blip ran with NO MQTT subscriber for its whole life -- no position stored, no
// message stored, no bridge marked online, no SOS detected -- while passing
// /readyz, because the bus probe is informational and never affects readiness.
func TestSubscriptionMadeBeforeTheBrokerExistsStillDelivers(t *testing.T) {
	// A port nothing is listening on yet. Reserving and releasing it is how the
	// existing helper picks a free one; here the gap is the point.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	b := paho.New("tcp://"+addr, "late-broker-test")
	// Connect fails: there is nothing there. Exactly as production logs it, this
	// is a warning and startup continues.
	if err := b.Connect(); err == nil {
		t.Log("connect unexpectedly succeeded; the port was reused, test still valid")
	}

	got := make(chan string, 4)
	// THE CALL UNDER TEST: subscribing to a broker that does not exist yet.
	if err := b.Subscribe("meshsat/dev1/sos", 1, func(topic string, _ []byte) {
		got <- topic
	}); err != nil {
		t.Fatalf("Subscribe while the broker is down must succeed and register: %v", err)
	}

	// Now the broker appears.
	broker := mochi.New(nil)
	_ = broker.AddHook(new(auth.AllowHook), nil)
	tcp := listeners.NewTCP(listeners.Config{ID: "late", Address: addr})
	if err := broker.AddListener(tcp); err != nil {
		t.Fatalf("add listener: %v", err)
	}
	go func() { _ = broker.Serve() }()
	t.Cleanup(func() { _ = broker.Close() })

	// Paho reconnects on its own; give it room, then publish.
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) && !b.IsConnected() {
		time.Sleep(200 * time.Millisecond)
	}
	if !b.IsConnected() {
		t.Fatal("the bus never reconnected to the late broker")
	}

	// Publish until the replayed subscription delivers, or give up. The replay
	// happens in the OnConnect handler, which can land just after IsConnected.
	for i := 0; i < 60; i++ {
		if err := b.Publish("meshsat/dev1/sos", 1, false,
			[]byte(fmt.Sprintf(`{"triggered":true,"n":%d}`, i))); err != nil {
			t.Fatalf("publish: %v", err)
		}
		select {
		case topic := <-got:
			if topic != "meshsat/dev1/sos" {
				t.Fatalf("delivered on the wrong topic: %q", topic)
			}
			return // the subscription made against a dead broker is live
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatal("the subscription registered before the broker existed never delivered. " +
		"A replica that started during a broker blip would consume nothing for the " +
		"life of the process, SOS included.")
}

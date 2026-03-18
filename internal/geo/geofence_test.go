package geo

import (
	"context"
	"sync"
	"testing"
)

// Simple square polygon: (0,0), (0,10), (10,10), (10,0)
var square = []Point{
	{Lat: 0, Lon: 0},
	{Lat: 0, Lon: 10},
	{Lat: 10, Lon: 10},
	{Lat: 10, Lon: 0},
}

func TestPointInPolygon_Inside(t *testing.T) {
	if !PointInPolygon(Point{5, 5}, square) {
		t.Error("expected (5,5) inside square")
	}
}

func TestPointInPolygon_Outside(t *testing.T) {
	if PointInPolygon(Point{15, 15}, square) {
		t.Error("expected (15,15) outside square")
	}
}

func TestPointInPolygon_Edge(t *testing.T) {
	// Points on edges are implementation-dependent; just ensure no crash.
	_ = PointInPolygon(Point{0, 5}, square)
	_ = PointInPolygon(Point{5, 0}, square)
}

func TestPointInPolygon_Triangle(t *testing.T) {
	tri := []Point{{0, 0}, {0, 10}, {10, 5}}
	if !PointInPolygon(Point{3, 5}, tri) {
		t.Error("expected (3,5) inside triangle")
	}
	if PointInPolygon(Point{8, 2}, tri) {
		t.Error("expected (8,2) outside triangle")
	}
}

func TestPointInPolygon_TooFewPoints(t *testing.T) {
	if PointInPolygon(Point{0, 0}, []Point{{0, 0}, {1, 1}}) {
		t.Error("expected false for < 3 vertices")
	}
}

func TestEngine_EnterEvent(t *testing.T) {
	e := NewEngine()
	e.AddFence(Fence{
		ID: "f1", Name: "Base", Polygon: square,
		Trigger: TriggerEnter, Enabled: true,
	})

	var mu sync.Mutex
	var events []FenceEvent
	e.OnEvent(func(_ context.Context, ev FenceEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})

	// Move device inside — should trigger enter.
	result := e.Evaluate(context.Background(), "dev1", 5, 5)
	if len(result) != 1 {
		t.Fatalf("expected 1 enter event, got %d", len(result))
	}
	if result[0].EventType != "enter" {
		t.Errorf("expected enter, got %s", result[0].EventType)
	}

	// Stay inside — no new event.
	result = e.Evaluate(context.Background(), "dev1", 6, 6)
	if len(result) != 0 {
		t.Errorf("expected 0 events while staying inside, got %d", len(result))
	}
}

func TestEngine_ExitEvent(t *testing.T) {
	e := NewEngine()
	e.AddFence(Fence{
		ID: "f1", Name: "Base", Polygon: square,
		Trigger: TriggerExit, Enabled: true,
	})

	// Enter (no event for exit-only trigger).
	result := e.Evaluate(context.Background(), "dev1", 5, 5)
	if len(result) != 0 {
		t.Errorf("expected 0 events for enter with exit-only trigger, got %d", len(result))
	}

	// Exit — should trigger.
	result = e.Evaluate(context.Background(), "dev1", 15, 15)
	if len(result) != 1 || result[0].EventType != "exit" {
		t.Errorf("expected 1 exit event, got %v", result)
	}
}

func TestEngine_BothTrigger(t *testing.T) {
	e := NewEngine()
	e.AddFence(Fence{
		ID: "f1", Name: "Zone", Polygon: square,
		Trigger: TriggerBoth, Enabled: true,
	})

	// Enter.
	result := e.Evaluate(context.Background(), "dev1", 5, 5)
	if len(result) != 1 || result[0].EventType != "enter" {
		t.Fatal("expected enter event")
	}

	// Exit.
	result = e.Evaluate(context.Background(), "dev1", 15, 15)
	if len(result) != 1 || result[0].EventType != "exit" {
		t.Fatal("expected exit event")
	}
}

func TestEngine_DisabledFence(t *testing.T) {
	e := NewEngine()
	e.AddFence(Fence{
		ID: "f1", Name: "Disabled", Polygon: square,
		Trigger: TriggerBoth, Enabled: false,
	})

	result := e.Evaluate(context.Background(), "dev1", 5, 5)
	if len(result) != 0 {
		t.Errorf("expected 0 events for disabled fence, got %d", len(result))
	}
}

func TestEngine_MultipleFences(t *testing.T) {
	e := NewEngine()
	e.AddFence(Fence{
		ID: "f1", Name: "Small", Polygon: []Point{{0, 0}, {0, 5}, {5, 5}, {5, 0}},
		Trigger: TriggerEnter, Enabled: true,
	})
	e.AddFence(Fence{
		ID: "f2", Name: "Large", Polygon: square,
		Trigger: TriggerEnter, Enabled: true,
	})

	// Point (3,3) is inside both.
	result := e.Evaluate(context.Background(), "dev1", 3, 3)
	if len(result) != 2 {
		t.Errorf("expected 2 enter events, got %d", len(result))
	}
}

func TestEngine_RemoveFence(t *testing.T) {
	e := NewEngine()
	e.AddFence(Fence{ID: "f1", Name: "Test", Polygon: square, Trigger: TriggerBoth, Enabled: true})
	e.RemoveFence("f1")
	if len(e.ListFences()) != 0 {
		t.Error("expected 0 fences after remove")
	}
}

func TestEngine_DeviceIsolation(t *testing.T) {
	e := NewEngine()
	e.AddFence(Fence{
		ID: "f1", Name: "Zone", Polygon: square,
		Trigger: TriggerEnter, Enabled: true,
	})

	// dev1 enters.
	r1 := e.Evaluate(context.Background(), "dev1", 5, 5)
	if len(r1) != 1 {
		t.Fatal("expected enter for dev1")
	}

	// dev2 enters separately — should also trigger.
	r2 := e.Evaluate(context.Background(), "dev2", 5, 5)
	if len(r2) != 1 {
		t.Fatal("expected enter for dev2")
	}
}

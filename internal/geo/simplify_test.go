package geo

import (
	"math"
	"testing"
)

func TestSimplify_StraightLine(t *testing.T) {
	// 5 collinear points should simplify to 2.
	points := []Point{
		{0, 0}, {1, 1}, {2, 2}, {3, 3}, {4, 4},
	}
	result := Simplify(points, 0.001)
	if len(result) != 2 {
		t.Errorf("expected 2 points for straight line, got %d", len(result))
	}
}

func TestSimplify_ZigZag(t *testing.T) {
	// Zigzag pattern — should keep all vertices with small epsilon.
	points := []Point{
		{0, 0}, {1, 1}, {0, 2}, {1, 3}, {0, 4},
	}
	result := Simplify(points, 0.001)
	if len(result) != 5 {
		t.Errorf("expected all 5 points preserved for zigzag, got %d", len(result))
	}
}

func TestSimplify_LargeEpsilon(t *testing.T) {
	// Large epsilon should reduce zigzag to just endpoints.
	points := []Point{
		{0, 0}, {0.5, 1}, {0, 2}, {0.5, 3}, {0, 4},
	}
	result := Simplify(points, 10.0)
	if len(result) != 2 {
		t.Errorf("expected 2 points with large epsilon, got %d", len(result))
	}
}

func TestSimplify_TwoPoints(t *testing.T) {
	points := []Point{{0, 0}, {1, 1}}
	result := Simplify(points, 0.001)
	if len(result) != 2 {
		t.Errorf("expected 2 points, got %d", len(result))
	}
}

func TestSimplify_OnePoint(t *testing.T) {
	points := []Point{{0, 0}}
	result := Simplify(points, 0.001)
	if len(result) != 1 {
		t.Errorf("expected 1 point, got %d", len(result))
	}
}

func TestSimplify_Empty(t *testing.T) {
	result := Simplify(nil, 0.001)
	if len(result) != 0 {
		t.Errorf("expected 0 points, got %d", len(result))
	}
}

func TestSimplify_RealTrack(t *testing.T) {
	// Simulate a GPS track with some noise.
	points := make([]Point, 100)
	for i := range points {
		lat := 52.37 + float64(i)*0.001
		lon := 4.89 + float64(i)*0.001
		// Add small noise every 10th point
		if i%10 == 5 {
			lat += 0.0005
		}
		points[i] = Point{Lat: lat, Lon: lon}
	}

	result := Simplify(points, 0.0001)
	// Should significantly reduce points but keep the noisy ones.
	if len(result) >= len(points) {
		t.Errorf("expected simplification, got %d/%d", len(result), len(points))
	}
	if len(result) < 3 {
		t.Errorf("expected at least 3 points, got %d", len(result))
	}
	t.Logf("simplified %d → %d points (%.0f%% reduction)",
		len(points), len(result), 100*(1-float64(len(result))/float64(len(points))))
}

func TestHaversineDistance(t *testing.T) {
	// Amsterdam → Rotterdam ≈ 57 km
	amsterdam := Point{Lat: 52.3676, Lon: 4.9041}
	rotterdam := Point{Lat: 51.9225, Lon: 4.4792}
	d := HaversineDistance(amsterdam, rotterdam)
	if d < 55000 || d > 60000 {
		t.Errorf("Amsterdam→Rotterdam: expected ~57km, got %.0fm", d)
	}
}

func TestHaversineDistance_SamePoint(t *testing.T) {
	p := Point{Lat: 52.37, Lon: 4.89}
	d := HaversineDistance(p, p)
	if d != 0 {
		t.Errorf("expected 0, got %f", d)
	}
}

func TestPerpendicularDistance_OnLine(t *testing.T) {
	// Point exactly on the line should have distance 0.
	a := Point{0, 0}
	b := Point{0, 4}
	p := Point{0, 2}
	d := perpendicularDistance(p, a, b)
	if math.Abs(d) > 1e-10 {
		t.Errorf("expected ~0, got %f", d)
	}
}

func TestPerpendicularDistance_OffLine(t *testing.T) {
	a := Point{0, 0}
	b := Point{0, 4}
	p := Point{1, 2} // 1 degree away from line
	d := perpendicularDistance(p, a, b)
	if math.Abs(d-1.0) > 0.01 {
		t.Errorf("expected ~1.0, got %f", d)
	}
}

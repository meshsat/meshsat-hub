// Package geo provides geographic utilities: Douglas-Peucker track simplification,
// point-in-polygon testing, and distance calculations.
package geo

import "math"

// Point represents a geographic coordinate.
type Point struct {
	Lat float64
	Lon float64
}

// Simplify applies the Douglas-Peucker algorithm to reduce the number of points
// in a track while preserving its shape. Epsilon is the maximum perpendicular
// distance (in degrees) a point can deviate from the simplified line.
// Typical values: 0.0001 (~11m), 0.001 (~111m).
func Simplify(points []Point, epsilon float64) []Point {
	if len(points) <= 2 {
		return points
	}
	return douglasPeucker(points, epsilon)
}

func douglasPeucker(points []Point, epsilon float64) []Point {
	if len(points) <= 2 {
		return points
	}

	// Find the point with the maximum distance from the line (first→last).
	maxDist := 0.0
	maxIdx := 0
	first := points[0]
	last := points[len(points)-1]

	for i := 1; i < len(points)-1; i++ {
		d := perpendicularDistance(points[i], first, last)
		if d > maxDist {
			maxDist = d
			maxIdx = i
		}
	}

	if maxDist > epsilon {
		// Recursively simplify both halves.
		left := douglasPeucker(points[:maxIdx+1], epsilon)
		right := douglasPeucker(points[maxIdx:], epsilon)
		// Combine, avoiding duplicate at the split point.
		result := make([]Point, 0, len(left)+len(right)-1)
		result = append(result, left[:len(left)-1]...)
		result = append(result, right...)
		return result
	}

	// All points within epsilon — keep only endpoints.
	return []Point{first, last}
}

// perpendicularDistance calculates the perpendicular distance from point p
// to the line segment defined by a→b, using the cross-product method.
func perpendicularDistance(p, a, b Point) float64 {
	dx := b.Lon - a.Lon
	dy := b.Lat - a.Lat

	if dx == 0 && dy == 0 {
		// a and b are the same point.
		return math.Sqrt((p.Lat-a.Lat)*(p.Lat-a.Lat) + (p.Lon-a.Lon)*(p.Lon-a.Lon))
	}

	// Cross product magnitude / line length.
	num := math.Abs(dy*(p.Lon-a.Lon) - dx*(p.Lat-a.Lat))
	den := math.Sqrt(dx*dx + dy*dy)
	return num / den
}

// HaversineDistance calculates the great-circle distance in meters between
// two points using the Haversine formula.
func HaversineDistance(a, b Point) float64 {
	const R = 6371000 // Earth radius in meters
	lat1 := a.Lat * math.Pi / 180
	lat2 := b.Lat * math.Pi / 180
	dlat := (b.Lat - a.Lat) * math.Pi / 180
	dlon := (b.Lon - a.Lon) * math.Pi / 180

	h := math.Sin(dlat/2)*math.Sin(dlat/2) +
		math.Cos(lat1)*math.Cos(lat2)*math.Sin(dlon/2)*math.Sin(dlon/2)
	c := 2 * math.Atan2(math.Sqrt(h), math.Sqrt(1-h))
	return R * c
}

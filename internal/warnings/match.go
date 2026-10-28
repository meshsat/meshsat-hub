package warnings

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/meshsat/meshsat-hub/internal/geo"
	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// A tenant's opt-in, as Settings stores it (integrations.ProviderWarnings).
type TenantConfig struct {
	Enabled   bool
	Satellite bool
	// Geocodes are "NAME:VALUE" or bare "VALUE" (any name) area codes.
	Geocodes []string
	// Circles are places the tenant named with a radius.
	Circles []Circle
	// Languages in order of preference, ISO 639-1.
	Languages []string
	// Feeds are the source names taken; empty means all.
	Feeds []string
}

// ParseTenantConfig reads a tenant's warnings account.
func ParseTenantConfig(a *integrations.Account) (*TenantConfig, error) {
	if a == nil {
		return nil, errors.New("no account")
	}
	c := &TenantConfig{
		Enabled:   isTrue(a.Get("enabled")),
		Satellite: isTrue(a.Get("satellite")),
	}
	for _, line := range splitLines(a.Get("areas")) {
		if circle, ok := parseCircleLine(line); ok {
			c.Circles = append(c.Circles, circle)
			continue
		}
		if first, _, ok := strings.Cut(line, ","); ok {
			if _, err := strconv.ParseFloat(strings.TrimSpace(first), 64); err == nil {
				return nil, fmt.Errorf("area %q looks like a circle but is not lat,lon,radius_km", clip(line, 40))
			}
		}
		if !isASCII(line) || len(line) > 64 {
			return nil, fmt.Errorf("area %q is neither a geocode nor lat,lon,radius_km", clip(line, 40))
		}
		c.Geocodes = append(c.Geocodes, strings.ToUpper(line))
	}
	for _, l := range splitList(a.Get("languages")) {
		l = strings.ToLower(l)
		if len(l) == 2 && isASCIILetter(l[0]) && isASCIILetter(l[1]) {
			c.Languages = append(c.Languages, l)
		}
	}
	for _, f := range splitList(a.Get("feeds")) {
		f = strings.ToLower(f)
		if SourceID(f) == 0 {
			return nil, fmt.Errorf("feed %q is not one of dwd, meteoalarm-nl", clip(f, 40))
		}
		c.Feeds = append(c.Feeds, f)
	}
	return c, nil
}

// TakesFeed reports whether the tenant takes a source.
func (c *TenantConfig) TakesFeed(source string) bool {
	if len(c.Feeds) == 0 {
		return true
	}
	for _, f := range c.Feeds {
		if f == source {
			return true
		}
	}
	return false
}

// parseCircleLine reads "lat,lon,radius_km".
func parseCircleLine(line string) (Circle, bool) {
	parts := strings.Split(line, ",")
	if len(parts) != 3 {
		return Circle{}, false
	}
	lat, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	lon, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	r, err3 := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
	if err1 != nil || err2 != nil || err3 != nil || lat < -90 || lat > 90 || lon < -180 || lon > 180 || r < 0 || r > 2000 {
		return Circle{}, false
	}
	return Circle{Centre: geo.Point{Lat: lat, Lon: lon}, RadiusKm: r}, true
}

// Match is what a warning matched for a tenant.
type Match struct {
	// Bridges are the bridge ids the digest goes to, sorted.
	Bridges []string
	// Why says what matched: "geocode", "circle", "bridge".
	Why []string
}

// MatchTenant decides which of a tenant's bridges a warning goes to. A
// geocode or circle the tenant named that lies in the warning's area sends
// it to every bridge of the tenant; a bridge whose last known position lies
// in the warning's polygon or circle gets it on its own. A bridge with no
// position (0, 0) never matches by position.
func MatchTenant(a *Alert, cfg *TenantConfig, bridges []*store.Bridge) Match {
	var m Match
	if a == nil || cfg == nil || !cfg.Enabled {
		return m
	}
	all := false
	codes := a.Geocodes()
	for _, want := range cfg.Geocodes {
		for _, have := range codes {
			name, value, _ := strings.Cut(have, ":")
			if strings.EqualFold(have, want) || (strings.EqualFold(value, want) && !strings.Contains(want, ":")) ||
				(strings.Contains(want, ":") && strings.EqualFold(name+":"+value, want)) {
				all = true
				m.Why = append(m.Why, "geocode "+have)
			}
		}
	}
	for _, c := range cfg.Circles {
		if alertCovers(a, c.Centre, c.RadiusKm) {
			all = true
			m.Why = append(m.Why, fmt.Sprintf("circle %.3f,%.3f", c.Centre.Lat, c.Centre.Lon))
		}
	}
	set := map[string]bool{}
	for _, b := range bridges {
		if b == nil {
			continue
		}
		if all {
			set[b.BridgeID] = true
			continue
		}
		if (b.LocationLat != 0 || b.LocationLon != 0) && alertCovers(a, geo.Point{Lat: b.LocationLat, Lon: b.LocationLon}, 0) {
			set[b.BridgeID] = true
			m.Why = append(m.Why, "bridge "+b.BridgeID)
		}
	}
	for id := range set {
		m.Bridges = append(m.Bridges, id)
	}
	sort.Strings(m.Bridges)
	return m
}

// alertCovers reports whether a point, or a circle of radiusKm around it,
// touches any polygon or circle of any area of the alert. A circle touches a
// polygon when its centre is inside, or any vertex is within the radius.
func alertCovers(a *Alert, p geo.Point, radiusKm float64) bool {
	for _, info := range a.Infos {
		for _, area := range info.Areas {
			for _, poly := range area.Polygons {
				if geo.PointInPolygon(p, poly) {
					return true
				}
				if radiusKm > 0 {
					for _, v := range poly {
						if geo.HaversineDistance(p, v) <= radiusKm*1000 {
							return true
						}
					}
				}
			}
			for _, c := range area.Circles {
				if geo.HaversineDistance(p, c.Centre) <= (c.RadiusKm+radiusKm)*1000 {
					return true
				}
			}
		}
	}
	return false
}

func isTrue(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "true" || s == "1" || s == "yes" || s == "on"
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(strings.TrimRight(l, "\r")); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' || r == ';' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

package tak

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"sync"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/bus"
)

// OTSPoller polls the OpenTAKServer REST API for markers and forwards new/updated
// positions to MQTT so bridges and Android devices receive them. This closes the
// inbound data loop: OTS → Hub → MQTT → Bridge/Android.
//
// The TCP CoT connection to OTS is one-directional (Hub→OTS). OTS does not relay
// events back over plain TCP. The REST API poller provides the reverse path.
type OTSPoller struct {
	baseURL  string
	username string
	password string
	pollSec  int

	mqtt       bus.MessageBus
	httpClient *http.Client

	knownMarkers map[string]otsMarkerState // uid → last seen state
	mu           sync.Mutex
	stopCh       chan struct{}
}

type otsMarkerState struct {
	Lat       float64
	Lon       float64
	Callsign  string
	Type      string
	Timestamp string
}

// otsMarkersResponse is the paginated response from GET /api/markers.
type otsMarkersResponse struct {
	Results []otsMarker `json:"results"`
}

type otsMarker struct {
	UID      string   `json:"uid"`
	Type     string   `json:"type"`
	How      string   `json:"how"`
	Callsign string   `json:"callsign"`
	Stale    string   `json:"stale"`
	Start    string   `json:"start"`
	Point    otsPoint `json:"point"`
}

type otsPoint struct {
	Lat       float64 `json:"latitude"`
	Lon       float64 `json:"longitude"`
	Hae       float64 `json:"hae"`
	Ce        float64 `json:"ce"`
	Le        float64 `json:"le"`
	Callsign  string  `json:"callsign"`
	Timestamp string  `json:"timestamp"`
	Type      string  `json:"type"`
	UID       string  `json:"device_uid"`
}

// NewOTSPoller creates a new OpenTAKServer REST API poller.
func NewOTSPoller(baseURL, username, password string, pollSec int, mqtt bus.MessageBus) *OTSPoller {
	if pollSec <= 0 {
		pollSec = 10
	}
	jar, _ := cookiejar.New(nil)
	return &OTSPoller{
		baseURL:      baseURL,
		username:     username,
		password:     password,
		pollSec:      pollSec,
		mqtt:         mqtt,
		httpClient:   &http.Client{Jar: jar, Timeout: 10 * time.Second},
		knownMarkers: make(map[string]otsMarkerState),
		stopCh:       make(chan struct{}),
	}
}

// Start begins the polling loop in a background goroutine.
func (p *OTSPoller) Start() {
	go p.pollLoop()
	slog.Info("tak: OTS API poller started", "url", p.baseURL, "poll_sec", p.pollSec)
}

// Stop signals the polling loop to exit.
func (p *OTSPoller) Stop() {
	close(p.stopCh)
}

func (p *OTSPoller) pollLoop() {
	// Initial login.
	if err := p.login(); err != nil {
		slog.Warn("tak: OTS API login failed", "error", err)
	}

	ticker := time.NewTicker(time.Duration(p.pollSec) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			if err := p.poll(); err != nil {
				slog.Debug("tak: OTS poll failed, re-login", "error", err)
				if loginErr := p.login(); loginErr != nil {
					slog.Warn("tak: OTS re-login failed", "error", loginErr)
				}
			}
		}
	}
}

func (p *OTSPoller) login() error {
	resp, err := p.httpClient.PostForm(p.baseURL+"/api/login", map[string][]string{
		"username": {p.username},
		"password": {p.password},
	})
	if err != nil {
		return fmt.Errorf("login request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusFound {
		return fmt.Errorf("login status: %d", resp.StatusCode)
	}
	slog.Debug("tak: OTS API logged in")
	return nil
}

func (p *OTSPoller) poll() error {
	resp, err := p.httpClient.Get(p.baseURL + "/api/markers?per_page=100")
	if err != nil {
		return fmt.Errorf("get markers: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("auth expired (status %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("markers status: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	var result otsMarkersResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parse markers: %w", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	for _, m := range result.Results {
		// Skip markers that we ourselves sent (bridge births, device births).
		if isHubMarker(m.UID) {
			continue
		}

		// Check if marker is new or changed.
		prev, known := p.knownMarkers[m.UID]
		state := otsMarkerState{
			Lat:       m.Point.Lat,
			Lon:       m.Point.Lon,
			Callsign:  m.Callsign,
			Type:      m.Type,
			Timestamp: m.Point.Timestamp,
		}

		if known && prev == state {
			continue // unchanged
		}

		p.knownMarkers[m.UID] = state

		// Skip null-island positions.
		if m.Point.Lat == 0 && m.Point.Lon == 0 {
			continue
		}

		// Build CoT XML and broadcast to bridges.
		ev := p.markerToCotEvent(m)
		cotXML, err := MarshalCotEvent(ev)
		if err != nil {
			slog.Warn("tak: marshal OTS marker", "error", err, "uid", m.UID)
			continue
		}

		broadcastTopic := "meshsat/broadcast/tak/cot/in"
		if err := p.mqtt.Publish(broadcastTopic, 1, false, cotXML); err != nil {
			slog.Warn("tak: publish OTS marker", "error", err, "uid", m.UID)
			continue
		}

		action := "new"
		if known {
			action = "updated"
		}
		slog.Info("tak: OTS marker forwarded", "uid", m.UID, "callsign", m.Callsign,
			"lat", m.Point.Lat, "lon", m.Point.Lon, "action", action)
	}

	return nil
}

func (p *OTSPoller) markerToCotEvent(m otsMarker) CotEvent {
	now := time.Now().UTC()
	staleSec := 600

	cotType := m.Type
	if cotType == "" {
		cotType = TypePosition
	}

	callsign := m.Callsign
	if callsign == "" {
		callsign = m.UID
	}

	how := m.How
	if how == "" {
		how = "m-g"
	}

	return CotEvent{
		Version: "2.0",
		UID:     m.UID,
		Type:    cotType,
		How:     how,
		Time:    now.Format(cotTimeFormat),
		Start:   now.Format(cotTimeFormat),
		Stale:   now.Add(time.Duration(staleSec) * time.Second).Format(cotTimeFormat),
		Point:   CotPoint{Lat: m.Point.Lat, Lon: m.Point.Lon, Hae: m.Point.Hae, Ce: m.Point.Ce, Le: m.Point.Le},
		Detail: &CotDetail{
			Contact: &CotContact{Callsign: callsign},
			Remarks: &CotRemarks{Source: "OTS", Text: "Via OpenTAKServer"},
		},
	}
}

// isHubMarker returns true if the UID belongs to a marker the Hub itself sent
// (bridge births, device births, device positions forwarded by bridges).
func isHubMarker(uid string) bool {
	for _, prefix := range []string{"meshsat-bridge-", "meshsat-device-"} {
		if len(uid) > len(prefix) && uid[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

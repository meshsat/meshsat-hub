package cloudloop

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ThingInfo holds cached metadata about a Cloudloop thing.
type ThingInfo struct {
	ThingID string
	IsIMT   bool
}

// CloudloopThing represents a thing returned by the Data/GetThings API.
type CloudloopThing struct {
	ID                 string          `json:"id"`
	Account            string          `json:"account"`
	SupportsSBD        bool            `json:"supportsSbd"`
	SupportsP6         bool            `json:"supportsP6"`     // P6 = Certus/IMT
	SupportsCeefax     bool            `json:"supportsCeefax"` // Ceefax = IoT
	SupportsIOT        bool            `json:"supportsIot"`
	SubscriberSBD      json.RawMessage `json:"subscriberSbd,omitempty"`
	SubscriberCertus   json.RawMessage `json:"subscriberCertus,omitempty"`
	SubscriberBGAN     json.RawMessage `json:"subscriberBgan,omitempty"`
	SubscriberCellular json.RawMessage `json:"subscriberCellular,omitempty"`
}

// subscRef parses a subscriber reference that the live API returns either as
// a bare ID string or (legacy) as an object with id and imei. [MESHSAT-750]
func subscRef(raw json.RawMessage) (id, imei string) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", ""
	}
	if raw[0] == '"' {
		_ = json.Unmarshal(raw, &id)
		return id, ""
	}
	var ref CloudloopSubscRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		return "", ""
	}
	return ref.ID, ref.IMEI
}

// CloudloopSubscRef is a reference to a subscriber record within a thing.
type CloudloopSubscRef struct {
	ID   string `json:"id"`
	IMEI string `json:"imei,omitempty"`
}

// UnmarshalJSON tolerates Cloudloop's GetThings schema drift (IFRNLLEI01PRD-906,
// observed 2026-05-15): the subscriber-ref fields (subscriberCertus, and
// potentially subscriberSbd/Bgan/Cellular) may arrive as a plain JSON string
// instead of an object. Before this, the fixed `*CloudloopSubscRef` decode failed
// the entire GetThings unmarshal once per minute, breaking periodic API refresh.
// Accept both shapes: a bare string is taken as the subscriber ID; an object
// decodes normally. null leaves the ref zero-valued.
func (r *CloudloopSubscRef) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	// String form (post-drift): the value IS the subscriber id.
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		r.ID = s
		return nil
	}
	// Object form (original schema). Alias breaks the UnmarshalJSON recursion.
	type alias CloudloopSubscRef
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*r = CloudloopSubscRef(a)
	return nil
}

// GetThingsResponse is the response from Data/GetThings.
type GetThingsResponse struct {
	Things []CloudloopThing `json:"things"`
}

// ThingResolver implements DeviceResolver by caching IMEI-to-thingID mappings
// learned from incoming LingoMO messages and optionally from the Cloudloop API.
type ThingResolver struct {
	mu     sync.RWMutex
	cache  map[string]ThingInfo // IMEI -> ThingInfo
	client *Client              // for API lookups (optional, nil = learn-only mode)
}

// NewThingResolver creates a new resolver. If client is non-nil, RefreshFromAPI
// can be called to pre-populate the cache from the Cloudloop Data API.
func NewThingResolver(client *Client) *ThingResolver {
	return &ThingResolver{
		cache:  make(map[string]ThingInfo),
		client: client,
	}
}

// Resolve returns the Cloudloop thingID and whether the device uses IMT protocol.
// If the device is unknown, thingID is the IMEI itself and isIMT is false.
func (r *ThingResolver) Resolve(imei string) (thingID string, isIMT bool) {
	r.mu.RLock()
	info, ok := r.cache[imei]
	r.mu.RUnlock()

	if ok {
		return info.ThingID, info.IsIMT
	}
	return imei, false
}

// Register manually adds or updates an IMEI-to-thingID mapping.
func (r *ThingResolver) Register(imei, thingID string, isIMT bool) {
	r.mu.Lock()
	r.cache[imei] = ThingInfo{ThingID: thingID, IsIMT: isIMT}
	r.mu.Unlock()

	slog.Debug("resolver: registered device",
		"imei", imei, "thing_id", thingID, "imt", isIMT)
}

// LearnFromMO extracts thingId and hardware type from an incoming LingoMO
// message and caches the IMEI-to-thingId mapping. This is the primary way
// the resolver learns about devices -- every MO received teaches it.
func (r *ThingResolver) LearnFromMO(mo *LingoMO) {
	if mo == nil {
		return
	}

	imei := mo.ExtractIMEI()
	thingID := mo.Identity.ThingID
	if imei == "" || thingID == "" {
		return
	}

	// Determine if this is an IMT device from the hardware type or MO payload type.
	isIMT := r.detectIMT(mo)

	r.mu.Lock()
	prev, existed := r.cache[imei]
	r.cache[imei] = ThingInfo{ThingID: thingID, IsIMT: isIMT}
	r.mu.Unlock()

	if !existed || prev.ThingID != thingID || prev.IsIMT != isIMT {
		slog.Info("resolver: learned device from MO",
			"imei", imei, "thing_id", thingID, "imt", isIMT,
			"source", mo.Source())
	}
}

// detectIMT determines whether a LingoMO originated from an IMT/Certus device.
func (r *ThingResolver) detectIMT(mo *LingoMO) bool {
	// IMT field present means this was an IMT message.
	if mo.IMT != nil {
		return true
	}

	// Check hardware type from identity.
	if mo.Identity.Hardware != nil {
		hwType := strings.ToUpper(mo.Identity.Hardware.Type)
		if strings.Contains(hwType, "CERTUS") || strings.Contains(hwType, "IMT") ||
			strings.Contains(hwType, "9704") {
			return true
		}
	}

	// Check subscriber type from identity.
	if mo.Identity.Subscriber != nil {
		subType := strings.ToUpper(mo.Identity.Subscriber.Type)
		if strings.Contains(subType, "CERTUS") || strings.Contains(subType, "IMT") ||
			strings.Contains(subType, "P6") {
			return true
		}
	}

	return false
}

// Count returns the number of cached IMEI-to-thingID mappings.
func (r *ThingResolver) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.cache)
}

// ListThings calls the Cloudloop Data/GetThings API and returns all things
// in the account. Requires a valid API key configured on the client.
func (c *Client) ListThings(ctx context.Context) ([]CloudloopThing, error) {
	params := url.Values{
		"token": {c.apiKey},
	}

	apiURL := fmt.Sprintf("%s/Data/GetThings?%s", c.apiURL, params.Encode())
	slog.Debug("cloudloop: listing things")

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cloudloop: create GetThings request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cloudloop: GetThings: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("cloudloop: read GetThings response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("cloudloop: GetThings HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result GetThingsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("cloudloop: parse GetThings response: %w", err)
	}

	slog.Info("cloudloop: GetThings returned", "count", len(result.Things))
	return result.Things, nil
}

// RefreshFromAPI fetches all things from the Cloudloop Data API and updates
// the cache. Things with SupportsP6 (Certus/IMT) are marked as IMT.
// This does not remove existing cache entries learned from MO messages,
// since the API may not include IMEI in the thing record.
func (r *ThingResolver) RefreshFromAPI(ctx context.Context) error {
	if r.client == nil {
		return fmt.Errorf("resolver: no client configured for API refresh")
	}

	things, err := r.client.ListThings(ctx)
	if err != nil {
		return fmt.Errorf("resolver: API refresh: %w", err)
	}

	added := 0
	for _, t := range things {
		// A Certus subscriber on the thing means IMT regardless of the
		// supportsP6 flag, which the live API reports false even for
		// active Certus things. [MESHSAT-750]
		certusID, certusIMEI := subscRef(t.SubscriberCertus)
		_, sbdIMEI := subscRef(t.SubscriberSBD)
		isIMT := t.SupportsP6 || certusID != ""

		// Try to extract IMEI from subscriber references. The live API
		// returns bare subscriber IDs with no IMEI, so this usually
		// yields nothing and mappings come from MO learning or the
		// HUB_CLOUDLOOP_DEVICE_MAP seed.
		imei := sbdIMEI
		if certusIMEI != "" {
			imei = certusIMEI
			isIMT = true
		}

		if imei == "" {
			// No IMEI available from API -- will be learned from MO messages.
			continue
		}

		r.mu.Lock()
		if _, exists := r.cache[imei]; !exists {
			r.cache[imei] = ThingInfo{ThingID: t.ID, IsIMT: isIMT}
			added++
		}
		r.mu.Unlock()
	}

	slog.Info("resolver: API refresh complete",
		"things_total", len(things), "new_mappings", added, "cache_size", r.Count())
	return nil
}

// SeedFromSpec registers device mappings from a config string of the form
// "imei:thingID:imt,imei:thingID:sbd". Returns the number of entries
// registered. Used with HUB_CLOUDLOOP_DEVICE_MAP so IMT devices route
// correctly from first boot, since the Data API exposes no IMEIs and MO
// learning is in-memory only. [MESHSAT-750]
func (r *ThingResolver) SeedFromSpec(spec string) int {
	n := 0
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) < 2 {
			slog.Warn("resolver: bad HUB_CLOUDLOOP_DEVICE_MAP entry", "entry", entry)
			continue
		}
		isIMT := len(parts) >= 3 && strings.EqualFold(parts[2], "imt")
		r.Register(parts[0], parts[1], isIMT)
		n++
	}
	return n
}

// StartPeriodicRefresh runs RefreshFromAPI at the given interval.
// Blocks until ctx is cancelled. Intended to run as a goroutine.
func (r *ThingResolver) StartPeriodicRefresh(ctx context.Context, interval time.Duration) {
	// Initial refresh on startup.
	if err := r.RefreshFromAPI(ctx); err != nil {
		slog.Warn("resolver: initial API refresh failed", "error", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.RefreshFromAPI(ctx); err != nil {
				slog.Warn("resolver: periodic API refresh failed", "error", err)
			}
		}
	}
}

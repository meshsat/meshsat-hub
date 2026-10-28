package warnings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
)

var (
	fetchedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_warnings_fetched_total",
		Help: "Warning feed polls and the messages they carried, by source and result (MESHSAT-1734).",
	}, []string{"source", "result"})
	matchedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_warnings_matched_total",
		Help: "Warnings that matched a tenant's areas or kits, by source (MESHSAT-1734).",
	}, []string{"source"})
	deliveredTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_warnings_delivered_total",
		Help: "Warning digests handed to kits, by bearer and result (MESHSAT-1734).",
	}, []string{"bearer", "result"})
)

func init() {
	for _, s := range []string{"dwd", "meteoalarm-nl", "test"} {
		for _, r := range []string{"poll", "failed", "message", "new"} {
			fetchedTotal.WithLabelValues(s, r)
		}
		matchedTotal.WithLabelValues(s)
	}
	for _, b := range []string{"mqtt", "satellite"} {
		for _, r := range []string{"sent", "failed", "claimed_elsewhere", "no_modem"} {
			deliveredTotal.WithLabelValues(b, r)
		}
	}
}

// Accounts is what the service needs of internal/integrations.
type Accounts interface {
	TenantsWith(ctx context.Context, provider string) ([]string, error)
	ForTenant(ctx context.Context, tenantID, provider string) (*integrations.Account, error)
}

// Bus is what the service needs of the message bus.
type Bus interface {
	Publish(topic string, qos byte, retained bool, payload []byte) error
}

// SatSender sends one digest as an MT to one modem on the tenant's own
// satellite account. main.go builds it from the Cloudloop and Rock7 senders.
type SatSender func(ctx context.Context, tenantID, imei string, wire []byte) error

// Service polls the feeds, matches and delivers. One Service per replica;
// Run is a leader singleton, Inject runs on whichever replica the request
// reached. Both deliver through ClaimOnce, so a message reaches a kit once.
type Service struct {
	store    store.Store
	bus      Bus
	accounts Accounts
	signer   Signer
	sat      SatSender
	feeds    []Feed
	interval time.Duration
	log      *slog.Logger
	now      func() time.Time
}

// New returns a Service. feeds may be empty (then Run only waits).
func New(s store.Store, b Bus, accounts Accounts, signer Signer, sat SatSender, feeds []Feed, interval time.Duration, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	if interval < time.Minute {
		interval = 5 * time.Minute
	}
	return &Service{store: s, bus: b, accounts: accounts, signer: signer, sat: sat, feeds: feeds, interval: interval, log: log, now: time.Now}
}

// PublicKey is the key the kits verify with, PKIX DER; nil without a signer.
func (s *Service) PublicKey() []byte {
	if s == nil || s.signer == nil {
		return nil
	}
	return s.signer.PublicKey()
}

// Run polls the feeds until ctx ends. A leader singleton: it is started on
// every acquisition and must build its own ticker.
func (s *Service) Run(ctx context.Context) {
	s.Poll(ctx)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Poll(ctx)
		}
	}
}

// Poll reads every feed once and processes what is new.
func (s *Service) Poll(ctx context.Context) {
	for _, f := range s.feeds {
		source := SourceName(f.Source())
		fetchedTotal.WithLabelValues(source, "poll").Inc()
		known := func(identifier string, sent time.Time) bool {
			w, err := s.store.GetWarning(ctx, source, identifier)
			return err == nil && w != nil && (sent.IsZero() || w.Sent.Equal(sent))
		}
		msgs, err := f.Fetch(ctx, known)
		if err != nil {
			fetchedTotal.WithLabelValues(source, "failed").Inc()
			s.log.Warn("warnings: a feed could not be read", "source", source, "error", err)
			if ctx.Err() != nil {
				return
			}
			continue
		}
		for _, m := range msgs {
			fetchedTotal.WithLabelValues(source, "message").Inc()
			w := toWarning(source, m.Alert, m.Raw)
			changed, err := s.store.UpsertWarning(ctx, w)
			if err != nil {
				s.log.Warn("warnings: a message could not be stored", "source", source, "identifier", w.Identifier, "error", err)
				continue
			}
			if !changed {
				continue
			}
			fetchedTotal.WithLabelValues(source, "new").Inc()
			s.log.Info("warnings: new message", "source", source, "identifier", w.Identifier, "type", w.MsgType,
				"event", w.Event, "severity", w.Severity, "expires", w.Expires)
			s.process(ctx, m.Alert, w, "")
		}
	}
}

// toWarning is the stored form of a CAP message.
func toWarning(source string, a *Alert, raw []byte) *store.Warning {
	sum := sha256.Sum256(raw)
	w := &store.Warning{Source: source, Identifier: a.Identifier, Sender: a.Sender, MsgType: a.MsgType, Status: a.Status,
		Sent: a.Sent, Expires: a.Expires(), RawHash: hex.EncodeToString(sum[:])}
	var refs []string
	for _, r := range a.References {
		refs = append(refs, r.Sender+","+r.Identifier+","+r.Sent)
	}
	w.References = strings.Join(refs, " ")
	type areaRow struct {
		Desc     string   `json:"desc"`
		Geocodes []string `json:"geocodes,omitempty"`
		Polygons int      `json:"polygons,omitempty"`
		Circles  int      `json:"circles,omitempty"`
	}
	rows := []areaRow{}
	if info := a.InfoFor(nil); info != nil {
		w.SenderName, w.Language, w.Event, w.Headline = info.SenderName, info.Language, info.Event, info.Headline
		w.Severity, w.Urgency, w.Certainty = info.Severity, info.Urgency, info.Certainty
		w.Effective = info.Effective
		if len(info.Category) > 0 {
			w.Category = info.Category[0]
		}
		w.Text = strings.TrimSpace(info.Description)
		if info.Instruction != "" {
			w.Text = strings.TrimSpace(w.Text + "\n" + info.Instruction)
		}
		for _, ar := range info.Areas {
			row := areaRow{Desc: ar.Desc, Polygons: len(ar.Polygons), Circles: len(ar.Circles)}
			for _, g := range ar.Geocodes {
				row.Geocodes = append(row.Geocodes, g.Name+":"+g.Value)
			}
			rows = append(rows, row)
		}
	}
	b, _ := json.Marshal(rows)
	w.Areas = string(b)
	return w
}

// Delivery is what one tenant's delivery of a warning came to.
type Delivery struct {
	TenantID string              `json:"tenant_id"`
	Bridges  []string            `json:"bridges"`
	Why      []string            `json:"why,omitempty"`
	Bearers  map[string][]string `json:"bearers"` // bridge id -> bearers the digest went on
	// Skipped names the tenant's bridges no digest can go to and why: a
	// phone, a bridge without a birth, one with no mesh, APRS or 2G cell.
	Skipped []string `json:"skipped,omitempty"`
	Digest  string   `json:"digest_hex,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// process matches a stored message to every tenant with the relay on (or to
// onlyTenant) and delivers to the matching bridges.
func (s *Service) process(ctx context.Context, a *Alert, w *store.Warning, onlyTenant string) []Delivery {
	var tenants []string
	if onlyTenant != "" {
		tenants = []string{onlyTenant}
	} else {
		var err error
		if tenants, err = s.accounts.TenantsWith(ctx, integrations.ProviderWarnings); err != nil {
			s.log.Warn("warnings: listing the tenants with the relay on failed", "error", err)
			return nil
		}
	}
	var out []Delivery
	for _, tid := range tenants {
		acct, err := s.accounts.ForTenant(ctx, tid, integrations.ProviderWarnings)
		if err != nil || acct == nil || acct.Platform {
			continue
		}
		cfg, err := ParseTenantConfig(acct)
		if err != nil {
			s.log.Warn("warnings: a tenant's relay settings cannot be used", "tenant", tid, "error", err)
			if onlyTenant != "" {
				out = append(out, Delivery{TenantID: tid, Error: err.Error()})
			}
			continue
		}
		if !cfg.Enabled {
			if onlyTenant != "" {
				out = append(out, Delivery{TenantID: tid, Error: "the relay is not enabled for this workspace"})
			}
			continue
		}
		if !cfg.TakesFeed(w.Source) && w.Source != SourceName(SourceTest) {
			continue
		}
		bridges, err := s.store.ListBridges(ctx, tid)
		if err != nil {
			s.log.Warn("warnings: listing a tenant's bridges failed", "tenant", tid, "error", err)
			continue
		}
		// Kits only: a phone or a bridge without a birth is never handed a
		// digest (CanRelay), whatever area it is in.
		kits, skipped := relayCapable(bridges)
		m := MatchTenant(a, cfg, kits)
		if len(m.Bridges) == 0 {
			if onlyTenant != "" {
				why := "no kit of this workspace is in the warning's area"
				if len(kits) == 0 && len(bridges) > 0 {
					why = "no kit of this workspace can relay a warning: " + strings.Join(skipped, "; ")
				}
				out = append(out, Delivery{TenantID: tid, Why: m.Why, Skipped: skipped, Error: why})
			}
			continue
		}
		matchedTotal.WithLabelValues(w.Source).Inc()
		d := s.deliver(ctx, a, w, tid, cfg, m)
		d.Skipped = skipped
		out = append(out, d)
	}
	return out
}

// deliver signs the digest once per tenant (its language choice) and hands
// it to each matching bridge, once across the replicas.
func (s *Service) deliver(ctx context.Context, a *Alert, w *store.Warning, tid string, cfg *TenantConfig, m Match) Delivery {
	d := Delivery{TenantID: tid, Bridges: m.Bridges, Why: m.Why, Bearers: map[string][]string{}}
	digest := FromAlert(a, SourceID(w.Source), cfg.Languages)
	wire, err := Encode(digest, s.signer)
	if err != nil {
		d.Error = "digest: " + err.Error()
		s.log.Warn("warnings: a digest could not be made", "tenant", tid, "identifier", w.Identifier, "error", err)
		return d
	}
	d.Digest = hex.EncodeToString(wire)
	msgType := strings.ToLower(w.MsgType)
	for _, bridgeID := range m.Bridges {
		key := "warn:" + tid + ":" + bridgeID + ":" + w.Source + "/" + w.Identifier + ":" + msgType
		switch ok, err := s.store.ClaimOnce(ctx, key); {
		case err != nil:
			deliveredTotal.WithLabelValues("mqtt", "failed").Inc()
			s.log.Warn("warnings: the delivery claim failed, not sent", "tenant", tid, "bridge", bridgeID, "error", err)
		case !ok:
			deliveredTotal.WithLabelValues("mqtt", "claimed_elsewhere").Inc()
		default:
			if err := s.bus.Publish(hubmqtt.TopicWarnInFor(tid, bridgeID), 1, false, wire); err != nil {
				deliveredTotal.WithLabelValues("mqtt", "failed").Inc()
				s.log.Warn("warnings: a digest could not be published", "tenant", tid, "bridge", bridgeID, "error", err)
			} else {
				deliveredTotal.WithLabelValues("mqtt", "sent").Inc()
				d.Bearers[bridgeID] = append(d.Bearers[bridgeID], "mqtt")
				s.log.Info("warnings: digest handed to a kit", "tenant", tid, "bridge", bridgeID, "source", w.Source,
					"identifier", w.Identifier, "type", w.MsgType, "bytes", len(wire))
			}
		}
		if !cfg.Satellite || s.sat == nil {
			continue
		}
		imei := s.modemOf(ctx, tid, bridgeID)
		if imei == "" {
			deliveredTotal.WithLabelValues("satellite", "no_modem").Inc()
			continue
		}
		skey := "warn:sat:" + tid + ":" + imei + ":" + w.Source + "/" + w.Identifier + ":" + msgType
		switch ok, err := s.store.ClaimOnce(ctx, skey); {
		case err != nil:
			deliveredTotal.WithLabelValues("satellite", "failed").Inc()
			s.log.Warn("warnings: the satellite claim failed, not sent", "tenant", tid, "bridge", bridgeID, "error", err)
		case !ok:
			deliveredTotal.WithLabelValues("satellite", "claimed_elsewhere").Inc()
		default:
			if err := s.sat(ctx, tid, imei, wire); err != nil {
				deliveredTotal.WithLabelValues("satellite", "failed").Inc()
				s.log.Warn("warnings: a digest could not be sent over satellite", "tenant", tid, "bridge", bridgeID, "error", err)
			} else {
				deliveredTotal.WithLabelValues("satellite", "sent").Inc()
				d.Bearers[bridgeID] = append(d.Bearers[bridgeID], "satellite")
				s.log.Info("warnings: digest sent over satellite", "tenant", tid, "bridge", bridgeID, "source", w.Source,
					"identifier", w.Identifier, "type", w.MsgType, "bytes", len(wire))
			}
		}
	}
	bridges, _ := json.Marshal(m.Bridges)
	detail, _ := json.Marshal(map[string]any{"bearers": d.Bearers, "why": m.Why})
	if err := s.store.UpsertWarningMatch(ctx, &store.WarningMatch{TenantID: tid, Source: w.Source, Identifier: w.Identifier,
		MsgType: w.MsgType, Test: a.IsTest(), Bridges: string(bridges), Detail: string(detail), MatchedAt: s.now()}); err != nil {
		s.log.Warn("warnings: the match could not be recorded", "tenant", tid, "error", err)
	}
	return d
}

// modemOf picks the modem a bridge's satellite digest goes to: the device of
// the tenant linked to that bridge that reported last. "" when none is.
func (s *Service) modemOf(ctx context.Context, tid, bridgeID string) string {
	devices, err := s.store.ListDevices(ctx, tid)
	if err != nil {
		return ""
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].LastSeen.After(devices[j].LastSeen) })
	for _, dev := range devices {
		owner, err := s.store.DeviceBridgeID(ctx, tid, dev.IMEI)
		if err == nil && owner == bridgeID {
			return dev.IMEI
		}
	}
	return ""
}

// Refusals of a test injection.
var (
	ErrNotTest = errors.New("warnings: a test injection must carry status Test or Exercise")
)

// InjectResult is what a test injection came to.
type InjectResult struct {
	Identifier string     `json:"identifier"`
	MsgType    string     `json:"msg_type"`
	Event      string     `json:"event"`
	Deliveries []Delivery `json:"deliveries"`
}

// Inject runs one CAP document through the parse, match, sign and deliver
// path for one tenant alone, as a test: the document must say so in its
// status, the kits mark it as a test on every bearer, and the match is
// recorded under source "test".
func (s *Service) Inject(ctx context.Context, tenantID string, capXML []byte) (*InjectResult, error) {
	a, err := ParseCAP(capXML)
	if err != nil {
		return nil, err
	}
	if !a.IsTest() {
		return nil, ErrNotTest
	}
	w := toWarning(SourceName(SourceTest), a, capXML)
	if _, err := s.store.UpsertWarning(ctx, w); err != nil {
		return nil, fmt.Errorf("warnings: store: %w", err)
	}
	res := &InjectResult{Identifier: a.Identifier, MsgType: a.MsgType, Event: w.Event}
	res.Deliveries = s.process(ctx, a, w, tenantID)
	return res, nil
}

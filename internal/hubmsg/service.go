package hubmsg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
)

var (
	submissionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_msg_submissions_total",
		Help: "hub_msg submissions by result and refusal reason.",
	}, []string{"result", "reason"})
	deliveriesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_msg_deliveries_total",
		Help: "hub_msg publications on a recipient's msg/in, by result.",
	}, []string{"result"})
	acksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_msg_acks_total",
		Help: "hub_msg recipient answers by evidence kind, or ignored.",
	}, []string{"kind"})
)

// Publisher is what the service needs from the bus: a QoS 1 publish that returns once the
// broker acknowledged it.
type Publisher interface {
	Publish(topic string, qos byte, retained bool, payload []byte) error
}

// SkewAllowance and the retention after a deadline are those of the receiver inbox (C3): a late
// repeat of an operation is answered from the stored row for a day after its deadline.
const (
	SkewAllowance = 120 * time.Second
	KeepAfter     = 24 * time.Hour
)

// DuplicateWindow is how soon after its admission a repeat of an operation is taken for the
// other replica's copy of the same frame rather than the sender asking again.
const DuplicateWindow = 10 * time.Second

// handlerTimeout bounds the store work of one bus message, so a stuck database cannot hold
// the bus client's callback. A message not decided in time is decided on the sender's repeat.
const handlerTimeout = 10 * time.Second

// Service is the Hub's side of hub_msg. HandleOut and HandleAck run on every replica (the bus
// gives each replica every message; the store's transactions and the deterministic event ids
// make that safe); Run is the outbox drainer and runs on the leader only.
type Service struct {
	st  store.Store
	bus Publisher
	now func() time.Time
	// Lease is how long a drainer holds an operation while it publishes it.
	Lease time.Duration
}

// NewService wires the store and the bus.
func NewService(st store.Store, bus Publisher) *Service {
	return &Service{st: st, bus: bus, now: time.Now, Lease: 30 * time.Second}
}

// Backoff is the wait after the n-th publication of an operation on the recipient's msg/in
// that the recipient has not answered yet: 15 s, 30 s, 60 s, 120 s, then 5 min.
func Backoff(n int) time.Duration {
	d := 15 * time.Second
	for i := 1; i < n && d < 5*time.Minute; i++ {
		d *= 2
	}
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

func principal(bridgeID string) string { return "bridge:" + bridgeID }

// bridgeExists reports whether bridgeID is a bridge of tenant. Both stores answer a missing
// row with sql.ErrNoRows from GetBridge, not store.ErrNotFound; either is "no", anything else
// is an error that decides nothing.
func (s *Service) bridgeExists(ctx context.Context, tenant, bridgeID string) (bool, error) {
	_, err := s.st.GetBridge(ctx, tenant, bridgeID)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, store.ErrNotFound):
		return false, nil
	}
	return false, err
}

// HandleOut takes one submission from a bridge's msg/out (C4).
func (s *Service) HandleOut(topic string, payload []byte) {
	tenant, bridgeID, kind, ok := hubmqtt.ParseMsgTopic(topic)
	if !ok || kind != "out" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	sender := principal(bridgeID)
	var sub Submission
	if err := json.Unmarshal(payload, &sub); err != nil {
		s.refuse(tenant, bridgeID, Submission{}, "bad_json")
		return
	}
	known := func(recipient string) (bool, error) { return s.bridgeExists(ctx, tenant, BridgeOf(recipient)) }
	v, op, err := Submit(ctx, s.st, tenant, sender, sub, s.now(), known)
	if err != nil {
		// Nothing was decided; the sender's next repeat is decided then.
		slog.Error("hub_msg: submission not decided", "tenant", tenant, "sender", sender, "op_id", sub.OpID, "error", err)
		submissionsTotal.WithLabelValues("error", "").Inc()
		return
	}
	if v.Result == Replay && op != nil && s.now().Sub(op.CreatedAt) < DuplicateWindow {
		// Every replica takes every frame from the bus, so the other replica's copy of a
		// submission admitted a moment ago reads as a repeat. It is the same frame, not the
		// sender asking again: nothing is replayed (the events go out once, as they are made).
		submissionsTotal.WithLabelValues("duplicate", "").Inc()
		return
	}
	submissionsTotal.WithLabelValues(v.Result, v.Reason).Inc()
	switch v.Result {
	case Admitted:
		slog.Info("hub_msg: operation admitted", "tenant", tenant, "sender", sender, "op_id", sub.OpID, "to", sub.To)
	case Replay:
		// Every event again, not only the last: the sender may have lost any of them (C4).
		if _, err := s.st.ReplayMsgOpEvents(ctx, tenant, sender, sub.OpID); err != nil {
			slog.Warn("hub_msg: replay not marked", "op_id", sub.OpID, "error", err)
		}
	case Refused:
		s.refuse(tenant, bridgeID, sub, v.Reason)
	}
}

func (s *Service) refuse(tenant, bridgeID string, sub Submission, reason string) {
	data, _ := json.Marshal(Refusal{V: 1, Type: "refusal", OpID: sub.OpID, Mid: sub.Mid, Reason: reason})
	if err := s.bus.Publish(hubmqtt.TopicMsgStatusFor(tenant, bridgeID), 1, false, data); err != nil {
		slog.Warn("hub_msg: refusal not published", "bridge", bridgeID, "reason", reason, "error", err)
	}
}

// HandleAck takes one answer from a recipient's msg/ack (C5). An answer that does not name an
// operation delivered to that recipient from that origin, with its mid, is ignored.
func (s *Service) HandleAck(topic string, payload []byte) {
	tenant, bridgeID, kind, ok := hubmqtt.ParseMsgTopic(topic)
	if !ok || kind != "ack" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	var a Ack
	if err := json.Unmarshal(payload, &a); err != nil || a.V != 1 {
		acksTotal.WithLabelValues("ignored").Inc()
		return
	}
	op, err := s.st.GetMsgOp(ctx, tenant, a.From, a.OpID)
	if err != nil || op.Recipient != principal(bridgeID) || op.Mid != a.Mid {
		acksTotal.WithLabelValues("ignored").Inc()
		return
	}
	evKind, class, reason, answer, ok := AckEvidence(a)
	if !ok {
		acksTotal.WithLabelValues("ignored").Inc()
		return
	}
	if _, err := s.st.AddMsgOpEvent(ctx, NewEvent(op, evKind, class, "recipient", reason, "", s.now()), Project(op)); err != nil {
		slog.Warn("hub_msg: recipient answer not recorded", "op_id", op.OpID, "error", err)
		return
	}
	if answer != "" {
		if err := s.st.SetMsgOpRecipientAnswer(ctx, tenant, op.Sender, op.OpID, answer); err != nil {
			slog.Warn("hub_msg: recipient answer not kept", "op_id", op.OpID, "error", err)
		}
	}
	acksTotal.WithLabelValues(evKind).Inc()
}

// Run drains the outbox until ctx ends: one pass every interval, and the prune once an hour.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	lastPrune := time.Time{}
	for {
		s.Drain(ctx)
		if s.now().Sub(lastPrune) >= time.Hour {
			if n, err := s.st.PruneMsgOps(ctx, s.now().Add(-KeepAfter-SkewAllowance)); err != nil {
				slog.Warn("hub_msg: prune failed", "error", err)
			} else if n > 0 {
				slog.Info("hub_msg: pruned operations past their retention", "count", n)
			}
			lastPrune = s.now()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Drain makes one pass: the deadlines first, then the deliveries due, then the senders' events.
func (s *Service) Drain(ctx context.Context) {
	s.expire(ctx)
	s.deliver(ctx)
	s.publishStatus(ctx)
}

// expire records hub_expired for every operation past its deadline that is not final. A
// recipient's answer that comes later is still recorded (completed is stronger than expired).
func (s *Service) expire(ctx context.Context) {
	ops, err := s.st.ListExpiredMsgOps(ctx, s.now(), 100)
	if err != nil {
		slog.Warn("hub_msg: expiry sweep failed", "error", err)
		return
	}
	for i := range ops {
		op := &ops[i]
		if _, err := s.st.AddMsgOpEvent(ctx, NewEvent(op, "hub_expired", "expired", "hub", "expired", "", s.now()), Project(op)); err != nil {
			slog.Warn("hub_msg: expiry not recorded", "op_id", op.OpID, "error", err)
		}
	}
}

// deliver publishes every due operation on its recipient's msg/in, records hub_forwarded once
// the broker took it, and sets when to publish it again unless the recipient answers.
func (s *Service) deliver(ctx context.Context) {
	ops, err := s.st.ListDueMsgOps(ctx, s.now(), 50)
	if err != nil {
		slog.Warn("hub_msg: due operations not listed", "error", err)
		return
	}
	for i := range ops {
		op := &ops[i]
		won, err := s.st.ClaimMsgOp(ctx, op.TenantID, op.Sender, op.OpID, s.now(), s.now().Add(s.Lease))
		if err != nil || !won {
			continue
		}
		rid := BridgeOf(op.Recipient)
		switch ok, err := s.bridgeExists(ctx, op.TenantID, rid); {
		case err != nil:
			slog.Warn("hub_msg: recipient not looked up, delivery deferred", "op_id", op.OpID, "error", err)
			_ = s.st.MarkMsgOpAttempt(ctx, op.TenantID, op.Sender, op.OpID, s.now().Add(Backoff(1)))
			continue
		case !ok:
			// Removed from the tenant after the operation was admitted.
			_, _ = s.st.AddMsgOpEvent(ctx, NewEvent(op, "hub_refused", "failed", "hub", "refused_unknown_recipient", "", s.now()), Project(op))
			_ = s.st.MarkMsgOpAttempt(ctx, op.TenantID, op.Sender, op.OpID, s.now().Add(time.Hour))
			continue
		}
		data, _ := json.Marshal(DeliveryOf(op))
		if err := s.bus.Publish(hubmqtt.TopicMsgInFor(op.TenantID, rid), 1, false, data); err != nil {
			deliveriesTotal.WithLabelValues("error").Inc()
			slog.Warn("hub_msg: delivery not published", "op_id", op.OpID, "error", err)
			_ = s.st.MarkMsgOpAttempt(ctx, op.TenantID, op.Sender, op.OpID, s.now().Add(Backoff(1)))
			continue
		}
		deliveriesTotal.WithLabelValues("published").Inc()
		if _, err := s.st.AddMsgOpEvent(ctx, NewEvent(op, "hub_forwarded", "forwarded", "hub", "", "", s.now()), Project(op)); err != nil {
			slog.Warn("hub_msg: forwarding not recorded", "op_id", op.OpID, "error", err)
		}
		if err := s.st.MarkMsgOpAttempt(ctx, op.TenantID, op.Sender, op.OpID, s.now().Add(Backoff(op.Attempts+1))); err != nil {
			slog.Warn("hub_msg: attempt not recorded", "op_id", op.OpID, "error", err)
		}
	}
}

// publishStatus publishes every event the senders' msg/status has not taken yet.
func (s *Service) publishStatus(ctx context.Context) {
	evs, err := s.st.ListUnpublishedMsgOpEvents(ctx, 200)
	if err != nil {
		slog.Warn("hub_msg: unpublished events not listed", "error", err)
		return
	}
	ops := map[[3]string]*store.MsgOp{}
	var done []string
	for _, e := range evs {
		key := [3]string{e.TenantID, e.Sender, e.OpID}
		op, ok := ops[key]
		if !ok {
			got, err := s.st.GetMsgOp(ctx, e.TenantID, e.Sender, e.OpID)
			if err != nil {
				continue
			}
			op, ops[key] = got, got
		}
		data, _ := json.Marshal(Wire(op, e))
		if err := s.bus.Publish(hubmqtt.TopicMsgStatusFor(e.TenantID, BridgeOf(e.Sender)), 1, false, data); err != nil {
			slog.Warn("hub_msg: event not published", "event_id", e.EventID, "error", err)
			break
		}
		done = append(done, e.EventID)
	}
	if err := s.st.MarkMsgOpEventsPublished(ctx, done, s.now()); err != nil {
		slog.Warn("hub_msg: published events not marked", "error", err)
	}
}

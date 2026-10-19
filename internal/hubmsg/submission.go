package hubmsg

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// ContentTypeText is the one content type of stage 2.
const ContentTypeText = "text/plain;charset=utf-8"

// MaxBody is the largest body the Hub admits, in bytes.
const MaxBody = 16 * 1024

// Submission is what a sender publishes on its own msg/out (C4). Body is the text, a JSON
// string: its UTF-8 bytes are the bytes the recipient stores and the digest covers.
type Submission struct {
	V             int    `json:"v"`
	OpID          string `json:"op_id"`
	Mid           string `json:"mid"`
	To            string `json:"to"`
	ContentType   string `json:"content_type"`
	MidExpiresAt  int64  `json:"mid_expires_at"`
	ExpiresAt     int64  `json:"expires_at"`
	ContentSHA256 string `json:"content_sha256"`
	Body          string `json:"body"`
}

// The results of a submission, as the fixtures name them.
const (
	Admitted = "admitted"
	Replay   = "replay_all_events"
	Refused  = "refused"
)

// Verdict is what the Hub decided about one submission.
type Verdict struct {
	Result string `json:"result"`
	Reason string `json:"reason"`
}

func refused(reason string) Verdict { return Verdict{Result: Refused, Reason: reason} }

// Check is the part of C4's ingress that needs no store, in the contract's order: valid ids
// (C1e), the digest recomputed from the delivered fields (C1d), the op's deadline within the
// message's. Then the Hub's own limits of stage 2: the version, the recipient's form, the
// content type, the size. "" when the submission passes.
func (s Submission) Check() string {
	if !IDValid(s.OpID, "op_id", IDContext{}) || !IDValid(s.Mid, "mid", IDContext{}) {
		return "bad_id"
	}
	digest, err := ContentSHA256(s.Mid, s.To, s.ContentType, s.MidExpiresAt, []byte(s.Body))
	if err != nil || digest != s.ContentSHA256 {
		return "digest_mismatch"
	}
	if s.ExpiresAt > s.MidExpiresAt {
		return "bad_expiry"
	}
	switch {
	case s.V != 1:
		return "bad_version"
	case BridgeOf(s.To) == "":
		return "bad_recipient"
	case s.ContentType != ContentTypeText:
		return "bad_content_type"
	case len(s.Body) > MaxBody:
		return "too_large"
	}
	return ""
}

// BridgeOf returns the bridge id of a "bridge:<id>" principal, or "".
func BridgeOf(principal string) string {
	id, ok := strings.CutPrefix(principal, "bridge:")
	if !ok || id == "" || strings.ContainsAny(id, "/+#") {
		return ""
	}
	return id
}

// sameImmutable is C4's repeat rule: the same recipient, mid, digest and deadline.
func sameImmutable(o *store.MsgOp, s Submission) bool {
	return o.Recipient == s.To && o.Mid == s.Mid && o.ContentSHA256 == s.ContentSHA256 && o.ExpiresAt == s.ExpiresAt
}

// Admitter is what Submit needs from the store.
type Admitter interface {
	GetMsgOp(ctx context.Context, tenantID, sender, opID string) (*store.MsgOp, error)
	AdmitMsgOp(ctx context.Context, op *store.MsgOp) (store.MsgOpAdmission, error)
}

// Submit decides one submission of sender in tenant (C4), storing the operation when it is
// new. known reports whether the recipient is a bridge of the tenant; it is asked only for a
// new operation, as is the deadline against now: a repeat of a stored operation is answered
// from what is stored, even after its deadline, so a sender that lost the events gets them.
// An error from known decides nothing: the sender's next repeat is decided then.
func Submit(ctx context.Context, st Admitter, tenant, sender string, s Submission, now time.Time, known func(recipient string) (bool, error)) (Verdict, *store.MsgOp, error) {
	if reason := s.Check(); reason != "" {
		return refused(reason), nil, nil
	}
	op := &store.MsgOp{
		TenantID: tenant, Sender: sender, OpID: s.OpID, Mid: s.Mid, Recipient: s.To, ContentType: s.ContentType,
		MidExpiresAt: s.MidExpiresAt, ExpiresAt: s.ExpiresAt, ContentSHA256: s.ContentSHA256, Body: []byte(s.Body),
		// Due at once by the clock that decides it: the drainer asks ListDueMsgOps with the
		// same clock, so a store's own time never delays the first publication.
		State: "pending", NextAttemptAt: now,
	}
	// The deadline and the recipient decide only for a new operation, so the stored row is
	// read first. AdmitMsgOp decides the key again in its transaction, for a racing repeat.
	switch o, err := st.GetMsgOp(ctx, tenant, sender, s.OpID); {
	case err == nil:
		return repeatVerdict(o, s), o, nil
	case !errors.Is(err, store.ErrNotFound):
		return Verdict{}, nil, err
	}
	if now.UnixMilli() > s.ExpiresAt {
		return refused("expired_at_hub"), nil, nil
	}
	if known != nil {
		switch ok, err := known(s.To); {
		case err != nil:
			return Verdict{}, nil, err
		case !ok:
			return refused("refused_unknown_recipient"), nil, nil
		}
	}
	got, err := st.AdmitMsgOp(ctx, op)
	if err != nil {
		return Verdict{}, nil, err
	}
	switch got.Outcome {
	case store.MsgOpAdmitted:
		return Verdict{Result: Admitted}, op, nil
	case store.MsgOpMidConflict:
		return refused("mid_conflict"), nil, nil
	default:
		return repeatVerdict(got.Existing, s), got.Existing, nil
	}
}

func repeatVerdict(o *store.MsgOp, s Submission) Verdict {
	if sameImmutable(o, s) {
		return Verdict{Result: Replay}
	}
	return refused("op_conflict")
}

// Package signups tells the platform's administrators when somebody is waiting
// for a decision on an account request.
//
// # What was missing
//
// Approval has lived in the Hub since MESHSAT-978 and the applicant is emailed
// the moment it happens. The step before it was silent to the operator: a
// stranger finishes enrolment, lands inactive in the meshsat-pending group, and
// the only notice was a YouTrack issue and a Matrix message raised by an n8n
// workflow the identity provider calls. No inbox. So a request sat in the Beta
// requests panel until somebody happened to open Settings, while the person who
// had just been asked to verify their address waited without knowing for what.
//
// # Why a watcher and not a hook
//
// Enrolment happens in the identity provider, not here, so the Hub has no event
// to hang an email on. It does have the list, which is better: this reports
// STATE rather than an occurrence. A notification that fires once can be lost --
// a relay down for ten minutes, a pod killed mid-send -- and nothing ever
// mentions that request again. A watcher that reads what is outstanding corrects
// itself on the next pass, and goes on reminding while the work is still there.
//
// # Not a second approval mechanism
//
// It only reads and sends. Approval stays exactly where it is, in
// internal/api/signups.go behind an operator's session. This package cannot
// admit anybody.
package signups

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/meshsat/meshsat-hub/internal/authentik"
	"github.com/meshsat/meshsat-hub/internal/mail"
)

// stateKey is where the watcher remembers what it has already reported, in
// system_config.
//
// In the DATABASE rather than in memory, deliberately. The watcher is a leader
// singleton, so a lease handover moves it to the other replica -- and a replica
// that remembered nothing would re-announce every outstanding request the moment
// it took over. Every deploy rolls both pods, so that would be an email per
// deploy per waiting request.
const stateKey = "signup_notify_state"

// Defaults. Both are deliberately unhurried: an account request is not an
// outage, and the cost of being early is an inbox nobody reads.
const (
	// DefaultEvery is how often the pending list is read.
	DefaultEvery = 10 * time.Minute
	// DefaultRemind is how long a request may stay outstanding before the
	// operator is reminded about it again.
	DefaultRemind = 24 * time.Hour
)

// Pending is the identity provider, narrowed to the one call this needs.
type Pending interface {
	ListPending(ctx context.Context) ([]authentik.PendingUser, error)
}

// Admins resolves who to tell.
type Admins interface {
	AdminEmails(ctx context.Context, group string) ([]string, error)
}

// StateStore is the Hub's system_config. There is no delete in that interface,
// so "forget everything" is writing an empty value, the same shape
// bridge_provision.go uses to consume a claim.
type StateStore interface {
	GetSystemConfig(ctx context.Context, key string) (string, error)
	SetSystemConfig(ctx context.Context, key, value string) error
}

// Watcher emails the administrators about account requests awaiting a decision.
type Watcher struct {
	ak     Pending
	admins Admins
	store  StateStore
	mail   mail.Sender

	// group is the identity provider group whose members are the platform's
	// administrators. to overrides it with one configured address.
	group string
	to    string

	hubURL string
	every  time.Duration
	remind time.Duration
	now    func() time.Time
	log    *slog.Logger
}

// New returns a watcher. Any missing collaborator yields nil, which main reads
// as "this is not configured" rather than as an error at startup -- the same
// contract authentik.New and mail.New already use.
func New(ak Pending, admins Admins, st StateStore, m mail.Sender, group, to, hubURL string, log *slog.Logger) *Watcher {
	if ak == nil || st == nil || m == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &Watcher{
		ak: ak, admins: admins, store: st, mail: m,
		group: group, to: to, hubURL: hubURL,
		every: DefaultEvery, remind: DefaultRemind,
		now: time.Now, log: log,
	}
}

// Run polls until ctx is cancelled. Registered with leaderSingletons, so
// exactly one replica sends: two would mean two emails per request.
func (w *Watcher) Run(ctx context.Context) {
	w.log.Info("signups: watching for account requests awaiting a decision",
		"every", w.every.String(), "remind_after", w.remind.String(),
		"recipients", w.recipientSource())
	t := time.NewTicker(w.every)
	defer t.Stop()
	w.Once(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Once(ctx)
		}
	}
}

// recipientSource describes where the addresses come from, for the startup line.
// The addresses themselves are not logged: an operator's address in a log that
// ships off the cluster is avoidable.
func (w *Watcher) recipientSource() string {
	if w.to != "" {
		return "configured address"
	}
	return "members of " + w.group
}

// state is what has already been reported.
type state struct {
	// Reported are the identity-provider primary keys already announced. A pk
	// is stable and is not personal data, unlike the address.
	Reported []int `json:"reported"`
	// LastSent is when the last notice went out, which is what paces the
	// reminder.
	LastSent time.Time `json:"last_sent"`
}

// Once does one pass.
func (w *Watcher) Once(ctx context.Context) {
	pending, err := w.ak.ListPending(ctx)
	if err != nil {
		// The identity provider being unreachable is not this job's problem to
		// solve; the next pass tries again. Warn rather than Error: it recovers
		// by itself and a page for it would be noise.
		w.log.Warn("signups: could not read the pending list", "error", err)
		return
	}

	var actionable []authentik.PendingUser
	unverified := 0
	for _, u := range pending {
		// An unverified enrolment cannot be approved -- ak.Approve refuses it
		// with ErrEmailNotVerified -- so naming it as work to do would be
		// telling the operator to do something the system forbids.
		if u.EmailVerified {
			actionable = append(actionable, u)
			continue
		}
		unverified++
	}

	st, ok := w.load(ctx)
	if !ok {
		// Reading the state failed for a reason that is not "nothing stored
		// yet". Treating that as an empty state would re-announce everything
		// outstanding on EVERY pass while the database misbehaved, which turns a
		// storage problem into an email flood. Skip instead.
		return
	}

	if len(actionable) == 0 {
		// Nothing to act on. Forget what was reported so that the next request
		// is announced rather than suppressed by a stale list.
		if len(st.Reported) > 0 || !st.LastSent.IsZero() {
			w.save(ctx, state{})
		}
		return
	}

	ids := make([]int, 0, len(actionable))
	for _, u := range actionable {
		ids = append(ids, u.PK)
	}
	sort.Ints(ids)

	known := make(map[int]bool, len(st.Reported))
	for _, pk := range st.Reported {
		known[pk] = true
	}
	fresh := 0
	for _, pk := range ids {
		if !known[pk] {
			fresh++
		}
	}

	overdue := !st.LastSent.IsZero() && w.now().Sub(st.LastSent) >= w.remind
	if fresh == 0 && !overdue {
		return
	}

	to, err := w.recipients(ctx)
	if err != nil || len(to) == 0 {
		// THIS IS THE FAILURE WORTH SHOUTING ABOUT. Everything else here
		// recovers on the next pass; having nobody to tell means the feature is
		// silently inert, which is the exact condition this package was written
		// to end. Name the consequence, not just the error.
		w.log.Error("signups: account requests are waiting and there is NOBODY TO EMAIL -- "+
			"set HUB_ADMIN_NOTIFY_EMAIL or give a member of the administrator group an address; "+
			"requests will sit unseen until somebody opens Settings",
			"waiting", len(actionable), "group", w.group, "error", err)
		return
	}

	msg := mail.PendingSignups(summaries(actionable), unverified, w.hubURL)
	sent := 0
	for _, addr := range to {
		if err := w.mail.SendMessage(ctx, addr, msg); err != nil {
			w.log.Warn("signups: could not send the pending-requests notice", "error", err)
			continue
		}
		sent++
	}
	if sent == 0 {
		// Nothing recorded, so the next pass retries. The order matters: record
		// first and a relay that was down for one minute would lose the only
		// notice a request ever gets. A duplicate email is a far cheaper mistake
		// than a request nobody hears about.
		return
	}

	w.save(ctx, state{Reported: ids, LastSent: w.now()})
	w.log.Info("signups: told the administrators about account requests awaiting a decision",
		"waiting", len(actionable), "new_since_last_notice", fresh,
		"unverified", unverified, "recipients", len(to), "reminder", overdue && fresh == 0)
}

// recipients is the configured address if there is one, else the administrator
// group's members.
func (w *Watcher) recipients(ctx context.Context) ([]string, error) {
	if w.to != "" {
		return []string{w.to}, nil
	}
	if w.admins == nil {
		return nil, errors.New("no administrator lookup and no configured address")
	}
	return w.admins.AdminEmails(ctx, w.group)
}

// load reads the state. The bool is false only for a failure that is NOT
// "nothing stored yet", so the caller can tell "first run" from "the database is
// unwell" -- they want opposite behaviour.
func (w *Watcher) load(ctx context.Context) (state, bool) {
	raw, err := w.store.GetSystemConfig(ctx, stateKey)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return state{}, true
	case err != nil:
		w.log.Warn("signups: could not read the notification state", "error", err)
		return state{}, false
	}
	if raw == "" {
		// Consumed, the way this interface expresses a delete.
		return state{}, true
	}
	var st state
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		// Unreadable is recoverable: announce again and overwrite it. An
		// unparseable blob must not wedge the notices forever.
		w.log.Warn("signups: notification state is unreadable, starting again", "error", err)
		return state{}, true
	}
	return st, true
}

func (w *Watcher) save(ctx context.Context, st state) {
	raw, err := json.Marshal(st)
	if err != nil {
		w.log.Warn("signups: could not encode the notification state", "error", err)
		return
	}
	if err := w.store.SetSystemConfig(ctx, stateKey, string(raw)); err != nil {
		// The email has already gone. Failing to remember it means the next pass
		// sends again, which is the direction to fail in.
		w.log.Warn("signups: could not record the notification state", "error", err)
	}
}

// summaries converts to the mail package's own shape, which exists so that
// package carries no dependency on the identity provider.
func summaries(users []authentik.PendingUser) []mail.SignupRequest {
	out := make([]mail.SignupRequest, 0, len(users))
	for _, u := range users {
		out = append(out, mail.SignupRequest{
			Name:         u.Name,
			Email:        u.Email,
			Organisation: u.Organisation,
			Country:      u.Country,
			Callsign:     u.Callsign,
			Hardware:     u.Hardware,
			IntendedUse:  u.IntendedUse,
			When:         u.Created,
		})
	}
	return out
}

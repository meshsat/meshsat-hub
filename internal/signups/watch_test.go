package signups

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/authentik"
	"github.com/meshsat/meshsat-hub/internal/mail"
)

// A notice nobody asked for is the failure mode that gets a feature switched
// off. Almost every test here is about NOT sending: once per request, not once
// per ten minutes; not again when the lease moves to the other replica; not at
// all for an enrolment the system would refuse to approve.

type fakeAK struct {
	users []authentik.PendingUser
	err   error
	calls int
}

func (f *fakeAK) ListPending(context.Context) ([]authentik.PendingUser, error) {
	f.calls++
	return f.users, f.err
}

type fakeAdmins struct {
	emails []string
	err    error
	group  string
}

func (f *fakeAdmins) AdminEmails(_ context.Context, group string) ([]string, error) {
	f.group = group
	return f.emails, f.err
}

type fakeStore struct {
	vals    map[string]string
	getErr  error
	setErr  error
	written int
}

func newStore() *fakeStore { return &fakeStore{vals: map[string]string{}} }

func (f *fakeStore) GetSystemConfig(_ context.Context, key string) (string, error) {
	if f.getErr != nil {
		return "", f.getErr
	}
	v, ok := f.vals[key]
	if !ok {
		// What both real stores do: database/sql's no-rows, straight through.
		return "", sql.ErrNoRows
	}
	return v, nil
}

func (f *fakeStore) SetSystemConfig(_ context.Context, key, value string) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.written++
	f.vals[key] = value
	return nil
}

type sentMail struct {
	to  string
	msg mail.Message
}

type fakeMail struct {
	sent []sentMail
	err  error
}

func (f *fakeMail) SendMessage(_ context.Context, to string, m mail.Message) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, sentMail{to: to, msg: m})
	return nil
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func pendingUser(pk int, name string, verified bool) authentik.PendingUser {
	return authentik.PendingUser{
		PK: pk, Username: name, Name: name,
		Email:         name + "@example.org",
		EmailVerified: verified,
		Organisation:  "Example SAR",
		Country:       "NL",
		Created:       time.Date(2026, 9, 13, 9, 0, 0, 0, time.UTC),
	}
}

type rig struct {
	ak     *fakeAK
	admins *fakeAdmins
	store  *fakeStore
	mailer *fakeMail
	w      *Watcher
	clock  time.Time
}

func newRig(t *testing.T, users ...authentik.PendingUser) *rig {
	t.Helper()
	r := &rig{
		ak:     &fakeAK{users: users},
		admins: &fakeAdmins{emails: []string{"admin@meshsat.net"}},
		store:  newStore(),
		mailer: &fakeMail{},
		clock:  time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
	}
	r.w = New(r.ak, r.admins, r.store, r.mailer, "meshsat-platform-admin", "",
		"https://hub.meshsat.net", quietLog())
	if r.w == nil {
		t.Fatal("New returned nil with every collaborator supplied")
	}
	r.w.now = func() time.Time { return r.clock }
	return r
}

func TestAWaitingRequestIsReportedOnce(t *testing.T) {
	r := newRig(t, pendingUser(1, "thomas", true))

	r.w.Once(context.Background())
	if len(r.mailer.sent) != 1 {
		t.Fatalf("first pass sent %d emails, want 1", len(r.mailer.sent))
	}
	got := r.mailer.sent[0]
	if got.to != "admin@meshsat.net" {
		t.Errorf("sent to %q, want the administrator", got.to)
	}
	if !strings.Contains(got.msg.Subject, "1 account request") {
		t.Errorf("subject = %q, want it to say how many are waiting", got.msg.Subject)
	}
	if !strings.Contains(got.msg.Text, "thomas@example.org") {
		t.Errorf("the notice does not name the applicant:\n%s", got.msg.Text)
	}

	// THE TEST THAT MATTERS. The watcher runs every ten minutes forever; if the
	// same request is announced on every pass, the operator stops reading the
	// notices and the feature is worse than nothing.
	for i := 0; i < 5; i++ {
		r.clock = r.clock.Add(time.Minute)
		r.w.Once(context.Background())
	}
	if len(r.mailer.sent) != 1 {
		t.Fatalf("the same request was announced %d times; it must be announced once",
			len(r.mailer.sent))
	}
}

func TestALeaderHandoverDoesNotReannounce(t *testing.T) {
	// The watcher is a leader singleton, so it moves between replicas on a lease
	// change -- and every deploy rolls both pods. A watcher that remembered in
	// memory would re-announce every outstanding request on each handover.
	r := newRig(t, pendingUser(1, "thomas", true))
	r.w.Once(context.Background())
	if len(r.mailer.sent) != 1 {
		t.Fatalf("first pass sent %d, want 1", len(r.mailer.sent))
	}

	// A fresh Watcher over the SAME store is exactly what the other replica has.
	other := New(r.ak, r.admins, r.store, r.mailer, "meshsat-platform-admin", "",
		"https://hub.meshsat.net", quietLog())
	other.now = func() time.Time { return r.clock.Add(time.Minute) }
	other.Once(context.Background())

	if len(r.mailer.sent) != 1 {
		t.Fatalf("the replica that took over re-announced the request (%d emails total)",
			len(r.mailer.sent))
	}
}

func TestAStillWaitingRequestIsRemindedAfterADay(t *testing.T) {
	r := newRig(t, pendingUser(1, "thomas", true))
	r.w.Once(context.Background())

	r.clock = r.clock.Add(DefaultRemind - time.Minute)
	r.w.Once(context.Background())
	if len(r.mailer.sent) != 1 {
		t.Fatalf("reminded early: %d emails before the reminder window elapsed", len(r.mailer.sent))
	}

	r.clock = r.clock.Add(2 * time.Minute)
	r.w.Once(context.Background())
	if len(r.mailer.sent) != 2 {
		t.Fatalf("no reminder after %s: %d emails", DefaultRemind, len(r.mailer.sent))
	}
}

func TestASecondRequestIsAnnouncedAndBothAreListed(t *testing.T) {
	r := newRig(t, pendingUser(1, "thomas", true))
	r.w.Once(context.Background())

	r.ak.users = append(r.ak.users, pendingUser(2, "matthew", true))
	r.clock = r.clock.Add(time.Minute)
	r.w.Once(context.Background())

	if len(r.mailer.sent) != 2 {
		t.Fatalf("a new request was not announced: %d emails", len(r.mailer.sent))
	}
	last := r.mailer.sent[1].msg
	// The notice reports what is outstanding, not only what changed: an operator
	// opening one email should see the whole queue.
	for _, want := range []string{"thomas@example.org", "matthew@example.org"} {
		if !strings.Contains(last.Text, want) {
			t.Errorf("the notice omits %s; it must list everything still waiting:\n%s", want, last.Text)
		}
	}
	if !strings.Contains(last.Subject, "2 account requests") {
		t.Errorf("subject = %q, want the count", last.Subject)
	}
}

func TestAnUnverifiedEnrolmentIsNotReportedAsWork(t *testing.T) {
	// ak.Approve refuses an unverified address with ErrEmailNotVerified, so
	// telling the operator to approve one is telling them to do something the
	// system forbids.
	r := newRig(t, pendingUser(1, "notyet", false))
	r.w.Once(context.Background())
	if len(r.mailer.sent) != 0 {
		t.Fatalf("emailed about a request that cannot be approved: %d emails", len(r.mailer.sent))
	}

	// When it verifies, it becomes work and is announced.
	r.ak.users[0].EmailVerified = true
	r.clock = r.clock.Add(time.Minute)
	r.w.Once(context.Background())
	if len(r.mailer.sent) != 1 {
		t.Fatalf("a verified request was not announced: %d emails", len(r.mailer.sent))
	}
}

func TestUnverifiedOnesAreCountedBesideActionableOnes(t *testing.T) {
	r := newRig(t, pendingUser(1, "thomas", true), pendingUser(2, "notyet", false))
	r.w.Once(context.Background())
	if len(r.mailer.sent) != 1 {
		t.Fatalf("sent %d emails, want 1", len(r.mailer.sent))
	}
	m := r.mailer.sent[0].msg
	if !strings.Contains(m.Text, "has not confirmed its address") {
		t.Errorf("the notice does not mention the unverified enrolment:\n%s", m.Text)
	}
	// It is counted, not listed: there is nothing to act on for it.
	if strings.Contains(m.Text, "notyet@example.org") {
		t.Error("an unverified enrolment was listed as though it could be approved")
	}
	if !strings.Contains(m.Subject, "1 account request") {
		t.Errorf("subject = %q; the unverified one must not inflate the count", m.Subject)
	}
}

func TestAnEmptyQueueForgetsSoTheNextRequestIsAnnounced(t *testing.T) {
	r := newRig(t, pendingUser(1, "thomas", true))
	r.w.Once(context.Background())

	// Approved, so it leaves the pending group.
	r.ak.users = nil
	r.clock = r.clock.Add(time.Minute)
	r.w.Once(context.Background())
	if len(r.mailer.sent) != 1 {
		t.Fatalf("emailed about an empty queue: %d emails", len(r.mailer.sent))
	}

	// A brand new request must be announced, not suppressed by a stale list. If
	// pk reuse or a re-enrolment produced the same pk, a watcher that never
	// cleared its state would stay silent forever.
	r.ak.users = []authentik.PendingUser{pendingUser(1, "thomas", true)}
	r.clock = r.clock.Add(time.Minute)
	r.w.Once(context.Background())
	if len(r.mailer.sent) != 2 {
		t.Fatalf("a new request after an empty queue was not announced: %d emails", len(r.mailer.sent))
	}
}

func TestAFailedSendIsRetriedRatherThanForgotten(t *testing.T) {
	r := newRig(t, pendingUser(1, "thomas", true))
	r.mailer.err = errors.New("relay refused")

	r.w.Once(context.Background())
	if r.store.written != 0 {
		t.Fatal("recorded the request as reported even though no email was sent; " +
			"that request would never be mentioned again")
	}

	r.mailer.err = nil
	r.clock = r.clock.Add(time.Minute)
	r.w.Once(context.Background())
	if len(r.mailer.sent) != 1 {
		t.Fatalf("the retry did not send: %d emails", len(r.mailer.sent))
	}
}

func TestNobodyToEmailIsLoudAndSendsNothing(t *testing.T) {
	r := newRig(t, pendingUser(1, "thomas", true))
	r.admins.emails = nil

	var buf strings.Builder
	r.w.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r.w.Once(context.Background())

	if len(r.mailer.sent) != 0 {
		t.Fatalf("sent %d emails with no recipient resolved", len(r.mailer.sent))
	}
	if r.store.written != 0 {
		t.Fatal("recorded the request as reported when nobody was told")
	}
	line := buf.String()
	if !strings.Contains(line, "ERROR") || !strings.Contains(line, "NOBODY TO EMAIL") {
		t.Errorf("a feature that is silently inert must say so at ERROR; got:\n%s", line)
	}
	// The operator needs to know what to do about it.
	if !strings.Contains(line, "HUB_ADMIN_NOTIFY_EMAIL") {
		t.Errorf("the error does not name the fix:\n%s", line)
	}
}

func TestAnUnreadableStateDoesNotFloodTheInbox(t *testing.T) {
	// An error that is NOT "nothing stored yet" must not be read as an empty
	// state: the watcher would re-announce everything outstanding on every pass
	// for as long as the database misbehaved.
	r := newRig(t, pendingUser(1, "thomas", true))
	r.store.getErr = errors.New("connection reset")

	for i := 0; i < 4; i++ {
		r.clock = r.clock.Add(time.Minute)
		r.w.Once(context.Background())
	}
	if len(r.mailer.sent) != 0 {
		t.Fatalf("a failing state read produced %d emails; it must produce none", len(r.mailer.sent))
	}
}

func TestCorruptStateRecoversRatherThanWedging(t *testing.T) {
	// The opposite direction: a blob that is present but unparseable must not
	// stop notices forever. Announce again and overwrite it.
	r := newRig(t, pendingUser(1, "thomas", true))
	r.store.vals[stateKey] = "{not json"
	r.w.Once(context.Background())
	if len(r.mailer.sent) != 1 {
		t.Fatalf("unreadable state wedged the notices: %d emails", len(r.mailer.sent))
	}
	if r.store.vals[stateKey] == "{not json" {
		t.Error("the corrupt state was not replaced, so it would be re-read forever")
	}
}

func TestAConfiguredAddressWinsOverTheGroup(t *testing.T) {
	r := newRig(t, pendingUser(1, "thomas", true))
	r.w.to = "ops@example.org"
	r.admins.emails = []string{"admin@meshsat.net"}

	r.w.Once(context.Background())
	if len(r.mailer.sent) != 1 || r.mailer.sent[0].to != "ops@example.org" {
		t.Fatalf("sent = %+v, want exactly one to the configured address", r.mailer.sent)
	}
	if r.admins.group != "" {
		t.Error("the identity provider was queried even though an address was configured")
	}
}

func TestEveryAdministratorIsTold(t *testing.T) {
	r := newRig(t, pendingUser(1, "thomas", true))
	r.admins.emails = []string{"one@meshsat.net", "two@meshsat.net"}
	r.w.Once(context.Background())
	if len(r.mailer.sent) != 2 {
		t.Fatalf("told %d of 2 administrators", len(r.mailer.sent))
	}
	if r.admins.group != "meshsat-platform-admin" {
		t.Errorf("looked up group %q, want the configured administrator group", r.admins.group)
	}
}

func TestAnUnreachableIdentityProviderIsQuietAndRetries(t *testing.T) {
	r := newRig(t, pendingUser(1, "thomas", true))
	r.ak.err = errors.New("502 bad gateway")
	r.w.Once(context.Background())
	if len(r.mailer.sent) != 0 || r.store.written != 0 {
		t.Fatal("acted on a failed read of the pending list")
	}

	r.ak.err = nil
	r.clock = r.clock.Add(time.Minute)
	r.w.Once(context.Background())
	if len(r.mailer.sent) != 1 {
		t.Fatalf("did not recover once the provider answered: %d emails", len(r.mailer.sent))
	}
}

func TestNewRefusesAnIncompleteSetOfCollaborators(t *testing.T) {
	// nil rather than a half-working watcher, the contract authentik.New and
	// mail.New already use, so main can treat it as "not configured".
	st, m, ak := newStore(), &fakeMail{}, &fakeAK{}
	for what, w := range map[string]*Watcher{
		"no identity provider": New(nil, nil, st, m, "g", "", "u", quietLog()),
		"no store":             New(ak, nil, nil, m, "g", "", "u", quietLog()),
		"no mailer":            New(ak, nil, st, nil, "g", "", "u", quietLog()),
	} {
		if w != nil {
			t.Errorf("%s: New returned a watcher", what)
		}
	}
}

// Run must return when its context is cancelled, or a shutdown hangs on it.
func TestRunStopsOnContextCancel(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.w.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return when its context was cancelled")
	}
	if r.ak.calls == 0 {
		t.Error("Run did not do a pass before waiting for its first tick")
	}
}

package takfront

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// CertFile serves the front's server certificate from disk and notices when it
// changes.
//
// # Why this has to exist
//
// The certificate the front presents is `meshsat-net-tls`, renewed end to end --
// Let's Encrypt, then the NL cert-manager, then a PushSecret into OpenBao, then
// an ExternalSecret into the namespace. When it renews, kubelet rewrites the
// files in the mounted volume underneath a running pod. A certificate that was
// parsed once at startup does not change, so the front would keep presenting the
// expired one until somebody happened to restart it -- and because every tenant's
// phones check this one chain, they would all fail within the same hour, months
// after the code that caused it was written.
//
// This is the third shape of one recurring bug in this estate: the tak-operator
// reading TAK_OTS_IMAGE once from envFrom, the Hub reading its Stripe key once
// from a Secret, and a TLS certificate read once from a volume. NATS solves its
// copy with a sidecar that SIGHUPs on change; inside our own process a callback
// is cheaper and needs no second container.
//
// # What it does not do
//
// It does not watch with inotify. A mounted Secret is updated by an atomic
// symlink swap of the whole directory, which inotify on the leaf path misses
// anyway, and a renewal is a once-every-sixty-days event: a stat every minute is
// plenty and cannot get stuck. It also never serves nothing -- a reload that
// fails keeps the certificate already in hand, because a half-written file
// during an update must not take the listener down.
type CertFile struct {
	certPath, keyPath string
	log               *slog.Logger

	// checkEvery bounds how often the files are stat'ed. Tests set it to zero so
	// every call checks.
	checkEvery time.Duration
	now        func() time.Time

	mu        sync.RWMutex
	cur       *tls.Certificate
	stamp     string
	lastCheck time.Time
	reloads   int
}

// defaultCertCheckInterval is how often the files are stat'ed at most. A renewal
// happens every sixty days or so, and a phone reconnects constantly, so a minute
// of staleness is invisible.
const defaultCertCheckInterval = time.Minute

// NewCertFile loads the pair once and fails if it cannot: a front that starts
// without a usable certificate would accept connections and refuse every
// handshake, which reads as "TAK is broken" rather than "TAK is misconfigured".
func NewCertFile(certPath, keyPath string, log *slog.Logger) (*CertFile, error) {
	if log == nil {
		log = slog.Default()
	}
	c := &CertFile{
		certPath:   certPath,
		keyPath:    keyPath,
		log:        log,
		checkEvery: defaultCertCheckInterval,
		now:        time.Now,
	}
	pair, stamp, err := c.read()
	if err != nil {
		return nil, err
	}
	c.cur, c.stamp, c.lastCheck = pair, stamp, c.now()
	if leaf := leafOf(pair); leaf != nil {
		log.Info("takfront: server certificate loaded",
			"subject", leaf.Subject.CommonName,
			"issuer", leaf.Issuer.CommonName,
			"not_after", leaf.NotAfter.UTC())
	}
	return c, nil
}

// GetCertificate is the tls.Config callback.
func (c *CertFile) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.RLock()
	cur, last := c.cur, c.lastCheck
	c.mu.RUnlock()

	if c.now().Sub(last) < c.checkEvery {
		return cur, nil
	}
	return c.refresh(cur), nil
}

// refresh re-reads when the files have changed, and always returns something
// usable.
func (c *CertFile) refresh(cur *tls.Certificate) *tls.Certificate {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastCheck = c.now()

	stamp, err := c.signature()
	if err != nil {
		// The files went away or became unreadable mid-life. Keep serving: an
		// unreadable volume is not a reason to stop answering phones.
		c.log.Warn("takfront: cannot stat the server certificate, keeping the one in hand",
			"error", err)
		return c.cur
	}
	if stamp == c.stamp {
		return c.cur
	}

	pair, newStamp, err := c.read()
	if err != nil {
		// Very likely a partially written update. The stamp is deliberately NOT
		// recorded, so the next check tries again rather than accepting a broken
		// file as the new state.
		c.log.Error("takfront: the server certificate changed but does not load, keeping the previous one",
			"error", err)
		return c.cur
	}
	c.cur, c.stamp, c.reloads = pair, newStamp, c.reloads+1
	if leaf := leafOf(pair); leaf != nil {
		c.log.Info("takfront: server certificate reloaded after a change on disk",
			"subject", leaf.Subject.CommonName,
			"not_after", leaf.NotAfter.UTC(),
			"reloads", c.reloads)
	}
	return c.cur
}

func (c *CertFile) read() (*tls.Certificate, string, error) {
	pair, err := tls.LoadX509KeyPair(c.certPath, c.keyPath)
	if err != nil {
		return nil, "", fmt.Errorf("takfront: loading %s: %w", c.certPath, err)
	}
	stamp, err := c.signature()
	if err != nil {
		return nil, "", err
	}
	return &pair, stamp, nil
}

// signature is what "changed" means: size and modification time of both files.
// Content hashing would be more exact and would read 4 KB every minute for the
// rest of the process's life to learn nothing.
func (c *CertFile) signature() (string, error) {
	var sb string
	for _, p := range []string{c.certPath, c.keyPath} {
		st, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		sb += fmt.Sprintf("%s:%d:%d;", p, st.Size(), st.ModTime().UnixNano())
	}
	return sb, nil
}

// NotAfter is when the certificate currently held expires, for a readiness check
// or an alert. Zero if it cannot be parsed.
func (c *CertFile) NotAfter() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if leaf := leafOf(c.cur); leaf != nil {
		return leaf.NotAfter
	}
	return time.Time{}
}

// Reloads counts how many times the file changed under us. Exported for tests
// and for anything that wants to assert a renewal was picked up.
func (c *CertFile) Reloads() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.reloads
}

// leafOf returns the leaf certificate, parsing it if the field is not filled in.
//
// tls.LoadX509KeyPair populates Leaf as of Go 1.23, but this only ever reads it
// for a log line and an expiry, so falling back costs nothing and means a future
// caller building a tls.Certificate by hand does not silently get a nil here.
func leafOf(pair *tls.Certificate) *x509.Certificate {
	if pair == nil {
		return nil
	}
	if pair.Leaf != nil {
		return pair.Leaf
	}
	if len(pair.Certificate) == 0 {
		return nil
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil
	}
	return leaf
}

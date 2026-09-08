package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/rest"
)

// CASecretWriter keeps a Kubernetes Secret's key equal to the bridge CA
// certificate so NATS and stunnel verify bridge client certificates against
// the CA the Hub actually signs with, including after a rotation. It only
// patches an existing Secret (get + merge-patch; never create or delete):
// the Secret is pre-created by the platform tree and must not be reconciled
// by another controller, or the two would fight.
//
// It talks to the API server with the in-cluster REST config and plain
// JSON: linking the typed core/v1 client would add ~27 MiB to the binary.
type CASecretWriter struct {
	client    *http.Client
	base      string // API server base URL
	namespace string
	name      string
	key       string

	mu      sync.Mutex
	lastErr error
	synced  time.Time
}

// ErrCASecretNotFound is returned when the target Secret does not exist.
var ErrCASecretNotFound = errors.New("bridge-ca: target secret not found (it must be pre-created)")

// NewCASecretWriter builds a writer from the in-cluster service account.
// namespace defaults to POD_NAMESPACE, key to "ca.crt".
func NewCASecretWriter(name, key string) (*CASecretWriter, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("bridge-ca: in-cluster config: %w", err)
	}
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("bridge-ca: api client: %w", err)
	}
	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		ns = "default"
	}
	return NewCASecretWriterWithClient(hc, cfg.Host, ns, name, key), nil
}

// NewCASecretWriterWithClient is the constructor used by tests: client must
// already carry the credentials, base is the API server URL.
func NewCASecretWriterWithClient(client *http.Client, base, namespace, name, key string) *CASecretWriter {
	if key == "" {
		key = "ca.crt"
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	w := &CASecretWriter{client: client, base: strings.TrimRight(base, "/"), namespace: namespace, name: name, key: key}
	w.lastErr = errors.New("bridge-ca: secret not synced yet")
	return w
}

func (w *CASecretWriter) secretURL() string {
	return w.base + "/api/v1/namespaces/" + url.PathEscape(w.namespace) + "/secrets/" + url.PathEscape(w.name)
}

// Sync writes certPEM into the Secret when it differs. Idempotent.
func (w *CASecretWriter) Sync(ctx context.Context, certPEM []byte) error {
	err := w.sync(ctx, certPEM)
	w.mu.Lock()
	w.lastErr = err
	if err == nil {
		w.synced = time.Now()
	}
	w.mu.Unlock()
	return err
}

type secretDoc struct {
	Data map[string]string `json:"data"`
}

func (w *CASecretWriter) sync(ctx context.Context, certPEM []byte) error {
	if len(certPEM) == 0 {
		return errors.New("bridge-ca: empty certificate")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.secretURL(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := w.client.Do(req) // #nosec G704 -- in-cluster API server address from the service account config
	if err != nil {
		return fmt.Errorf("bridge-ca: get secret: %w", err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	_ = resp.Body.Close()
	if err != nil {
		return fmt.Errorf("bridge-ca: read secret: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s/%s", ErrCASecretNotFound, w.namespace, w.name)
	default:
		return fmt.Errorf("bridge-ca: get secret: status %d", resp.StatusCode)
	}
	var cur secretDoc
	if err := json.Unmarshal(body, &cur); err != nil {
		return fmt.Errorf("bridge-ca: parse secret: %w", err)
	}
	if existing, err := base64.StdEncoding.DecodeString(cur.Data[w.key]); err == nil && bytes.Equal(existing, certPEM) {
		return nil
	}
	patch, _ := json.Marshal(secretDoc{Data: map[string]string{w.key: base64.StdEncoding.EncodeToString(certPEM)}})
	preq, err := http.NewRequestWithContext(ctx, http.MethodPatch, w.secretURL(), bytes.NewReader(patch))
	if err != nil {
		return err
	}
	preq.Header.Set("Content-Type", "application/merge-patch+json")
	preq.Header.Set("Accept", "application/json")
	presp, err := w.client.Do(preq) // #nosec G704 -- see above
	if err != nil {
		return fmt.Errorf("bridge-ca: patch secret: %w", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(presp.Body, 1<<20))
	_ = presp.Body.Close()
	if presp.StatusCode != http.StatusOK {
		return fmt.Errorf("bridge-ca: patch secret: status %d", presp.StatusCode)
	}
	slog.Info("bridge-ca: CA certificate written to secret", "namespace", w.namespace, "secret", w.name, "key", w.key)
	return nil
}

// LastError reports the outcome of the most recent Sync (nil = in sync);
// wired to the readiness endpoint as the informational probe
// "bridge_ca_export".
func (w *CASecretWriter) LastError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastErr
}

// Run syncs now and then every interval until ctx is done, so a CA rotated
// at runtime reaches NATS without a restart. certPEM is read on each tick.
func (w *CASecretWriter) Run(ctx context.Context, interval time.Duration, certPEM func() []byte) {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := w.Sync(ctx, certPEM()); err != nil {
			slog.Warn("bridge-ca: secret sync failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

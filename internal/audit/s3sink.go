package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/objstore"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// Sink receives the entries archived before a purge (MESHSAT-864 MR 22).
type Sink interface {
	// Write stores the entries of one tenant purged at cutoff.
	Write(ctx context.Context, tenantID string, cutoff time.Time, entries []store.AuditEntry) error
	// Name describes the sink in logs.
	Name() string
}

// FileSink appends JSONL files under dir/{tenant}/audit-{date}.jsonl (the
// historical layout, with a tenant directory now that retention covers every
// tenant).
type FileSink struct{ Dir string }

func (f FileSink) Name() string { return "file:" + f.Dir }

func (f FileSink) Write(_ context.Context, tenantID string, cutoff time.Time, entries []store.AuditEntry) error {
	dir := filepath.Join(f.Dir, filepath.Base(tenantID))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create archive dir: %w", err)
	}
	filename := filepath.Join(dir, fmt.Sprintf("audit-%s.jsonl", cutoff.Format("2006-01-02")))
	fh, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640) // #nosec G304 -- operator-configured archive dir + fixed name
	if err != nil {
		return fmt.Errorf("open archive file: %w", err)
	}
	defer func() { _ = fh.Close() }()
	enc := json.NewEncoder(fh)
	for i := range entries {
		if err := enc.Encode(&entries[i]); err != nil {
			return fmt.Errorf("write entry: %w", err)
		}
	}
	return nil
}

// S3Sink puts one JSONL object per tenant and purge day at
// {prefix}/{tenant}/audit-{date}.jsonl using AWS Signature V4 over plain
// net/http (no SDK: MESHSAT-864 keeps the dependency count flat). Path-style
// addressing suits SeaweedFS and MinIO alike.
type S3Sink struct {
	Prefix string // "hub-audit"

	store *objstore.Client
}

// S3Config is the operator configuration of the sink.
type S3Config struct {
	Endpoint, Bucket, Prefix, Region, AccessKey, SecretKey string
}

// NewS3Sink validates the configuration and returns the sink.
func NewS3Sink(c S3Config) (*S3Sink, error) {
	client, err := objstore.New(objstore.Config{
		Endpoint: c.Endpoint, Bucket: c.Bucket, Region: c.Region,
		AccessKey: c.AccessKey, SecretKey: c.SecretKey,
	})
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	prefix := strings.Trim(c.Prefix, "/")
	if prefix == "" {
		prefix = "hub-audit"
	}
	return &S3Sink{Prefix: prefix, store: client}, nil
}

func (s *S3Sink) Name() string {
	return "s3:" + s.store.Endpoint() + "/" + s.store.Bucket() + "/" + s.Prefix
}

// Key returns the object key for a tenant and purge day. Tenant IDs are
// xids or t_<hex>; anything else is reduced to a safe key segment.
func (s *S3Sink) Key(tenantID string, cutoff time.Time) string {
	return s.Prefix + "/" + safeSegment(tenantID) + "/audit-" + cutoff.Format("2006-01-02") + ".jsonl"
}

func safeSegment(v string) string {
	var b strings.Builder
	for _, r := range v {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

func (s *S3Sink) Write(ctx context.Context, tenantID string, cutoff time.Time, entries []store.AuditEntry) error {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for i := range entries {
		if err := enc.Encode(&entries[i]); err != nil {
			return fmt.Errorf("encode entry: %w", err)
		}
	}
	key := s.Key(tenantID, cutoff)
	if err := s.store.Put(ctx, key, "application/x-ndjson", body.Bytes()); err != nil {
		return err
	}
	slog.Info("audit: archived to s3", "tenant", tenantID, "key", key, "entries", len(entries), "bytes", body.Len())
	return nil
}

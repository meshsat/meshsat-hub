package audit

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
	Endpoint  string // https://nl-s3.nuclearlighters.net
	Bucket    string
	Prefix    string // "hub-audit"
	Region    string // "us-east-1" unless the endpoint cares
	AccessKey string
	SecretKey string
	Client    *http.Client
	now       func() time.Time
}

// S3Config is the operator configuration of the sink.
type S3Config struct {
	Endpoint, Bucket, Prefix, Region, AccessKey, SecretKey string
}

// NewS3Sink validates the configuration and returns the sink.
func NewS3Sink(c S3Config) (*S3Sink, error) {
	if c.Endpoint == "" || c.Bucket == "" || c.AccessKey == "" || c.SecretKey == "" {
		return nil, errors.New("audit: s3 sink needs endpoint, bucket, access key and secret key")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("audit: s3 endpoint %q is not an http(s) URL", c.Endpoint)
	}
	if c.Region == "" {
		c.Region = "us-east-1"
	}
	c.Prefix = strings.Trim(c.Prefix, "/")
	if c.Prefix == "" {
		c.Prefix = "hub-audit"
	}
	return &S3Sink{Endpoint: strings.TrimRight(c.Endpoint, "/"), Bucket: c.Bucket, Prefix: c.Prefix, Region: c.Region,
		AccessKey: c.AccessKey, SecretKey: c.SecretKey, Client: &http.Client{Timeout: 60 * time.Second}, now: time.Now}, nil
}

func (s *S3Sink) Name() string { return "s3:" + s.Endpoint + "/" + s.Bucket + "/" + s.Prefix }

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
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.Endpoint+"/"+s.Bucket+"/"+key, bytes.NewReader(body.Bytes()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.ContentLength = int64(body.Len())
	s.sign(req, body.Bytes())
	resp, err := s.Client.Do(req) // #nosec G704 -- operator-configured archive endpoint
	if err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("put %s: status %d: %s", key, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	slog.Info("audit: archived to s3", "tenant", tenantID, "key", key, "entries", len(entries), "bytes", body.Len())
	return nil
}

// sign adds the SigV4 Authorization header (service s3, UNSIGNED payload
// hashing is not used: the body hash is signed).
func (s *S3Sink) sign(req *http.Request, body []byte) {
	t := s.now().UTC()
	amzDate := t.Format("20060102T150405Z")
	dateStamp := t.Format("20060102")
	payloadHash := sha256Hex(body)
	req.Header.Set("Host", req.URL.Host)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	headers := map[string]string{
		"host":                 req.URL.Host,
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           amzDate,
	}
	if ct := req.Header.Get("Content-Type"); ct != "" {
		headers["content-type"] = ct
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + strings.TrimSpace(headers[k]) + "\n")
	}
	signedHeaders := strings.Join(names, ";")
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL.Path),
		req.URL.Query().Encode(),
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")
	scope := dateStamp + "/" + s.Region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonicalRequest))}, "\n")
	signature := hex.EncodeToString(hmacSHA256(signingKey(s.SecretKey, dateStamp, s.Region, "s3"), []byte(stringToSign)))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.AccessKey, scope, signedHeaders, signature))
}

// canonicalURI encodes each path segment the way SigV4 expects (S3: single
// encoding, '/' kept).
func canonicalURI(p string) string {
	if p == "" {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = awsEscape(seg)
	}
	return strings.Join(segs, "/")
}

func awsEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

// signingKey derives kSigning per the SigV4 specification.
func signingKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

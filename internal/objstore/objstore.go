// Package objstore is a small S3 client built on net/http and AWS Signature
// V4, with no SDK: MESHSAT-864 keeps the Hub's dependency count flat and the
// Hub needs three verbs only. Addressing is path-style, which suits SeaweedFS
// and MinIO as well as AWS.
package objstore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// EmptyPayloadHash is the SHA-256 of an empty body, the payload hash of every
// request that sends no bytes (GET, HEAD).
const EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Config describes one bucket on one endpoint.
type Config struct {
	Endpoint  string // https://nl-s3.nuclearlighters.net
	Bucket    string
	Region    string        // "us-east-1" unless the endpoint cares
	AccessKey string        // never logged
	SecretKey string        // never logged
	Timeout   time.Duration // per request, 60s when zero
}

// Client talks to one bucket.
type Client struct {
	endpoint  string
	bucket    string
	region    string
	accessKey string
	secretKey string

	HTTP *http.Client
	now  func() time.Time
}

// New validates the configuration and returns a client.
func New(c Config) (*Client, error) {
	if c.Endpoint == "" || c.Bucket == "" || c.AccessKey == "" || c.SecretKey == "" {
		return nil, errors.New("objstore: endpoint, bucket, access key and secret key are all required")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("objstore: endpoint %q is not an http(s) URL", c.Endpoint)
	}
	if c.Region == "" {
		c.Region = "us-east-1"
	}
	if c.Timeout <= 0 {
		c.Timeout = 60 * time.Second
	}
	return &Client{
		endpoint:  strings.TrimRight(c.Endpoint, "/"),
		bucket:    c.Bucket,
		region:    c.Region,
		accessKey: c.AccessKey,
		secretKey: c.SecretKey,
		HTTP:      &http.Client{Timeout: c.Timeout},
		now:       time.Now,
	}, nil
}

// Endpoint returns the endpoint URL, and Bucket the bucket name. Neither
// reveals a credential, so both are safe to log.
func (c *Client) Endpoint() string { return c.endpoint }

// Bucket returns the bucket this client is bound to.
func (c *Client) Bucket() string { return c.bucket }

// URL returns the absolute URL of an object key.
func (c *Client) URL(key string) string {
	return c.endpoint + "/" + c.bucket + "/" + strings.TrimPrefix(key, "/")
}

// SetClock replaces the clock; tests use it to sign deterministically.
func (c *Client) SetClock(now func() time.Time) { c.now = now }

// Put stores body at key. contentType may be empty.
func (c *Client) Put(ctx context.Context, key, contentType string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.URL(key), bytes.NewReader(body))
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.ContentLength = int64(len(body))
	c.Sign(req, PayloadHash(body))
	resp, err := c.HTTP.Do(req) // #nosec G704 -- operator-configured object store endpoint
	if err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("put %s: status %d: %s", key, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// Get fetches an object. byteRange, when not empty, is sent verbatim as the
// Range header ("bytes=0-16383"), so the store answers 206 with the slice.
// The caller owns the response body and must close it.
func (c *Client) Get(ctx context.Context, key, byteRange string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL(key), nil)
	if err != nil {
		return nil, err
	}
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	c.Sign(req, EmptyPayloadHash)
	resp, err := c.HTTP.Do(req) // #nosec G704 -- operator-configured object store endpoint
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	return resp, nil
}

// Sign adds the SigV4 Authorization header. payloadHash is the hex SHA-256 of
// the request body (EmptyPayloadHash for a body-less request); it is signed,
// never the UNSIGNED-PAYLOAD sentinel. Headers outside the canonical set are
// not signed, which S3 permits and keeps a forwarded Range header out of the
// signature.
func (c *Client) Sign(req *http.Request, payloadHash string) {
	t := c.now().UTC()
	amzDate := t.Format("20060102T150405Z")
	dateStamp := t.Format("20060102")
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
	scope := dateStamp + "/" + c.region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, PayloadHash([]byte(canonicalRequest))}, "\n")
	signature := hex.EncodeToString(hmacSHA256(SigningKey(c.secretKey, dateStamp, c.region, "s3"), []byte(stringToSign)))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.accessKey, scope, signedHeaders, signature))
}

// PayloadHash is the hex SHA-256 SigV4 signs a body with.
func PayloadHash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// SigningKey derives kSigning per the SigV4 specification.
func SigningKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
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
	for _, ch := range []byte(s) {
		if ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.' || ch == '~' {
			b.WriteByte(ch)
		} else {
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

package objstore

import (
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The derived signing key for the AWS SigV4 documentation example
// (secret wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY, 2015-08-30, us-east-1, iam),
// cross-checked with an independent HMAC-SHA256 chain in Python.
func TestSigningKeyVector(t *testing.T) {
	k := SigningKey("wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "20150830", "us-east-1", "iam")
	if got := hex.EncodeToString(k); got != "2c94c0cf5378ada6887f09bb697df8fc0affdb34ba1cdd5bda32b664bd55b73c" {
		t.Fatalf("signing key = %s", got)
	}
}

func TestEmptyPayloadHashConstant(t *testing.T) {
	if PayloadHash(nil) != EmptyPayloadHash {
		t.Fatalf("empty payload hash = %s", PayloadHash(nil))
	}
}

func TestRejectsBadConfig(t *testing.T) {
	if _, err := New(Config{Endpoint: "ftp://x", Bucket: "b", AccessKey: "a", SecretKey: "s"}); err == nil {
		t.Error("scheme must be http(s)")
	}
	if _, err := New(Config{Endpoint: "https://x", Bucket: "b"}); err == nil {
		t.Error("credentials required")
	}
	if _, err := New(Config{Endpoint: "https://x", AccessKey: "a", SecretKey: "s"}); err == nil {
		t.Error("bucket required")
	}
}

func TestSignIsDeterministicAndScoped(t *testing.T) {
	c, err := New(Config{Endpoint: "https://s3.example", Bucket: "b", AccessKey: "AKIA", SecretKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	c.SetClock(func() time.Time { return time.Date(2026, 9, 8, 7, 0, 0, 0, time.UTC) })
	req1, _ := http.NewRequest(http.MethodPut, c.URL("k"), nil)
	req2, _ := http.NewRequest(http.MethodPut, c.URL("k"), nil)
	c.Sign(req1, PayloadHash([]byte("x")))
	c.Sign(req2, PayloadHash([]byte("x")))
	auth := req1.Header.Get("Authorization")
	if auth != req2.Header.Get("Authorization") {
		t.Error("signature not deterministic")
	}
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIA/20260908/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=") {
		t.Errorf("authorization %q", auth)
	}
	// A different body must not sign the same.
	req3, _ := http.NewRequest(http.MethodPut, c.URL("k"), nil)
	c.Sign(req3, PayloadHash([]byte("y")))
	if req3.Header.Get("Authorization") == auth {
		t.Error("payload hash is not part of the signature")
	}
}

func TestGetForwardsRangeAndSigns(t *testing.T) {
	var gotRange, gotAuth, gotHash, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange, gotAuth = r.Header.Get("Range"), r.Header.Get("Authorization")
		gotHash, gotPath = r.Header.Get("X-Amz-Content-Sha256"), r.URL.Path
		w.Header().Set("Content-Range", "bytes 0-3/9")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("tile"))
	}))
	defer srv.Close()
	c, err := New(Config{Endpoint: srv.URL, Bucket: "bucket", AccessKey: "a", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get(context.Background(), "basemap/world.pmtiles", "bytes=0-3")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(body) != "tile" {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	if gotRange != "bytes=0-3" {
		t.Errorf("range %q", gotRange)
	}
	if gotPath != "/bucket/basemap/world.pmtiles" {
		t.Errorf("path %q", gotPath)
	}
	if gotHash != EmptyPayloadHash || !strings.Contains(gotAuth, "SignedHeaders=host;x-amz-content-sha256;x-amz-date") {
		t.Errorf("hash %q auth %q", gotHash, gotAuth)
	}
}

func TestPutSurfacesStoreErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "AccessDenied", http.StatusForbidden)
	}))
	defer srv.Close()
	c, _ := New(Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s"})
	err := c.Put(context.Background(), "k", "text/plain", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("error = %v", err)
	}
}

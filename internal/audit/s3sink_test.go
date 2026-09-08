package audit

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/objstore"
	"github.com/meshsat/meshsat-hub/internal/store"
)

func TestS3SinkPutSignedAndKeyed(t *testing.T) {
	var gotPath, gotAuth, gotHash, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotHash = r.Header.Get("X-Amz-Content-Sha256")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	sink, err := NewS3Sink(S3Config{Endpoint: srv.URL, Bucket: "cnpg-meshsat-hub", Prefix: "/hub-audit/", AccessKey: "AKIA", SecretKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	sink.store.SetClock(func() time.Time { return time.Date(2026, 9, 8, 7, 0, 0, 0, time.UTC) })
	cutoff := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	err = sink.Write(context.Background(), "t_a/b", cutoff, []store.AuditEntry{{ID: "1", Action: "login"}, {ID: "2", Action: "logout"}})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/cnpg-meshsat-hub/hub-audit/t_a_b/audit-2026-06-10.jsonl" {
		t.Errorf("path %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AKIA/20260908/us-east-1/s3/aws4_request, SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date, Signature=") || len(gotAuth) < 150 {
		t.Errorf("authorization %q", gotAuth)
	}
	if gotHash != objstore.PayloadHash([]byte(gotBody)) || strings.Count(gotBody, "\n") != 2 || !strings.Contains(gotBody, `"action":"login"`) {
		t.Errorf("body/hash mismatch: %q %q", gotHash, gotBody)
	}
}

func TestS3SinkRejectsBadConfig(t *testing.T) {
	if _, err := NewS3Sink(S3Config{Endpoint: "ftp://x", Bucket: "b", AccessKey: "a", SecretKey: "s"}); err == nil {
		t.Error("scheme must be http(s)")
	}
	if _, err := NewS3Sink(S3Config{Endpoint: "https://x", Bucket: "b"}); err == nil {
		t.Error("credentials required")
	}
}

func TestS3SinkSurfacesErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "AccessDenied", http.StatusForbidden)
	}))
	defer srv.Close()
	sink, _ := NewS3Sink(S3Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s"})
	if err := sink.Write(context.Background(), "default", time.Now(), []store.AuditEntry{{ID: "1"}}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("expected a 403 error, got %v", err)
	}
}

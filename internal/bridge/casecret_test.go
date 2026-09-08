package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeAPI is the Secret endpoint of an API server: one secret, counting patches.
type fakeAPI struct {
	data    map[string]string
	exists  bool
	patches int
}

func (f *fakeAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/meshsat-hub/secrets/meshsat-bridge-ca" {
			w.WriteHeader(404)
			return
		}
		if !f.exists {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"kind":"Status","reason":"NotFound"}`))
			return
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Secret", "data": f.data})
		case http.MethodPatch:
			if r.Header.Get("Content-Type") != "application/merge-patch+json" {
				w.WriteHeader(415)
				return
			}
			var doc secretDoc
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &doc)
			for k, v := range doc.Data {
				f.data[k] = v
			}
			f.patches++
			_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Secret", "data": f.data})
		default:
			w.WriteHeader(405)
		}
	})
}

func TestCASecretWriter_PatchesOnlyWhenDifferent(t *testing.T) {
	api := &fakeAPI{exists: true, data: map[string]string{"ca.crt": base64.StdEncoding.EncodeToString([]byte("old"))}}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()
	w := NewCASecretWriterWithClient(srv.Client(), srv.URL, "meshsat-hub", "meshsat-bridge-ca", "")
	if w.LastError() == nil {
		t.Fatal("must report not-synced before the first Sync")
	}
	if err := w.Sync(context.Background(), []byte("new")); err != nil {
		t.Fatal(err)
	}
	if got, _ := base64.StdEncoding.DecodeString(api.data["ca.crt"]); string(got) != "new" || api.patches != 1 {
		t.Fatalf("secret not updated: %q patches=%d", got, api.patches)
	}
	if err := w.Sync(context.Background(), []byte("new")); err != nil || api.patches != 1 {
		t.Fatalf("idempotent sync patched again: err=%v patches=%d", err, api.patches)
	}
	if w.LastError() != nil {
		t.Fatalf("LastError after success: %v", w.LastError())
	}
	if err := w.Sync(context.Background(), nil); err == nil {
		t.Fatal("empty certificate accepted")
	}
}

func TestCASecretWriter_MissingSecretIsAnError(t *testing.T) {
	api := &fakeAPI{exists: false, data: map[string]string{}}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()
	w := NewCASecretWriterWithClient(srv.Client(), srv.URL, "meshsat-hub", "meshsat-bridge-ca", "ca.crt")
	err := w.Sync(context.Background(), []byte("pem"))
	if !errors.Is(err, ErrCASecretNotFound) {
		t.Fatalf("expected ErrCASecretNotFound, got %v", err)
	}
	if w.LastError() == nil || api.patches != 0 {
		t.Fatalf("LastError must carry the failure and nothing may be written: %v %d", w.LastError(), api.patches)
	}
}

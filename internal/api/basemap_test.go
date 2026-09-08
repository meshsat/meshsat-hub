package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/objstore"
)

func basemapFixture(t *testing.T, store http.HandlerFunc) (*BasemapHandler, func()) {
	t.Helper()
	srv := httptest.NewServer(store)
	client, err := objstore.New(objstore.Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	return NewBasemapHandler(client, "basemap/world.pmtiles", "basemap/local.pmtiles", "basemap/assets", time.Hour), srv.Close
}

func TestBasemapForwardsRangeAndCaches(t *testing.T) {
	var gotRange, gotPath string
	h, done := basemapFixture(t, func(w http.ResponseWriter, r *http.Request) {
		gotRange, gotPath = r.Header.Get("Range"), r.URL.Path
		w.Header().Set("Content-Range", "bytes 0-3/900")
		w.Header().Set("ETag", `"abc"`)
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("tile"))
	})
	defer done()

	req := httptest.NewRequest(http.MethodGet, "/basemap/basemap.pmtiles", nil)
	req.Header.Set("Range", "bytes=0-3")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent || rec.Body.String() != "tile" {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	if gotRange != "bytes=0-3" || gotPath != "/b/basemap/world.pmtiles" {
		t.Errorf("upstream range %q path %q", gotRange, gotPath)
	}
	if rec.Header().Get("Content-Range") != "bytes 0-3/900" || rec.Header().Get("ETag") != `"abc"` {
		t.Errorf("range/etag not passed through: %v", rec.Header())
	}
	if rec.Header().Get("Cache-Control") != "public, max-age=3600" || rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Errorf("cache headers %v", rec.Header())
	}
}

func TestBasemapRejectsMalformedRange(t *testing.T) {
	reached := false
	h, done := basemapFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	defer done()

	req := httptest.NewRequest(http.MethodGet, "/basemap/basemap.pmtiles", nil)
	req.Header.Set("Range", "items=0-3")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || reached {
		t.Fatalf("status %d, upstream reached %v", rec.Code, reached)
	}
}

func TestBasemapMapsStoreStatuses(t *testing.T) {
	for _, tc := range []struct {
		upstream, want int
	}{
		{http.StatusNotFound, http.StatusNotFound},
		{http.StatusForbidden, http.StatusBadGateway},
		{http.StatusInternalServerError, http.StatusBadGateway},
	} {
		h, done := basemapFixture(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.upstream)
		})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/basemap/basemap.pmtiles", nil))
		if rec.Code != tc.want {
			t.Errorf("store %d -> %d, want %d", tc.upstream, rec.Code, tc.want)
		}
		done()
	}
}

func TestBasemapHeadSendsNoBodyAndPostIsRejected(t *testing.T) {
	h, done := basemapFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "900")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("whole archive"))
	})
	defer done()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/basemap/basemap.pmtiles", nil))
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("head: status %d body %d bytes", rec.Code, rec.Body.Len())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/basemap/basemap.pmtiles", strings.NewReader("x")))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") == "" {
		t.Errorf("post: status %d allow %q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestBasemapDescribeNamesTheObject(t *testing.T) {
	h, done := basemapFixture(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	defer done()
	if got := h.Describe(); !strings.HasSuffix(got, "/b/basemap/world.pmtiles") {
		t.Errorf("describe = %q", got)
	}
	_ = io.Discard
}

func TestBasemapAssetPathValidation(t *testing.T) {
	var gotPath string
	h, done := basemapFixture(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("glyphs"))
	})
	defer done()

	// The high-density sprite sheet resolves too.
	rec0 := httptest.NewRecorder()
	req0 := httptest.NewRequest(http.MethodGet, "/basemap/assets/x", nil)
	req0.URL.Path = "/basemap/assets/sprites/v4/dark@2x.png"
	h.ServeAsset(rec0, req0)
	if rec0.Code != http.StatusOK || gotPath != "/b/basemap/assets/sprites/v4/dark@2x.png" {
		t.Fatalf("sprite: status %d upstream %q", rec0.Code, gotPath)
	}
	if ct := rec0.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("sprite content type %q", ct)
	}

	// A font range with a space in the stack name resolves under the prefix.
	req := httptest.NewRequest(http.MethodGet, "/basemap/assets/fonts/Noto%20Sans%20Regular/0-255.pbf", nil)
	req.URL.Path = "/basemap/assets/fonts/Noto Sans Regular/0-255.pbf"
	rec := httptest.NewRecorder()
	h.ServeAsset(rec, req)
	if rec.Code != http.StatusOK || gotPath != "/b/basemap/assets/fonts/Noto Sans Regular/0-255.pbf" {
		t.Fatalf("status %d upstream %q", rec.Code, gotPath)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-protobuf" {
		t.Errorf("content type %q", ct)
	}

	for _, bad := range []string{
		"../../secret.json",
		"fonts/../../etc/passwd.json",
		"fonts/x/y/z/deep/too-deep.pbf",
		"noextension",
		"fonts/stack/0-255.jsonl",
		"/absolute.png",
		"fonts//0-255.pbf",
	} {
		gotPath = ""
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/basemap/assets/x", nil)
		req.URL.Path = "/basemap/assets/" + bad
		h.ServeAsset(rec, req)
		if rec.Code != http.StatusNotFound || gotPath != "" {
			t.Errorf("%q: status %d, upstream %q", bad, rec.Code, gotPath)
		}
	}
}

func TestBasemapLocalArchive(t *testing.T) {
	var gotPath string
	h, done := basemapFixture(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("deep"))
	})
	defer done()

	if !h.HasLocal() {
		t.Fatal("fixture should have a local archive")
	}
	rec := httptest.NewRecorder()
	h.ServeLocal(rec, httptest.NewRequest(http.MethodGet, "/basemap/local.pmtiles", nil))
	if rec.Code != http.StatusOK || gotPath != "/b/basemap/local.pmtiles" {
		t.Fatalf("status %d upstream %q", rec.Code, gotPath)
	}

	// Without one configured the route answers 404 rather than serving the world.
	client, err := objstore.New(objstore.Config{Endpoint: "https://s3.example", Bucket: "b", AccessKey: "a", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	worldOnly := NewBasemapHandler(client, "basemap/world.pmtiles", "", "basemap/assets", time.Hour)
	if worldOnly.HasLocal() {
		t.Fatal("empty local key must not count as configured")
	}
	rec = httptest.NewRecorder()
	worldOnly.ServeLocal(rec, httptest.NewRequest(http.MethodGet, "/basemap/local.pmtiles", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("world-only: status %d", rec.Code)
	}
}

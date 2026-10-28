package warnings

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func dwdZip(t *testing.T, docs map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, doc := range docs {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(doc)
	}
	_ = zw.Close()
	return buf.Bytes()
}

func TestDWDFeedReadsEveryDocumentInTheZip(t *testing.T) {
	z := dwdZip(t, map[string][]byte{
		"a.xml":      fixture(t, "dwd-boeen.cap.xml"),
		"b.xml":      fixture(t, "dwd-second.cap.xml"),
		"broken.xml": []byte("<alert>"),
		"notes.txt":  []byte("ignored"),
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/latest.zip" {
			http.NotFound(w, r)
			return
		}
		if !strings.Contains(r.Header.Get("User-Agent"), "MeshSat") {
			t.Errorf("no user agent: %q", r.Header.Get("User-Agent"))
		}
		_, _ = w.Write(z)
	}))
	defer srv.Close()
	f := &DWDFeed{URL: srv.URL + "/latest.zip", Fetcher: &Fetcher{Client: srv.Client()}}
	got, err := f.Fetch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d messages", len(got))
	}
	for _, m := range got {
		if m.Alert.Sender != "opendata@dwd.de" || len(m.Raw) == 0 {
			t.Fatalf("message: %+v", m.Alert)
		}
	}
	f.URL = srv.URL + "/missing.zip"
	if _, err := f.Fetch(context.Background(), nil); err == nil {
		t.Fatal("a 404 was not an error")
	}
}

func TestMeteoAlarmFeedFetchesOnlyWhatIsNewAndOnlyFromItsHost(t *testing.T) {
	capDoc := fixture(t, "meteoalarm-nl-texel.cap.xml")
	var capFetches atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/feed":
			// The fixture, with its links pointed at this server, and one
			// entry pointed elsewhere.
			atom := strings.ReplaceAll(string(fixture(t, "meteoalarm-nl.atom")), "https://feeds.meteoalarm.org/api/v1/warnings/feeds-netherlands/", srv.URL+"/cap/")
			atom = strings.Replace(atom, "</feed>", `<entry><cap:identifier>elsewhere</cap:identifier><cap:sent>2026-10-28T10:00:00+00:00</cap:sent>`+
				`<link type="application/cap+xml" href="https://evil.example/cap"/></entry></feed>`, 1)
			_, _ = w.Write([]byte(atom))
		case strings.HasPrefix(r.URL.Path, "/cap/"):
			capFetches.Add(1)
			_, _ = w.Write(capDoc)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := &MeteoAlarmFeed{URL: srv.URL + "/feed", ID: SourceMeteoAlarmNL, Fetcher: &Fetcher{Client: srv.Client()}}

	got, err := f.Fetch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Two entries in the fixture link two documents (the same identifier in
	// two languages' index entries resolve to two fetches); the one on
	// another host is never fetched.
	if n := capFetches.Load(); n != 2 || len(got) != 2 {
		t.Fatalf("fetched %d, got %d", n, len(got))
	}
	// Known: nothing fetched.
	capFetches.Store(0)
	sent := time.Date(2026, 10, 28, 10, 11, 41, 0, time.UTC)
	got, err = f.Fetch(context.Background(), func(id string, s time.Time) bool {
		return strings.HasSuffix(id, "_2") && s.Equal(sent)
	})
	if err != nil || len(got) != 0 || capFetches.Load() != 0 {
		t.Fatalf("known entries were fetched: %d %d %v", capFetches.Load(), len(got), err)
	}
}

func TestParseMeteoAlarmFeed(t *testing.T) {
	entries, err := ParseMeteoAlarmFeed(fixture(t, "meteoalarm-nl.atom"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || !strings.HasPrefix(entries[0].CAPURL, "https://feeds.meteoalarm.org/api/v1/warnings/feeds-netherlands/") {
		t.Fatalf("entries: %+v", entries)
	}
	if entries[0].Identifier != "2.49.0.0.528.0.NL.20261028101141.6ddd5a99-421a-4c94-b9ea-25fa0ddcdd48_2" || entries[0].Sent.IsZero() {
		t.Fatalf("entry: %+v", entries[0])
	}
}

func TestFetcherRefusesRedirectsAndBadURLs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	c := NewFeedClient(5 * time.Second)
	c.Transport = srv.Client().Transport
	f := &Fetcher{Client: c}
	if _, err := f.get(context.Background(), srv.URL+"/x", ""); err == nil {
		t.Fatal("a redirect was followed")
	}
	for _, u := range []string{"ftp://x/y", "://", "file:///etc/passwd", ""} {
		if _, err := f.get(context.Background(), u, ""); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
}

package warnings

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The feeds (docs/warning-relay.md "Feeds"). Both are official open data
// read without a key:
//
//   - DWD, Deutscher Wetterdienst: opendata.dwd.de serves a zip of one CAP
//     document per current warning, replaced about every ten minutes; the
//     "LATEST" name always points at the newest. Licence: the DWD open data
//     terms (GeoNutzV, attribution "Quelle: Deutscher Wetterdienst").
//   - MeteoAlarm (EUMETNET): one Atom feed per country whose entries carry the
//     CAP identifier and link the CAP document. The feed says: "Licensed
//     under terms equivalent to CC BY 4.0, with additional requirements for
//     redistributing outlined in our Terms and Conditions."
//
// A fetch is bounded in bytes and in time, follows no redirect off the feed's
// host, and takes only a document it can parse as CAP 1.2.

// Fetch limits.
const (
	maxFeedBytes = 8 << 20 // the DWD zip is a few hundred KB; an Atom feed a few KB
	maxEntries   = 200     // CAP documents fetched from one Atom poll
)

// Feed is one source of CAP messages.
type Feed interface {
	// Source is the Source* id of the feed.
	Source() int
	// Fetch reads the feed's current messages. known answers whether a message
	// (identifier, sent) is already stored, so a feed that links its documents
	// can skip fetching the ones the Hub has.
	Fetch(ctx context.Context, known func(identifier string, sent time.Time) bool) ([]Fetched, error)
}

// Fetched is one CAP message a feed carried, with the document it came in.
type Fetched struct {
	Alert *Alert
	Raw   []byte
}

// Fetcher is the HTTP side both feeds share.
type Fetcher struct {
	Client    *http.Client
	UserAgent string
}

// get reads one URL into memory, bounded. A redirect is refused: the feeds
// answer directly, and following one from a platform URL to an arbitrary
// one would make the configuration a fetch of anything.
func (f *Fetcher) get(ctx context.Context, rawURL string, accept string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("warnings: feed URL %q", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	ua := f.UserAgent
	if ua == "" {
		ua = "MeshSat-Hub warning relay (+https://meshsat.net)"
	}
	req.Header.Set("User-Agent", ua)
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("warnings: %s answered %d", u.Host, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxFeedBytes {
		return nil, fmt.Errorf("warnings: %s answered more than %d bytes", u.Host, maxFeedBytes)
	}
	return body, nil
}

// NoRedirect is the CheckRedirect a feed client uses.
func NoRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// NewFeedClient is the HTTP client the pollers use: a timeout, no redirects.
func NewFeedClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: NoRedirect}
}

// --- DWD -----------------------------------------------------------------------

// DWDFeed reads DWD's CAP snapshot zip.
type DWDFeed struct {
	URL     string
	Fetcher *Fetcher
}

func (f *DWDFeed) Source() int { return SourceDWD }

func (f *DWDFeed) Fetch(ctx context.Context, _ func(string, time.Time) bool) ([]Fetched, error) {
	body, err := f.Fetcher.get(ctx, f.URL, "application/zip")
	if err != nil {
		return nil, err
	}
	return ParseDWDZip(body)
}

// ParseDWDZip reads every CAP document in a DWD snapshot zip. A document
// that does not parse is skipped, not fatal: one bad file must not hide the
// rest of the snapshot.
func ParseDWDZip(data []byte) ([]Fetched, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("warnings: DWD zip: %w", err)
	}
	var out []Fetched
	for _, zf := range zr.File {
		if !strings.HasSuffix(strings.ToLower(zf.Name), ".xml") || zf.UncompressedSize64 > MaxCAPBytes {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			continue
		}
		doc, err := io.ReadAll(io.LimitReader(rc, MaxCAPBytes+1))
		_ = rc.Close()
		if err != nil || len(doc) > MaxCAPBytes {
			continue
		}
		a, err := ParseCAP(doc)
		if err != nil {
			continue
		}
		out = append(out, Fetched{Alert: a, Raw: doc})
	}
	return out, nil
}

// --- MeteoAlarm ----------------------------------------------------------------

// MeteoAlarmFeed reads a MeteoAlarm country Atom feed and the CAP documents
// its entries link.
type MeteoAlarmFeed struct {
	URL     string
	ID      int // the Source* id of this country feed
	Fetcher *Fetcher
}

func (f *MeteoAlarmFeed) Source() int { return f.ID }

// atomFeed is the part of the MeteoAlarm Atom feed the relay reads.
type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	Identifier string     `xml:"identifier"`
	Sent       string     `xml:"sent"`
	Links      []atomLink `xml:"link"`
}

type atomLink struct {
	Type string `xml:"type,attr"`
	Rel  string `xml:"rel,attr"`
	Href string `xml:"href,attr"`
}

// ParseMeteoAlarmFeed lists the entries of a MeteoAlarm Atom feed: the CAP
// identifier, its sent time and the CAP document's address.
func ParseMeteoAlarmFeed(data []byte) ([]MeteoAlarmEntry, error) {
	if len(data) > maxFeedBytes || bytes.Contains(data, []byte("<!DOCTYPE")) {
		return nil, errors.New("warnings: MeteoAlarm feed refused")
	}
	var feed atomFeed
	if err := xml.Unmarshal(data, &feed); err != nil {
		return nil, fmt.Errorf("warnings: MeteoAlarm feed: %w", err)
	}
	var out []MeteoAlarmEntry
	for _, e := range feed.Entries {
		m := MeteoAlarmEntry{Identifier: strings.TrimSpace(e.Identifier)}
		m.Sent, _ = parseCAPTime(e.Sent)
		for _, l := range e.Links {
			if l.Type == "application/cap+xml" && l.Href != "" {
				m.CAPURL = l.Href
			}
		}
		if m.Identifier != "" && m.CAPURL != "" {
			out = append(out, m)
		}
	}
	return out, nil
}

// MeteoAlarmEntry is one entry of the Atom feed.
type MeteoAlarmEntry struct {
	Identifier string
	Sent       time.Time
	CAPURL     string
}

func (f *MeteoAlarmFeed) Fetch(ctx context.Context, known func(string, time.Time) bool) ([]Fetched, error) {
	body, err := f.Fetcher.get(ctx, f.URL, "application/atom+xml")
	if err != nil {
		return nil, err
	}
	entries, err := ParseMeteoAlarmFeed(body)
	if err != nil {
		return nil, err
	}
	feedURL, err := url.Parse(f.URL)
	if err != nil {
		return nil, err
	}
	var out []Fetched
	fetched := 0
	for _, e := range entries {
		if known != nil && known(e.Identifier, e.Sent) {
			continue
		}
		if fetched >= maxEntries {
			break
		}
		// A document is taken from the feed's own host only, over the feed's
		// own scheme: the entry's link is data from the network, not
		// configuration.
		u, err := url.Parse(e.CAPURL)
		if err != nil || u.Host != feedURL.Host || u.Scheme != feedURL.Scheme {
			continue
		}
		fetched++
		doc, err := f.Fetcher.get(ctx, u.String(), "application/cap+xml")
		if err != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			continue
		}
		a, err := ParseCAP(doc)
		if err != nil {
			continue
		}
		out = append(out, Fetched{Alert: a, Raw: doc})
	}
	return out, nil
}

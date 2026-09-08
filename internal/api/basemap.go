package api

import (
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/objstore"
)

// rangeHeader matches the byte-range forms MapLibre's PMTiles reader sends.
// Anything else is dropped rather than forwarded (MESHSAT-967).
var rangeHeader = regexp.MustCompile(`^bytes=[0-9]*-[0-9]*(,[0-9]*-[0-9]*)*$`)

// BasemapHandler streams the self-hosted vector basemap out of the Hub's
// object store: the PMTiles archive at /basemap/basemap.pmtiles and the glyph
// and sprite assets the style needs under /basemap/assets/. The map in the
// browser reads the archive with HTTP range requests, so this forwards Range
// and the store's 206 response verbatim: a session transfers the few tiles it
// displays, not the whole archive. Keys come from configuration and from a
// strictly validated relative path, never from a raw request path, and the
// objects hold public OpenStreetMap-derived data, so the routes carry no
// tenant information and need no authentication.
type BasemapHandler struct {
	store      *objstore.Client
	archiveKey string
	assets     string // key prefix of the glyph and sprite assets
	maxAge     int
}

// NewBasemapHandler returns a handler for one archive object and the asset
// prefix beside it. assetPrefix may be empty, which disables /basemap/assets/.
func NewBasemapHandler(store *objstore.Client, archiveKey, assetPrefix string, maxAge time.Duration) *BasemapHandler {
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}
	return &BasemapHandler{
		store:      store,
		archiveKey: archiveKey,
		assets:     strings.Trim(assetPrefix, "/"),
		maxAge:     int(maxAge.Seconds()),
	}
}

// Describe returns the object this handler serves, for startup logging.
func (h *BasemapHandler) Describe() string {
	return h.store.Endpoint() + "/" + h.store.Bucket() + "/" + h.archiveKey
}

// assetSegment accepts the characters that appear in a glyph or sprite path.
// Font stack directories carry spaces ("Noto Sans Regular"), which arrive
// percent-encoded and are decoded by the router before this check, and the
// high-density sprite sheets carry an "@" ("dark@2x.png").
var assetSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 @._-]*$`)

var assetSuffixes = map[string]string{
	".pbf":  "application/x-protobuf",
	".png":  "image/png",
	".json": "application/json",
}

// assetKey validates a relative asset path and returns its object key. The
// second result is false for anything that is not a plain, shallow path of
// safe segments ending in a known asset suffix: no traversal, no absolute
// path, no empty segment, no control character.
func (h *BasemapHandler) assetKey(rel string) (key, contentType string, ok bool) {
	if h.assets == "" || rel == "" || len(rel) > 200 {
		return "", "", false
	}
	dot := strings.LastIndex(rel, ".")
	if dot < 0 {
		return "", "", false
	}
	contentType, known := assetSuffixes[strings.ToLower(rel[dot:])]
	if !known {
		// Sprite base names are extension-less ("sprites/v4/light"); the
		// style asks for .json and .png explicitly, so anything else is out.
		return "", "", false
	}
	segments := strings.Split(rel, "/")
	if len(segments) > 4 {
		return "", "", false
	}
	for _, seg := range segments {
		if !assetSegment.MatchString(seg) || strings.Contains(seg, "..") {
			return "", "", false
		}
	}
	return h.assets + "/" + rel, contentType, true
}

// ServeAsset streams one glyph range or sprite file.
//
//	@Summary		Basemap glyph and sprite assets
//	@Description	Streams the self-hosted map font ranges and sprite sheets. Public map data, no authentication.
//	@Tags			map
//	@Param			path	path		string	true	"Relative asset path, e.g. fonts/Noto Sans Regular/0-255.pbf"
//	@Success		200		{file}		binary	"Asset"
//	@Failure		404		{object}	map[string]string	"Unknown or malformed asset path"
//	@Router			/basemap/assets/{path} [get]
func (h *BasemapHandler) ServeAsset(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/basemap/assets/")
	key, contentType, ok := h.assetKey(rel)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown basemap asset")
		return
	}
	h.stream(w, r, key, contentType)
}

// ServeHTTP streams the basemap archive.
//
//	@Summary		Vector basemap archive
//	@Description	Streams the self-hosted PMTiles basemap, forwarding HTTP range requests. Public map data, no authentication.
//	@Tags			map
//	@Produce		octet-stream
//	@Param			Range	header	string	false	"Byte range, e.g. bytes=0-16383"
//	@Success		200		{file}	binary	"Whole archive"
//	@Success		206		{file}	binary	"Requested byte range"
//	@Failure		404		{object}	map[string]string	"Basemap object missing from the store"
//	@Failure		502		{object}	map[string]string	"Object store unreachable"
//	@Router			/basemap/basemap.pmtiles [get]
func (h *BasemapHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.stream(w, r, h.archiveKey, "application/octet-stream")
}

// stream copies one object through to the client, forwarding a Range request
// and the store's response status and headers.
func (h *BasemapHandler) stream(w http.ResponseWriter, r *http.Request, key, contentType string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	byteRange := r.Header.Get("Range")
	if byteRange != "" && !rangeHeader.MatchString(byteRange) {
		writeError(w, http.StatusBadRequest, "malformed Range header")
		return
	}
	resp, err := h.store.Get(r.Context(), key, byteRange)
	if err != nil {
		slog.Error("basemap: object store request failed", "error", err, "key", key)
		writeError(w, http.StatusBadGateway, "basemap unavailable")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
	case http.StatusNotFound:
		slog.Warn("basemap: object not found in the store", "key", key)
		writeError(w, http.StatusNotFound, "basemap not configured")
		return
	default:
		slog.Error("basemap: object store returned an error", "status", resp.StatusCode, "key", key)
		writeError(w, http.StatusBadGateway, "basemap unavailable")
		return
	}

	for _, header := range []string{"Content-Range", "ETag", "Last-Modified"} {
		if v := resp.Header.Get(header); v != "" {
			w.Header().Set(header, v)
		}
	}
	if v := resp.Header.Get("Content-Length"); v != "" {
		w.Header().Set("Content-Length", v)
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(h.maxAge))
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		slog.Warn("basemap: streaming to the client stopped", "error", err)
	}
}

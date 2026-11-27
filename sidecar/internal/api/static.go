package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

// Cache policies for the embedded dashboard. Vite content-hashes everything
// under assets/, so those names never change meaning; index.html must be
// revalidated on every load so a new release is picked up at once.
const (
	cacheImmutable = "public, max-age=31536000, immutable"
	cacheNoCache   = "no-cache"
	cachePublic    = "public, max-age=3600"
	staticIndex    = "index.html"
)

// compressibleTypes are the text formats gzipped once at startup. Their
// Content-Type is set explicitly so a gzip body is never sniffed, and so the
// type does not depend on the host's MIME registry.
var compressibleTypes = map[string]string{
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".svg":  "image/svg+xml",
	".html": "text/html; charset=utf-8",
	".json": "application/json",
	".md":   "text/markdown; charset=utf-8",
}

// staticAsset holds the validators and the precompressed copy of one file.
// The identity bytes stay in the fs (the embed is already in memory).
type staticAsset struct {
	name         string
	contentType  string
	cacheControl string
	etag         string
	gz           []byte
	gzETag       string
}

// staticHandler serves the embedded single-page dashboard with gzip, strong
// ETags and per-path Cache-Control. It is immutable after construction, so it
// is safe for concurrent use.
type staticHandler struct {
	fsys   fs.FS
	assets map[string]*staticAsset
}

// newStaticHandler indexes every file in fsys, hashing it for an ETag and
// gzipping the text formats once. Paths without a dot (and "/") fall back to
// index.html so client-side routes load the app; a missing dotted path is 404.
func newStaticHandler(fsys fs.FS) (http.Handler, error) {
	if fsys == nil {
		return nil, errors.New("static dashboard: no file system to serve")
	}
	h := &staticHandler{fsys: fsys, assets: map[string]*staticAsset{}}
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", name, err)
		}
		if d.IsDir() {
			return nil
		}
		a, err := loadStaticAsset(fsys, name)
		if err != nil {
			return err
		}
		h.assets[name] = a
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("static dashboard: %w", err)
	}
	return h, nil
}

func loadStaticAsset(fsys fs.FS, name string) (*staticAsset, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	sum := sha256.Sum256(data)
	tag := hex.EncodeToString(sum[:16])
	a := &staticAsset{
		name:         name,
		contentType:  compressibleTypes[path.Ext(name)],
		cacheControl: cacheControlFor(name),
		etag:         `"` + tag + `"`,
	}
	if a.contentType == "" {
		return a, nil
	}
	gz, err := gzipBytes(data)
	if err != nil {
		return nil, fmt.Errorf("gzip %s: %w", name, err)
	}
	// Only keep a compressed copy that is actually smaller (an empty or tiny
	// file grows under gzip).
	if len(gz) < len(data) {
		a.gz = gz
		a.gzETag = `"` + tag + `-gzip"`
	}
	return a, nil
}

func gzipBytes(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(data); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func cacheControlFor(name string) string {
	switch {
	case strings.HasPrefix(name, "assets/"):
		return cacheImmutable
	case name == staticIndex:
		return cacheNoCache
	default:
		return cachePublic
	}
}

func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || !strings.Contains(name, ".") {
		name = staticIndex
	}
	a, ok := h.assets[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	hdr := w.Header()
	hdr.Set("Cache-Control", a.cacheControl)
	if a.contentType != "" {
		hdr.Set("Content-Type", a.contentType)
	}
	if a.gz != nil {
		hdr.Add("Vary", "Accept-Encoding")
		if acceptsGzip(r.Header.Values("Accept-Encoding")) {
			hdr.Set("Content-Encoding", "gzip")
			hdr.Set("ETag", a.gzETag)
			http.ServeContent(w, r, a.name, time.Time{}, bytes.NewReader(a.gz))
			return
		}
	}
	hdr.Set("ETag", a.etag)
	h.serveIdentity(w, r, a)
}

// serveIdentity streams the uncompressed file from the fs.
func (h *staticHandler) serveIdentity(w http.ResponseWriter, r *http.Request, a *staticAsset) {
	f, err := h.fsys.Open(a.name)
	if err != nil {
		staticAssetError(w, a.name, err)
		return
	}
	defer func() { _ = f.Close() }()
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, a.name, time.Time{}, rs)
		return
	}
	data, err := io.ReadAll(f)
	if err != nil {
		staticAssetError(w, a.name, err)
		return
	}
	http.ServeContent(w, r, a.name, time.Time{}, bytes.NewReader(data))
}

// staticAssetError answers a read failure with a 500 that no cache keeps: the
// asset's validators and its (possibly year-long) Cache-Control are dropped.
func staticAssetError(w http.ResponseWriter, name string, err error) {
	slog.Error("dashboard asset unreadable", "asset", name, "error", err)
	hdr := w.Header()
	hdr.Del("ETag")
	hdr.Del("Vary")
	hdr.Set("Cache-Control", "no-store")
	http.Error(w, "dashboard asset unavailable", http.StatusInternalServerError)
}

// acceptsGzip reports whether the Accept-Encoding values allow gzip: an
// explicit gzip (or x-gzip) coding with q > 0, or else a "*" with q > 0.
// A malformed q value makes that coding unacceptable, so the response falls
// back to identity, which every client can read.
func acceptsGzip(values []string) bool {
	star, gz := -1.0, -1.0
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			coding, q := parseCoding(part)
			switch coding {
			case "gzip", "x-gzip":
				gz = q
			case "*":
				star = q
			}
		}
	}
	if gz >= 0 {
		return gz > 0
	}
	return star > 0
}

// parseCoding splits "gzip;q=0.5" into its lowercased coding and q value.
// The q value is 1 when absent and 0 when malformed or out of range.
func parseCoding(part string) (string, float64) {
	coding, params, _ := strings.Cut(part, ";")
	coding = strings.ToLower(strings.TrimSpace(coding))
	q := 1.0
	for _, p := range strings.Split(params, ";") {
		key, val, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "q") {
			continue
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err != nil || parsed < 0 || parsed > 1 {
			return coding, 0
		}
		q = parsed
	}
	return coding, q
}

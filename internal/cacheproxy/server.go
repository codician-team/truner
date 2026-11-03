// Package cacheproxy implements the truner "magic cache": a local HTTP
// server on each runner that speaks the GitHub Actions cache protocols and
// stores entries in the account's own S3 bucket.
//
// It implements:
//   - Cache service v2 (twirp, JSON and protobuf) used by actions/cache v4+
//   - The Azure Block Blob subset used by the toolkit to upload archives
//   - The legacy v1 REST API (actions/cache v3, older buildkit)
//
// Everything else under /twirp/ (artifacts, etc.) is reverse-proxied to
// GitHub unchanged.
package cacheproxy

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codician-team/truner/internal/cachestore"
)

// DefaultUpstream is GitHub's results service for github.com.
const DefaultUpstream = "https://results-receiver.actions.githubusercontent.com"

const twirpPrefix = "/twirp/github.actions.results.api.v1.CacheService/"

// Server is the cache proxy. Create it with New.
type Server struct {
	store     cachestore.Store
	prefix    string // e.g. cache/owner/repo/
	publicURL string // base URL handed to clients, e.g. http://10.0.1.5:28777
	tempDir   string
	log       *slog.Logger
	upstream  *httputil.ReverseProxy

	// ChunkSize and Concurrency tune parallel downloads from the store.
	ChunkSize   int64
	Concurrency int

	mu        sync.Mutex
	uploads   map[string]*upload // by upload id
	v1uploads map[int64]*upload  // by v1 cache id
	nextV1    int64

	stats Stats
}

// Stats are cumulative cache counters.
type Stats struct {
	Hits, Misses, Saves, SaveErrors int64
	BytesDown, BytesUp              int64
	DownNanos, UpNanos              int64
}

type upload struct {
	id        string
	objKey    string
	key       string
	version   string
	dir       string
	created   time.Time
	mu        sync.Mutex
	blocks    map[string]string // block id -> temp file
	committed bool
	size      int64
	v1file    *os.File
}

// Options configures a Server.
type Options struct {
	Store     cachestore.Store
	Prefix    string
	PublicURL string
	TempDir   string
	Upstream  string
	Logger    *slog.Logger
}

// New creates a cache proxy.
func New(o Options) (*Server, error) {
	if o.Store == nil {
		return nil, errors.New("cacheproxy: store is required")
	}
	if o.Upstream == "" {
		o.Upstream = DefaultUpstream
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.TempDir == "" {
		o.TempDir = filepath.Join(os.TempDir(), "truner-cache")
	}
	if err := os.MkdirAll(o.TempDir, 0o755); err != nil {
		return nil, err
	}
	up, err := url.Parse(o.Upstream)
	if err != nil {
		return nil, err
	}
	rp := httputil.NewSingleHostReverseProxy(up)
	baseDirector := rp.Director
	rp.Director = func(r *http.Request) {
		baseDirector(r)
		r.Host = up.Host
	}
	return &Server{
		store:       o.Store,
		prefix:      o.Prefix,
		publicURL:   strings.TrimRight(o.PublicURL, "/"),
		tempDir:     o.TempDir,
		log:         o.Logger,
		upstream:    rp,
		ChunkSize:   16 << 20,
		Concurrency: 8,
		uploads:     map[string]*upload{},
		v1uploads:   map[int64]*upload{},
		nextV1:      time.Now().Unix() % 1000000,
	}, nil
}

// Stats returns a snapshot of the cache counters.
func (s *Server) Stats() Stats {
	return Stats{
		Hits: atomic.LoadInt64(&s.stats.Hits), Misses: atomic.LoadInt64(&s.stats.Misses),
		Saves: atomic.LoadInt64(&s.stats.Saves), SaveErrors: atomic.LoadInt64(&s.stats.SaveErrors),
		BytesDown: atomic.LoadInt64(&s.stats.BytesDown), BytesUp: atomic.LoadInt64(&s.stats.BytesUp),
		DownNanos: atomic.LoadInt64(&s.stats.DownNanos), UpNanos: atomic.LoadInt64(&s.stats.UpNanos),
	}
}

// ServeHTTP routes requests.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/healthz":
		w.Write([]byte("ok"))
	case strings.HasPrefix(p, twirpPrefix):
		s.handleTwirp(w, r, strings.TrimPrefix(p, twirpPrefix))
	case strings.HasPrefix(p, "/upload/"):
		s.handleUpload(w, r, strings.TrimPrefix(p, "/upload/"))
	case strings.HasPrefix(p, "/download/"):
		s.handleDownload(w, r, strings.TrimPrefix(p, "/download/"))
	case strings.HasPrefix(p, "/_apis/artifactcache/"):
		s.handleV1(w, r, strings.TrimPrefix(p, "/_apis/artifactcache/"))
	default:
		// Artifacts and anything else go to GitHub untouched.
		s.upstream.ServeHTTP(w, r)
	}
}

// ---- scopes ----

// scopes extracts cache scopes from the runtime token, mirroring GitHub's
// branch isolation: the write scope is the current ref, read scopes add the
// base/default branch.
func scopes(authHeader string) (write string, read []string) {
	const fallback = "_"
	tok := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(authHeader, "Bearer "), "bearer "))
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return fallback, []string{fallback}
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return fallback, []string{fallback}
	}
	var claims struct {
		AC string `json:"ac"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.AC == "" {
		return fallback, []string{fallback}
	}
	var acs []struct {
		Scope      string `json:"Scope"`
		Permission int    `json:"Permission"`
	}
	if json.Unmarshal([]byte(claims.AC), &acs) != nil || len(acs) == 0 {
		return fallback, []string{fallback}
	}
	for _, a := range acs {
		if a.Permission&2 != 0 && write == "" {
			write = a.Scope
		}
	}
	if write != "" {
		read = append(read, write)
	}
	for _, a := range acs {
		if a.Permission&1 != 0 && a.Scope != write {
			read = append(read, a.Scope)
		}
	}
	if write == "" {
		write = fallback
	}
	if len(read) == 0 {
		read = []string{fallback}
	}
	return write, read
}

// ---- twirp (cache service v2) ----

type twirpReq struct {
	Key         string   `json:"key"`
	Version     string   `json:"version"`
	RestoreKeys []string `json:"restore_keys"`
	SizeBytes   json.Number
}

func (s *Server) handleTwirp(w http.ResponseWriter, r *http.Request, method string) {
	if r.Method != http.MethodPost {
		twirpError(w, http.StatusMethodNotAllowed, "bad_route", "POST required")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		twirpError(w, http.StatusBadRequest, "malformed", err.Error())
		return
	}
	isProto := strings.HasPrefix(r.Header.Get("Content-Type"), "application/protobuf")
	var req twirpReq
	if isProto {
		m, err := pbDecode(body)
		if err != nil {
			twirpError(w, http.StatusBadRequest, "malformed", err.Error())
			return
		}
		req.Key = pbString(m, 2)
		switch method {
		case "CreateCacheEntry":
			req.Version = pbString(m, 3)
		case "FinalizeCacheEntryUpload":
			req.SizeBytes = json.Number(strconv.FormatInt(pbInt(m, 3), 10))
			req.Version = pbString(m, 4)
		case "GetCacheEntryDownloadURL":
			req.RestoreKeys = pbStrings(m, 3)
			req.Version = pbString(m, 4)
		}
	} else {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			twirpError(w, http.StatusBadRequest, "malformed", err.Error())
			return
		}
		get := func(names ...string) json.RawMessage {
			for _, n := range names {
				if v, ok := raw[n]; ok {
					return v
				}
			}
			return nil
		}
		json.Unmarshal(get("key"), &req.Key)
		json.Unmarshal(get("version"), &req.Version)
		json.Unmarshal(get("restore_keys", "restoreKeys"), &req.RestoreKeys)
		if v := get("size_bytes", "sizeBytes"); v != nil {
			var n json.Number
			if json.Unmarshal(v, &n) != nil {
				var str string
				json.Unmarshal(v, &str)
				n = json.Number(str)
			}
			req.SizeBytes = n
		}
	}
	if req.Key == "" || req.Version == "" {
		twirpError(w, http.StatusBadRequest, "invalid_argument", "key and version are required")
		return
	}
	writeScope, readScopes := scopes(r.Header.Get("Authorization"))
	ctx := r.Context()
	switch method {
	case "CreateCacheEntry":
		uploadURL, err := s.createEntry(ctx, writeScope, req.Key, req.Version)
		if errors.Is(err, errEntryExists) || errors.Is(err, errEntryUploading) {
			// GitHub answers a taken key with already_exists, and BuildKit's gha
			// exporter relies on it: it skips the layer on that code but fails
			// the whole build on ok=false.
			twirpError(w, http.StatusConflict, "already_exists", err.Error())
			return
		}
		if err != nil {
			s.log.Error("cache reserve failed", "key", req.Key, "err", err)
			twirpError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if isProto {
			pw := &pbWriter{}
			pw.boolean(1, true)
			pw.str(2, uploadURL)
			writeProto(w, pw.b)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "signed_upload_url": uploadURL})
	case "FinalizeCacheEntryUpload":
		size, _ := req.SizeBytes.Int64()
		ok, id, msg := s.finalizeEntry(ctx, writeScope, req.Key, req.Version, size)
		if isProto {
			pw := &pbWriter{}
			pw.boolean(1, ok)
			pw.int64(2, id)
			pw.str(3, msg)
			writeProto(w, pw.b)
			return
		}
		writeJSON(w, map[string]any{"ok": ok, "entry_id": strconv.FormatInt(id, 10), "message": msg})
	case "GetCacheEntryDownloadURL":
		ent, err := s.lookup(ctx, readScopes, req.Key, req.RestoreKeys, req.Version)
		if err != nil {
			s.log.Error("cache lookup failed", "key", req.Key, "err", err)
			twirpError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		ok, dl, matched := false, "", ""
		if ent != nil {
			ok, dl, matched = true, s.downloadURL(ent.objKey), ent.key
			atomic.AddInt64(&s.stats.Hits, 1)
			s.log.Info("cache hit", "key", req.Key, "matched", matched, "size", ent.size)
		} else {
			atomic.AddInt64(&s.stats.Misses, 1)
			s.log.Info("cache miss", "key", req.Key)
		}
		if isProto {
			pw := &pbWriter{}
			pw.boolean(1, ok)
			pw.str(2, dl)
			pw.str(3, matched)
			writeProto(w, pw.b)
			return
		}
		writeJSON(w, map[string]any{"ok": ok, "signed_download_url": dl, "matched_key": matched})
	default:
		twirpError(w, http.StatusNotFound, "bad_route", "unknown method "+method)
	}
}

var (
	errEntryExists    = errors.New("cache entry already exists")
	errEntryUploading = errors.New("another job is already saving this cache entry")
)

// createEntry reserves an entry and returns its upload URL.
func (s *Server) createEntry(ctx context.Context, scope, key, version string) (string, error) {
	objKey := cachestore.ObjectKey(s.prefix, scope, version, key)
	if _, err := s.store.Head(ctx, objKey); err == nil {
		return "", errEntryExists
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.uploads {
		if u.objKey == objKey && !u.committed && time.Since(u.created) < time.Hour {
			return "", errEntryUploading
		}
	}
	u, err := s.newUploadLocked(objKey, key, version)
	if err != nil {
		return "", err
	}
	return s.publicURL + "/upload/" + u.id, nil
}

func (s *Server) newUploadLocked(objKey, key, version string) (*upload, error) {
	id := randomID()
	dir := filepath.Join(s.tempDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	u := &upload{id: id, objKey: objKey, key: key, version: version, dir: dir, created: time.Now(), blocks: map[string]string{}}
	s.uploads[id] = u
	return u, nil
}

func (s *Server) finalizeEntry(ctx context.Context, scope, key, version string, size int64) (bool, int64, string) {
	objKey := cachestore.ObjectKey(s.prefix, scope, version, key)
	obj, err := s.store.Head(ctx, objKey)
	if err != nil {
		return false, 0, "cache entry was not uploaded"
	}
	if size > 0 && obj.Size != size {
		s.log.Warn("cache size mismatch", "key", key, "expected", size, "actual", obj.Size)
	}
	atomic.AddInt64(&s.stats.Saves, 1)
	s.log.Info("cache saved", "key", key, "size", obj.Size)
	return true, entryID(objKey), ""
}

type found struct {
	objKey string
	key    string
	size   int64
}

// lookup searches scopes in order; within a scope the primary key must
// match exactly and restore keys match by prefix (newest wins).
func (s *Server) lookup(ctx context.Context, readScopes []string, key string, restoreKeys []string, version string) (*found, error) {
	for _, scope := range readScopes {
		objKey := cachestore.ObjectKey(s.prefix, scope, version, key)
		if obj, err := s.store.Head(ctx, objKey); err == nil {
			return &found{objKey: objKey, key: key, size: obj.Size}, nil
		} else if !errors.Is(err, cachestore.ErrNotFound) {
			return nil, err
		}
		for _, rk := range restoreKeys {
			if rk == "" {
				continue
			}
			base := cachestore.ScopePrefix(s.prefix, scope, version)
			objs, err := s.store.List(ctx, base+url.PathEscape(rk))
			if err != nil {
				return nil, err
			}
			if len(objs) == 0 {
				continue
			}
			sort.Slice(objs, func(i, j int) bool { return objs[i].LastModified.After(objs[j].LastModified) })
			_, _, k, ok := cachestore.ParseObjectKey(s.prefix, objs[0].Key)
			if !ok {
				continue
			}
			return &found{objKey: objs[0].Key, key: k, size: objs[0].Size}, nil
		}
	}
	return nil, nil
}

func (s *Server) downloadURL(objKey string) string {
	return s.publicURL + "/download/" + base64.RawURLEncoding.EncodeToString([]byte(objKey))
}

// ---- Azure Block Blob emulation (uploads) ----

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	u := s.uploads[id]
	s.mu.Unlock()
	if u == nil {
		azureError(w, http.StatusNotFound, "BlobNotFound", "unknown upload")
		return
	}
	if r.Method != http.MethodPut {
		azureError(w, http.StatusMethodNotAllowed, "UnsupportedHttpVerb", "PUT required")
		return
	}
	q := r.URL.Query()
	switch q.Get("comp") {
	case "block":
		s.putBlock(w, r, u, q.Get("blockid"))
	case "blocklist":
		s.putBlockList(w, r, u)
	case "":
		s.putBlob(w, r, u)
	default:
		azureError(w, http.StatusBadRequest, "InvalidQueryParameterValue", "unsupported comp")
	}
}

func (s *Server) putBlock(w http.ResponseWriter, r *http.Request, u *upload, blockID string) {
	if blockID == "" {
		azureError(w, http.StatusBadRequest, "InvalidQueryParameterValue", "blockid required")
		return
	}
	name := filepath.Join(u.dir, hex.EncodeToString([]byte(blockID)))
	f, err := os.Create(name)
	if err != nil {
		azureError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	n, err := io.Copy(f, r.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(name)
		azureError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	u.mu.Lock()
	u.blocks[blockID] = name
	u.mu.Unlock()
	atomic.AddInt64(&s.stats.BytesUp, n)
	azureOK(w, http.StatusCreated)
}

func (s *Server) putBlockList(w http.ResponseWriter, r *http.Request, u *upload) {
	var list struct {
		Items []struct {
			XMLName xml.Name
			ID      string `xml:",chardata"`
		} `xml:",any"`
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err == nil {
		err = xml.Unmarshal(data, &list)
	}
	if err != nil {
		azureError(w, http.StatusBadRequest, "InvalidXmlDocument", err.Error())
		return
	}
	u.mu.Lock()
	var files []string
	for _, it := range list.Items {
		f, ok := u.blocks[strings.TrimSpace(it.ID)]
		if !ok {
			u.mu.Unlock()
			azureError(w, http.StatusBadRequest, "InvalidBlockList", "unknown block "+it.ID)
			return
		}
		files = append(files, f)
	}
	u.mu.Unlock()
	start := time.Now()
	size, err := s.commitFiles(r.Context(), u, files)
	if err != nil {
		atomic.AddInt64(&s.stats.SaveErrors, 1)
		s.log.Error("cache upload failed", "key", u.key, "err", err)
		azureError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	atomic.AddInt64(&s.stats.UpNanos, int64(time.Since(start)))
	s.log.Info("cache archive stored", "key", u.key, "size", size, "blocks", len(files), "took", time.Since(start).Round(time.Millisecond))
	azureOK(w, http.StatusCreated)
}

func (s *Server) commitFiles(ctx context.Context, u *upload, files []string) (int64, error) {
	var total int64
	var readers []io.Reader
	var closers []io.Closer
	defer func() {
		for _, c := range closers {
			c.Close()
		}
	}()
	for _, name := range files {
		f, err := os.Open(name)
		if err != nil {
			return 0, err
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return 0, err
		}
		total += st.Size()
		readers = append(readers, f)
		closers = append(closers, f)
	}
	// Detach from the request context: a client timeout must not leave a
	// half-written entry, and S3 multipart uploads are atomic anyway.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Hour)
	defer cancel()
	if err := s.store.Put(pctx, u.objKey, io.MultiReader(readers...), total); err != nil {
		return 0, err
	}
	s.finishUpload(u, total)
	return total, nil
}

func (s *Server) putBlob(w http.ResponseWriter, r *http.Request, u *upload) {
	start := time.Now()
	cr := &countingReader{r: r.Body}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Hour)
	defer cancel()
	if err := s.store.Put(pctx, u.objKey, cr, r.ContentLength); err != nil {
		atomic.AddInt64(&s.stats.SaveErrors, 1)
		s.log.Error("cache upload failed", "key", u.key, "err", err)
		azureError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	atomic.AddInt64(&s.stats.BytesUp, cr.n)
	atomic.AddInt64(&s.stats.UpNanos, int64(time.Since(start)))
	s.finishUpload(u, cr.n)
	s.log.Info("cache archive stored", "key", u.key, "size", cr.n, "took", time.Since(start).Round(time.Millisecond))
	azureOK(w, http.StatusCreated)
}

func (s *Server) finishUpload(u *upload, size int64) {
	u.mu.Lock()
	u.committed, u.size = true, size
	u.mu.Unlock()
	os.RemoveAll(u.dir)
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// ---- downloads ----

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request, enc string) {
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil || !strings.HasPrefix(string(raw), s.prefix) {
		http.Error(w, "bad download id", http.StatusBadRequest)
		return
	}
	objKey := string(raw)
	obj, err := s.store.Head(r.Context(), objKey)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	off, length := int64(0), obj.Size
	status := http.StatusOK
	// Azure SDK clients (e.g. BuildKit's gha cache) send x-ms-range instead of Range.
	rh := r.Header.Get("x-ms-range")
	if rh == "" {
		rh = r.Header.Get("Range")
	}
	w.Header().Set("x-ms-blob-type", "BlockBlob")
	if rh != "" {
		a, b, ok := parseRange(rh, obj.Size)
		if !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", obj.Size))
			http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		off, length, status = a, b-a+1, http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, obj.Size))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	start := time.Now()
	n, err := s.stream(r.Context(), w, objKey, off, length)
	atomic.AddInt64(&s.stats.BytesDown, n)
	atomic.AddInt64(&s.stats.DownNanos, int64(time.Since(start)))
	if err != nil {
		s.log.Error("cache download failed", "key", objKey, "err", err)
		return
	}
	el := time.Since(start)
	s.log.Info("cache archive served", "size", n, "took", el.Round(time.Millisecond), "MBps", fmt.Sprintf("%.0f", float64(n)/1e6/el.Seconds()))
}

func parseRange(h string, size int64) (int64, int64, bool) {
	spec, ok := strings.CutPrefix(h, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	a, b, _ := strings.Cut(spec, "-")
	var start, end int64
	var err error
	switch {
	case a == "":
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		start, end = size-n, size-1
	default:
		if start, err = strconv.ParseInt(a, 10, 64); err != nil {
			return 0, 0, false
		}
		end = size - 1
		if b != "" {
			if end, err = strconv.ParseInt(b, 10, 64); err != nil {
				return 0, 0, false
			}
			if end >= size {
				end = size - 1
			}
		}
	}
	if start < 0 || start > end || start >= size {
		return 0, 0, false
	}
	return start, end, true
}

var bufPool = sync.Pool{}

// stream copies [off, off+length) of an object to w, fetching chunks in
// parallel and writing them in order.
func (s *Server) stream(ctx context.Context, w io.Writer, objKey string, off, length int64) (int64, error) {
	chunk := s.ChunkSize
	if length <= chunk || s.Concurrency <= 1 {
		rc, err := s.store.GetRange(ctx, objKey, off, length)
		if err != nil {
			return 0, err
		}
		defer rc.Close()
		return io.Copy(w, rc)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	n := int((length + chunk - 1) / chunk)
	type part struct {
		buf []byte
		err error
	}
	parts := make([]chan part, n)
	for i := range parts {
		parts[i] = make(chan part, 1)
	}
	sem := make(chan struct{}, s.Concurrency)
	go func() {
		for i := 0; i < n; i++ {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			go func(i int) {
				start := off + int64(i)*chunk
				size := chunk
				if start+size > off+length {
					size = off + length - start
				}
				var buf []byte
				if b, ok := bufPool.Get().([]byte); ok && int64(cap(b)) >= size {
					buf = b[:size]
				} else {
					buf = make([]byte, size, chunk)
				}
				var err error
				for attempt := 0; attempt < 4; attempt++ {
					var rc io.ReadCloser
					rc, err = s.store.GetRange(ctx, objKey, start, size)
					if err == nil {
						_, err = io.ReadFull(rc, buf)
						rc.Close()
					}
					if err == nil || ctx.Err() != nil {
						break
					}
					time.Sleep(time.Duration(attempt+1) * 300 * time.Millisecond)
				}
				parts[i] <- part{buf: buf, err: err}
			}(i)
		}
	}()
	var written int64
	for i := 0; i < n; i++ {
		var p part
		select {
		case p = <-parts[i]:
		case <-ctx.Done():
			return written, ctx.Err()
		}
		<-sem
		if p.err != nil {
			return written, p.err
		}
		m, err := w.Write(p.buf)
		written += int64(m)
		bufPool.Put(p.buf[:0]) //nolint:staticcheck // slices are fine here
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

// ---- cache service v1 (legacy REST) ----

func (s *Server) handleV1(w http.ResponseWriter, r *http.Request, rest string) {
	writeScope, readScopes := scopes(r.Header.Get("Authorization"))
	ctx := r.Context()
	switch {
	case rest == "cache" && r.Method == http.MethodGet:
		keys := strings.Split(r.URL.Query().Get("keys"), ",")
		version := r.URL.Query().Get("version")
		if len(keys) == 0 || keys[0] == "" || version == "" {
			http.Error(w, "keys and version required", http.StatusBadRequest)
			return
		}
		ent, err := s.lookup(ctx, readScopes, keys[0], keys, version)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if ent == nil {
			atomic.AddInt64(&s.stats.Misses, 1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		atomic.AddInt64(&s.stats.Hits, 1)
		writeJSON(w, map[string]any{
			"cacheKey": ent.key, "scope": writeScope, "cacheVersion": version,
			"creationTime": time.Now().UTC().Format(time.RFC3339), "archiveLocation": s.downloadURL(ent.objKey),
		})
	case rest == "caches" && r.Method == http.MethodPost:
		var req struct {
			Key     string `json:"key"`
			Version string `json:"version"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.Key == "" || req.Version == "" {
			http.Error(w, "key and version required", http.StatusBadRequest)
			return
		}
		objKey := cachestore.ObjectKey(s.prefix, writeScope, req.Version, req.Key)
		if _, err := s.store.Head(ctx, objKey); err == nil {
			writeJSONStatus(w, http.StatusConflict, map[string]any{"message": "cache entry already exists"})
			return
		}
		s.mu.Lock()
		u, err := s.newUploadLocked(objKey, req.Key, req.Version)
		if err == nil {
			u.v1file, err = os.Create(filepath.Join(u.dir, "archive"))
		}
		if err != nil {
			s.mu.Unlock()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.nextV1++
		id := s.nextV1
		s.v1uploads[id] = u
		s.mu.Unlock()
		writeJSONStatus(w, http.StatusCreated, map[string]any{"cacheId": id})
	case strings.HasPrefix(rest, "caches/"):
		id, err := strconv.ParseInt(strings.TrimPrefix(rest, "caches/"), 10, 64)
		s.mu.Lock()
		u := s.v1uploads[id]
		s.mu.Unlock()
		if err != nil || u == nil {
			http.Error(w, "unknown cache id", http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodPatch:
			var start int64
			if cr := r.Header.Get("Content-Range"); cr != "" {
				fmt.Sscanf(cr, "bytes %d-", &start)
			}
			data, err := io.ReadAll(io.LimitReader(r.Body, 512<<20))
			if err == nil {
				_, err = u.v1file.WriteAt(data, start)
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			atomic.AddInt64(&s.stats.BytesUp, int64(len(data)))
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			name := u.v1file.Name()
			u.v1file.Close()
			if _, err := s.commitFiles(ctx, u, []string{name}); err != nil {
				atomic.AddInt64(&s.stats.SaveErrors, 1)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			atomic.AddInt64(&s.stats.Saves, 1)
			s.mu.Lock()
			delete(s.v1uploads, id)
			s.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// Cleanup removes abandoned uploads older than maxAge.
func (s *Server) Cleanup(maxAge time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, u := range s.uploads {
		if time.Since(u.created) > maxAge {
			if u.v1file != nil {
				u.v1file.Close()
			}
			os.RemoveAll(u.dir)
			delete(s.uploads, id)
		}
	}
}

// ---- helpers ----

func randomID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func entryID(objKey string) int64 {
	h := fnv.New64a()
	h.Write([]byte(objKey))
	return int64(h.Sum64() & 0x7fffffffffff)
}

func writeJSON(w http.ResponseWriter, v any) { writeJSONStatus(w, http.StatusOK, v) }

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeProto(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "application/protobuf")
	w.Write(b)
}

func twirpError(w http.ResponseWriter, status int, code, msg string) {
	writeJSONStatus(w, status, map[string]string{"code": code, "msg": msg})
}

func azureOK(w http.ResponseWriter, status int) {
	h := w.Header()
	h.Set("x-ms-request-id", randomID())
	h.Set("x-ms-version", "2024-11-04")
	h.Set("x-ms-request-server-encrypted", "true")
	h.Set("ETag", `"0x`+strings.ToUpper(randomID()[:16])+`"`)
	h.Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	h.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	h.Set("Content-Length", "0")
	w.WriteHeader(status)
}

func azureError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-ms-error-code", code)
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, xmlEscape(msg))
}

func xmlEscape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

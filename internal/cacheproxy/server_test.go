package cacheproxy

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/truner/truner/internal/cachestore"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server, *cachestore.Memory) {
	t.Helper()
	store := cachestore.NewMemory()
	s, err := New(Options{Store: store, Prefix: "cache/acme/app/", TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s.ChunkSize = 1 << 10 // force the parallel download path
	ts := httptest.NewServer(s)
	s.publicURL = ts.URL
	t.Cleanup(ts.Close)
	return s, ts, store
}

func token(ac string) string {
	payload, _ := json.Marshal(map[string]string{"ac": ac})
	return "Bearer x." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func twirp(t *testing.T, base, method, auth string, req any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(req)
	r, _ := http.NewRequest(http.MethodPost, base+twirpPrefix+method, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("%s: status %d: %v", method, resp.StatusCode, out)
	}
	return out
}

func put(t *testing.T, url string, body []byte) {
	t.Helper()
	r, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	r.Header.Set("x-ms-blob-type", "BlockBlob")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT %s: %d", url, resp.StatusCode)
	}
}

func TestV2RoundTripWithBlocks(t *testing.T) {
	_, ts, _ := newTestServer(t)
	auth := token(`[{"Scope":"refs/heads/main","Permission":3}]`)

	res := twirp(t, ts.URL, "GetCacheEntryDownloadURL", auth, map[string]any{"key": "deps-abc", "version": "v1"})
	if res["ok"] != false {
		t.Fatalf("expected miss, got %v", res)
	}
	res = twirp(t, ts.URL, "CreateCacheEntry", auth, map[string]any{"key": "deps-abc", "version": "v1"})
	if res["ok"] != true {
		t.Fatalf("create failed: %v", res)
	}
	up := res["signed_upload_url"].(string)

	data := make([]byte, 10_000)
	rand.Read(data)
	// Upload out of order, commit in order, like the Azure SDK does.
	ids := []string{"YmxvY2sx", "YmxvY2sy", "YmxvY2sz"}
	chunks := [][]byte{data[:4000], data[4000:8000], data[8000:]}
	for _, i := range []int{2, 0, 1} {
		put(t, up+"?comp=block&blockid="+ids[i], chunks[i])
	}
	list := `<?xml version="1.0" encoding="utf-8"?><BlockList><Latest>` + ids[0] + `</Latest><Latest>` + ids[1] + `</Latest><Latest>` + ids[2] + `</Latest></BlockList>`
	put(t, up+"?comp=blocklist", []byte(list))

	res = twirp(t, ts.URL, "FinalizeCacheEntryUpload", auth, map[string]any{"key": "deps-abc", "version": "v1", "size_bytes": "10000"})
	if res["ok"] != true {
		t.Fatalf("finalize failed: %v", res)
	}
	// Second create on the same key is refused.
	res = twirp(t, ts.URL, "CreateCacheEntry", auth, map[string]any{"key": "deps-abc", "version": "v1"})
	if res["ok"] != false {
		t.Fatalf("expected duplicate create to fail: %v", res)
	}

	res = twirp(t, ts.URL, "GetCacheEntryDownloadURL", auth, map[string]any{"key": "deps-zzz", "restore_keys": []string{"deps-"}, "version": "v1"})
	if res["ok"] != true || res["matched_key"] != "deps-abc" {
		t.Fatalf("expected restore-key hit: %v", res)
	}
	resp, err := http.Get(res["signed_download_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, data) {
		t.Fatalf("download mismatch: got %d bytes", len(got))
	}
	if resp.Header.Get("Content-Length") != "10000" {
		t.Fatalf("content-length = %q", resp.Header.Get("Content-Length"))
	}
}

func TestBranchScopes(t *testing.T) {
	_, ts, _ := newTestServer(t)
	main := token(`[{"Scope":"refs/heads/main","Permission":3}]`)
	pr := token(`[{"Scope":"refs/pull/7/merge","Permission":3},{"Scope":"refs/heads/main","Permission":1}]`)
	other := token(`[{"Scope":"refs/heads/feature","Permission":3}]`)

	res := twirp(t, ts.URL, "CreateCacheEntry", main, map[string]any{"key": "k", "version": "v"})
	put(t, res["signed_upload_url"].(string), []byte("main-data"))
	twirp(t, ts.URL, "FinalizeCacheEntryUpload", main, map[string]any{"key": "k", "version": "v", "size_bytes": 9})

	if res := twirp(t, ts.URL, "GetCacheEntryDownloadURL", pr, map[string]any{"key": "k", "version": "v"}); res["ok"] != true {
		t.Fatalf("PR should read the base branch cache: %v", res)
	}
	if res := twirp(t, ts.URL, "GetCacheEntryDownloadURL", other, map[string]any{"key": "k", "version": "v"}); res["ok"] != false {
		t.Fatalf("unrelated branch must not see main's cache: %v", res)
	}
	// Version isolation.
	if res := twirp(t, ts.URL, "GetCacheEntryDownloadURL", main, map[string]any{"key": "k", "version": "other"}); res["ok"] != false {
		t.Fatalf("different version must miss: %v", res)
	}
}

func TestRestoreKeyPicksNewest(t *testing.T) {
	_, ts, store := newTestServer(t)
	auth := token(`[{"Scope":"refs/heads/main","Permission":3}]`)
	now := time.Now()
	for i, k := range []string{"npm-1", "npm-3", "npm-2"} {
		store.Now = func() time.Time { return now.Add(time.Duration(i) * time.Minute) }
		res := twirp(t, ts.URL, "CreateCacheEntry", auth, map[string]any{"key": k, "version": "v"})
		put(t, res["signed_upload_url"].(string), []byte(k))
	}
	res := twirp(t, ts.URL, "GetCacheEntryDownloadURL", auth, map[string]any{"key": "npm-x", "restore_keys": []string{"npm-"}, "version": "v"})
	if res["matched_key"] != "npm-2" {
		t.Fatalf("expected newest entry npm-2, got %v", res["matched_key"])
	}
}

func TestProtobufTwirp(t *testing.T) {
	_, ts, _ := newTestServer(t)
	w := &pbWriter{}
	w.str(2, "proto-key")
	w.str(3, "v9")
	r, _ := http.NewRequest(http.MethodPost, ts.URL+twirpPrefix+"CreateCacheEntry", bytes.NewReader(w.b))
	r.Header.Set("Content-Type", "application/protobuf")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m, err := pbDecode(body)
	if err != nil {
		t.Fatal(err)
	}
	if pbInt(m, 1) != 1 || !strings.Contains(pbString(m, 2), "/upload/") {
		t.Fatalf("unexpected protobuf response: %v", m)
	}
}

func TestV1Flow(t *testing.T) {
	_, ts, _ := newTestServer(t)
	auth := token(`[{"Scope":"refs/heads/main","Permission":3}]`)
	do := func(method, path string, body []byte, hdr map[string]string) *http.Response {
		r, _ := http.NewRequest(method, ts.URL+"/_apis/artifactcache/"+path, bytes.NewReader(body))
		r.Header.Set("Authorization", auth)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := do(http.MethodGet, "cache?keys=v1key&version=abc", nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}
	resp = do(http.MethodPost, "caches", []byte(`{"key":"v1key","version":"abc"}`), nil)
	var created struct{ CacheID int64 }
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	data := []byte("hello legacy cache world")
	do(http.MethodPatch, fmt.Sprintf("caches/%d", created.CacheID), data[10:], map[string]string{"Content-Range": fmt.Sprintf("bytes 10-%d/*", len(data)-1)}).Body.Close()
	do(http.MethodPatch, fmt.Sprintf("caches/%d", created.CacheID), data[:10], map[string]string{"Content-Range": "bytes 0-9/*"}).Body.Close()
	if r := do(http.MethodPost, fmt.Sprintf("caches/%d", created.CacheID), []byte(`{"size":24}`), nil); r.StatusCode != http.StatusNoContent {
		t.Fatalf("commit status %d", r.StatusCode)
	}
	resp = do(http.MethodGet, "cache?keys=v1key&version=abc", nil, nil)
	var hit struct{ ArchiveLocation, CacheKey string }
	json.NewDecoder(resp.Body).Decode(&hit)
	resp.Body.Close()
	dl, _ := http.Get(hit.ArchiveLocation)
	got, _ := io.ReadAll(dl.Body)
	dl.Body.Close()
	if string(got) != string(data) {
		t.Fatalf("got %q", got)
	}
}

func TestRangedDownloads(t *testing.T) {
	_, ts, _ := newTestServer(t)
	auth := token(`[{"Scope":"refs/heads/main","Permission":3}]`)
	res := twirp(t, ts.URL, "CreateCacheEntry", auth, map[string]any{"key": "r", "version": "v"})
	data := make([]byte, 5000)
	rand.Read(data)
	put(t, res["signed_upload_url"].(string), data)
	dl := twirp(t, ts.URL, "GetCacheEntryDownloadURL", auth, map[string]any{"key": "r", "version": "v"})["signed_download_url"].(string)
	for _, hdr := range []string{"Range", "x-ms-range"} {
		req, _ := http.NewRequest(http.MethodGet, dl, nil)
		req.Header.Set(hdr, "bytes=1234-")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(got, data[1234:]) {
			t.Fatalf("%s: status %d, %d bytes", hdr, resp.StatusCode, len(got))
		}
	}
}

func TestParseRange(t *testing.T) {
	cases := []struct {
		h    string
		a, b int64
		ok   bool
	}{
		{"bytes=0-9", 0, 9, true},
		{"bytes=10-", 10, 99, true},
		{"bytes=-10", 90, 99, true},
		{"bytes=50-500", 50, 99, true},
		{"bytes=100-", 0, 0, false},
		{"items=0-1", 0, 0, false},
	}
	for _, c := range cases {
		a, b, ok := parseRange(c.h, 100)
		if ok != c.ok || (ok && (a != c.a || b != c.b)) {
			t.Errorf("%s: got %d-%d %v", c.h, a, b, ok)
		}
	}
}

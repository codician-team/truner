// Package cachestore is the object storage behind the truner magic cache.
package cachestore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// ErrNotFound is returned when an object does not exist.
var ErrNotFound = errors.New("cachestore: not found")

// Object describes a stored object.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// Store is the minimal object store interface used by the cache.
type Store interface {
	Head(ctx context.Context, key string) (*Object, error)
	List(ctx context.Context, prefix string) ([]Object, error)
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	GetRange(ctx context.Context, key string, off, length int64) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

// S3 is a Store backed by an S3 bucket.
type S3 struct {
	Client   *s3.Client
	Bucket   string
	uploader *manager.Uploader
}

// NewS3 returns an S3 store tuned for large cache archives.
func NewS3(client *s3.Client, bucket string) *S3 {
	return &S3{
		Client: client,
		Bucket: bucket,
		uploader: manager.NewUploader(client, func(u *manager.Uploader) {
			u.PartSize = 32 << 20
			u.Concurrency = 8
		}),
	}
}

// Head implements Store.
func (s *S3) Head(ctx context.Context, key string) (*Object, error) {
	out, err := s.Client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.Bucket, Key: &key})
	if err != nil {
		var nf *s3types.NotFound
		var nsk *s3types.NoSuchKey
		if errors.As(err, &nf) || errors.As(err, &nsk) || strings.Contains(err.Error(), "StatusCode: 404") {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &Object{Key: key, Size: aws.ToInt64(out.ContentLength), LastModified: aws.ToTime(out.LastModified)}, nil
}

// List implements Store.
func (s *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	p := s3.NewListObjectsV2Paginator(s.Client, &s3.ListObjectsV2Input{Bucket: &s.Bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			out = append(out, Object{Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size), LastModified: aws.ToTime(o.LastModified)})
		}
	}
	return out, nil
}

// Put implements Store.
func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	_, err := s.uploader.Upload(ctx, &s3.PutObjectInput{Bucket: &s.Bucket, Key: &key, Body: r, ContentType: aws.String("application/octet-stream")})
	return err
}

// GetRange implements Store. length < 0 reads to the end.
func (s *S3) GetRange(ctx context.Context, key string, off, length int64) (io.ReadCloser, error) {
	in := &s3.GetObjectInput{Bucket: &s.Bucket, Key: &key}
	if off > 0 || length >= 0 {
		if length < 0 {
			in.Range = aws.String(fmt.Sprintf("bytes=%d-", off))
		} else {
			in.Range = aws.String(fmt.Sprintf("bytes=%d-%d", off, off+length-1))
		}
	}
	out, err := s.Client.GetObject(ctx, in)
	if err != nil {
		var nsk *s3types.NoSuchKey
		if errors.As(err, &nsk) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return out.Body, nil
}

// Delete implements Store.
func (s *S3) Delete(ctx context.Context, key string) error {
	_, err := s.Client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.Bucket, Key: &key})
	return err
}

// Memory is an in-memory Store for tests.
type Memory struct {
	mu   sync.Mutex
	objs map[string]memObj
	Now  func() time.Time
}

type memObj struct {
	data []byte
	mod  time.Time
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory { return &Memory{objs: map[string]memObj{}, Now: time.Now} }

// Head implements Store.
func (m *Memory) Head(_ context.Context, key string) (*Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[key]
	if !ok {
		return nil, ErrNotFound
	}
	return &Object{Key: key, Size: int64(len(o.data)), LastModified: o.mod}, nil
}

// List implements Store.
func (m *Memory) List(_ context.Context, prefix string) ([]Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Object
	for k, o := range m.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, Object{Key: k, Size: int64(len(o.data)), LastModified: o.mod})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Put implements Store.
func (m *Memory) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = memObj{data: data, mod: m.Now()}
	return nil
}

// GetRange implements Store.
func (m *Memory) GetRange(_ context.Context, key string, off, length int64) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[key]
	if !ok {
		return nil, ErrNotFound
	}
	if off > int64(len(o.data)) {
		off = int64(len(o.data))
	}
	end := int64(len(o.data))
	if length >= 0 && off+length < end {
		end = off + length
	}
	return io.NopCloser(bytes.NewReader(o.data[off:end])), nil
}

// Delete implements Store.
func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	return nil
}

// Layout: <prefix><scope>/<version>/<key>, with scope and key path-escaped so
// that prefix matching on escaped keys equals prefix matching on raw keys.

// ObjectKey builds the storage key of a cache entry.
func ObjectKey(prefix, scope, version, key string) string {
	return prefix + url.PathEscape(scope) + "/" + version + "/" + url.PathEscape(key)
}

// ScopePrefix is the storage prefix of a scope+version pair.
func ScopePrefix(prefix, scope, version string) string {
	return prefix + url.PathEscape(scope) + "/" + version + "/"
}

// Entry is a parsed cache entry.
type Entry struct {
	Repo         string // owner/repo (when prefix is cache/)
	Scope        string
	Version      string
	Key          string
	Size         int64
	LastModified time.Time
	ObjectKey    string
}

// ParseObjectKey splits a storage key below prefix back into its parts.
// repoDepth is the number of path segments in prefix after "cache/".
func ParseObjectKey(prefix, objKey string) (scope, version, key string, ok bool) {
	rest, found := strings.CutPrefix(objKey, prefix)
	if !found {
		return "", "", "", false
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	s, err1 := url.PathUnescape(parts[0])
	k, err2 := url.PathUnescape(parts[2])
	if err1 != nil || err2 != nil {
		return "", "", "", false
	}
	return s, parts[1], k, true
}

// RepoPrefix returns the cache prefix of a repository.
func RepoPrefix(owner, repo string) string {
	return "cache/" + strings.ToLower(owner) + "/" + strings.ToLower(repo) + "/"
}

// ListEntries lists cache entries under "cache/", optionally for one repo ("owner/repo").
func ListEntries(ctx context.Context, st Store, repo string) ([]Entry, error) {
	prefix := "cache/"
	if repo != "" {
		owner, name, _ := strings.Cut(repo, "/")
		prefix = RepoPrefix(owner, name)
	}
	objs, err := st.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, o := range objs {
		parts := strings.SplitN(strings.TrimPrefix(o.Key, "cache/"), "/", 3)
		if len(parts) != 3 {
			continue
		}
		rp := RepoPrefix(parts[0], parts[1])
		scope, version, key, ok := ParseObjectKey(rp, o.Key)
		if !ok {
			continue
		}
		out = append(out, Entry{Repo: parts[0] + "/" + parts[1], Scope: scope, Version: version, Key: key, Size: o.Size, LastModified: o.LastModified, ObjectKey: o.Key})
	}
	return out, nil
}

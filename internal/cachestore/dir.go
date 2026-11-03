package cachestore

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Dir is a Store backed by a local directory. It is used for local testing
// of the cache proxy (`truner-agent cache-proxy --store-dir`).
type Dir struct{ Root string }

// Escaped keys never contain a raw '!' or '^' (url.PathEscape escapes both),
// so they can stand in for characters that are invalid in Windows file names.
var dirEscaper = strings.NewReplacer("%", "!", ":", "^")
var dirUnescaper = strings.NewReplacer("!", "%", "^", ":")

func (d Dir) path(key string) string {
	return filepath.Join(d.Root, filepath.FromSlash(dirEscaper.Replace(key)))
}

func (d Dir) key(p string) string {
	rel, _ := filepath.Rel(d.Root, p)
	return dirUnescaper.Replace(filepath.ToSlash(rel))
}

// Head implements Store.
func (d Dir) Head(_ context.Context, key string) (*Object, error) {
	st, err := os.Stat(d.path(key))
	if err != nil {
		return nil, ErrNotFound
	}
	return &Object{Key: key, Size: st.Size(), LastModified: st.ModTime()}, nil
}

// List implements Store.
func (d Dir) List(_ context.Context, prefix string) ([]Object, error) {
	var out []Object
	err := filepath.WalkDir(d.Root, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil
		}
		k := d.key(p)
		if strings.HasPrefix(k, prefix) && !strings.HasSuffix(k, ".tmp") {
			if info, err := e.Info(); err == nil {
				out = append(out, Object{Key: k, Size: info.Size(), LastModified: info.ModTime()})
			}
		}
		return nil
	})
	return out, err
}

// Put implements Store.
func (d Dir) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	p := d.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.Create(p + ".tmp")
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(p + ".tmp")
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

// GetRange implements Store.
func (d Dir) GetRange(_ context.Context, key string, off, length int64) (io.ReadCloser, error) {
	f, err := os.Open(d.path(key))
	if err != nil {
		return nil, ErrNotFound
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	if length < 0 {
		return f, nil
	}
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(f, length), f}, nil
}

// Delete implements Store.
func (d Dir) Delete(_ context.Context, key string) error {
	return os.Remove(d.path(key))
}

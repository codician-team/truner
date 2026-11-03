package agent

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Speeding up the start of a job on a machine launched from a baked image.
//
// EBS restores a custom AMI's volume lazily: every block is fetched from S3
// the first time it is read, which makes the first boot of a baked image far
// slower than its software suggests. Three things help: starting the agent
// from its own unit as soon as the network is up (see bootUnit), not walking
// the whole runner directory before every job (osFinalizeFiles), and reading
// the runner's files in parallel while the machine is being prepared, so the
// runner starts from the page cache instead of fetching block by block.

// hddInstanceType reports whether an instance type's local storage is hard
// disks (d2, d3, d3en, h1). Setting those up takes minutes and builds on them
// are slower than on EBS, so they are never used for the work directory.
func hddInstanceType(t string) bool {
	family, _, _ := strings.Cut(t, ".")
	switch family {
	case "d2", "d3", "d3en", "h1":
		return true
	}
	return false
}

// Lock makes sure only one agent runs on a machine. On baked images both the
// boot unit and the user data script start one; the second exits.
func Lock() bool { return osLock() }

// prefetch reads the files under dirs with several readers, in order, and
// returns when done. Errors are ignored: this only warms the page cache.
func prefetch(dirs []string, workers int) (files int64, bytes int64, took time.Duration) {
	start := time.Now()
	paths := make(chan string, 256)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 256<<10)
			for p := range paths {
				f, err := os.Open(p)
				if err != nil {
					continue
				}
				n, _ := io.CopyBuffer(io.Discard, f, buf)
				f.Close()
				atomic.AddInt64(&files, 1)
				atomic.AddInt64(&bytes, n)
			}
		}()
	}
	for _, d := range dirs {
		filepath.WalkDir(d, func(p string, e fs.DirEntry, err error) error {
			if err == nil && e.Type().IsRegular() {
				paths <- p
			}
			return nil
		})
	}
	close(paths)
	wg.Wait()
	return files, bytes, time.Since(start)
}

// warmRunner prefetches the runner's binaries, then the Node.js executable
// that JavaScript actions run on, while the rest of the machine is being
// prepared. The other bundled Node.js builds (older and Alpine) are rarely
// used and would only compete with the job for the volume.
func (a *Agent) warmRunner(dir string) {
	if _, err := os.Stat(filepath.Join(dir, "bin")); err != nil {
		return // not installed yet: the download leaves it in the page cache anyway
	}
	go func() {
		paths := []string{filepath.Join(dir, "bin")}
		if node := newestNode(filepath.Join(dir, "externals")); node != "" {
			paths = append(paths, filepath.Join(node, "bin", "node"))
		}
		n, b, took := prefetch(paths, 16)
		a.Log.Info("prefetched runner files", "files", n, "mib", b>>20, "took", took.Round(time.Millisecond))
	}()
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// newestNode returns the newest regular Node.js the runner bundles, such as
// externals/node24 next to node20, node20_alpine and node24_alpine.
func newestNode(externals string) string {
	dirs, _ := filepath.Glob(filepath.Join(externals, "node*"))
	best, bestV := "", -1
	for _, d := range dirs {
		v, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(d), "node"))
		if err == nil && v > bestV {
			best, bestV = d, v
		}
	}
	return best
}

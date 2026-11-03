package agent

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
)

// logWriter turns a command's output into log lines.
type logWriter struct {
	log    *slog.Logger
	prefix string
	mu     sync.Mutex
	buf    bytes.Buffer
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			w.buf.WriteString(line)
			break
		}
		w.emit(strings.TrimRight(line, "\r\n"))
	}
	return len(p), nil
}

func (w *logWriter) emit(line string) {
	if line == "" {
		return
	}
	if strings.Contains(line, "::truner-warning::") {
		w.log.Warn(w.prefix + line)
		return
	}
	w.log.Info(w.prefix + line)
}

// Flush writes out any partial last line.
func (w *logWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() > 0 {
		w.emit(w.buf.String())
		w.buf.Reset()
	}
}

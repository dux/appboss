package supervisor

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// logWriter is the daemon-owned sink for one process's stdout and stderr. Because dboss, not the
// child, holds the file, it can seal the current segment for ingestion and open a fresh one
// without the process noticing. Writes are serialized because os/exec copies stdout and stderr
// through two pipes into the same writer.
type logWriter struct {
	mu      sync.Mutex
	path    string
	file    *os.File
	maximum int64
	keep    int
	echo    *echoWriter
}

func newLogWriter(path string, maximum int64, keep int) (*logWriter, error) {
	w := &logWriter{path: path, maximum: maximum, keep: keep}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *logWriter) open() error {
	if err := os.MkdirAll(filepath.Dir(w.path), 0o750); err != nil {
		return err
	}
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	w.file = file
	return nil
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.maximum > 0 {
		if info, err := w.file.Stat(); err == nil && info.Size() >= w.maximum {
			w.rotate()
		}
	}
	n, err := w.file.Write(p)
	if w.echo != nil {
		_, _ = w.echo.Write(p)
	}
	return n, err
}

// rotate shifts the size archives (.1, .2, ...) and truncates the live file, keeping the fd the
// process already writes through. keep <= 0 keeps no archives: the file is truncated in place, so
// log_max_size still bounds it.
func (w *logWriter) rotate() {
	if w.keep <= 0 {
		_ = w.file.Truncate(0)
		return
	}
	shiftRotatedLogs(w.path, w.keep)
	data, err := os.ReadFile(w.path)
	if err != nil {
		return
	}
	_ = os.WriteFile(w.path+".1", data, 0o640)
	_ = w.file.Truncate(0)
}

// Seal closes the current segment, renames it aside and reopens a fresh file, returning the
// sealed path for the ingestion module. An empty segment returns "" and changes nothing.
func (w *logWriter) Seal() (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	info, err := w.file.Stat()
	if err != nil || info.Size() == 0 {
		return "", err
	}
	sealed := fmt.Sprintf("%s.%d.sealed", w.path, time.Now().UnixNano())
	if err := w.file.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(w.path, sealed); err != nil {
		_ = w.open()
		return "", err
	}
	if err := w.open(); err != nil {
		return "", err
	}
	return sealed, nil
}

func (w *logWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.echo != nil {
		w.echo.flush()
	}
	return w.file.Close()
}

var _ io.WriteCloser = (*logWriter)(nil)

func shiftRotatedLogs(path string, keep int) {
	_ = os.Remove(fmt.Sprintf("%s.%d", path, keep))
	for i := keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", path, i), fmt.Sprintf("%s.%d", path, i+1))
	}
}

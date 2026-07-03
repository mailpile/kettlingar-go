package logging

import (
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// rotatingWriter is an io.WriteCloser that writes to a file and rotates it when
// it would exceed MaxSizeMB: the current file becomes name.1 (gzipped to
// name.1.gz when Compress is set), older backups shift up to MaxBackups, and any
// beyond that or older than MaxAgeDays are removed. It avoids a third-party
// dependency; all access is serialized by mu.
type rotatingWriter struct {
	mu       sync.Mutex
	cfg      FileConfig
	maxBytes int64
	file     *os.File
	size     int64
}

func newRotatingWriter(cfg FileConfig) (*rotatingWriter, error) {
	w := &rotatingWriter{cfg: cfg, maxBytes: int64(cfg.MaxSizeMB) * 1024 * 1024}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

// open (re)opens the log file for appending, seeding size from its current
// length. The directory is created if missing.
func (w *rotatingWriter) open() error {
	if dir := filepath.Dir(w.cfg.Path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(w.cfg.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	w.file = f
	w.size = 0
	if fi, err := f.Stat(); err == nil {
		w.size = fi.Size()
	}
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.maxBytes > 0 && w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// rotate closes the current file, shifts the numbered backups up, moves the
// current file into slot 1 (compressing if configured), prunes by count/age and
// reopens a fresh file. Callers hold mu.
func (w *rotatingWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	// Shift name.(N-1) -> name.N ... name.1 -> name.2, dropping the one that would
	// exceed MaxBackups.
	if w.cfg.MaxBackups > 0 {
		os.Remove(w.backupName(w.cfg.MaxBackups))
		for i := w.cfg.MaxBackups - 1; i >= 1; i-- {
			_ = os.Rename(w.backupName(i), w.backupName(i+1))
		}
	}
	// Current file -> slot 1.
	if w.cfg.Compress {
		tmp := fmt.Sprintf("%s.1", w.cfg.Path)
		if err := os.Rename(w.cfg.Path, tmp); err == nil {
			if err := gzipFile(tmp, w.backupName(1)); err == nil {
				os.Remove(tmp)
			}
		}
	} else {
		_ = os.Rename(w.cfg.Path, w.backupName(1))
	}
	w.pruneAge()
	return w.open()
}

// backupName is the filename for the i-th rotated backup (name.i, or name.i.gz
// when compression is on).
func (w *rotatingWriter) backupName(i int) string {
	name := fmt.Sprintf("%s.%d", w.cfg.Path, i)
	if w.cfg.Compress {
		name += ".gz"
	}
	return name
}

// pruneAge removes rotated backups older than MaxAgeDays. Callers hold mu.
func (w *rotatingWriter) pruneAge() {
	if w.cfg.MaxAgeDays <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(w.cfg.MaxAgeDays) * 24 * time.Hour)
	prefix := filepath.Base(w.cfg.Path) + "."
	entries, err := os.ReadDir(filepath.Dir(w.cfg.Path))
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(filepath.Dir(w.cfg.Path), e.Name()))
		}
	}
}

func gzipFile(src, dst string) error {
	in, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	if _, err := zw.Write(in); err != nil {
		zw.Close()
		f.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// backupsFor lists existing backup files for path, oldest-numbered last; used by
// tests. Not part of the hot path.
func backupsFor(path string) []string {
	var out []string
	dir := filepath.Dir(path)
	prefix := filepath.Base(path) + "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// Package logfile provides a size-rotating io.Writer: when the active file
// would exceed MaxSize it is renamed to name.1 (shifting older backups up)
// and a fresh file is started.
package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Defaults from SPEC-001.
const (
	DefaultMaxSize = 10 << 20
	DefaultBackups = 3
)

// Writer is safe for concurrent use.
type Writer struct {
	path    string
	maxSize int64
	backups int

	mu   sync.Mutex
	f    *os.File
	size int64
}

// Open opens (or creates) path for appending, creating parent directories.
// maxSize <= 0 and backups <= 0 select the defaults.
func Open(path string, maxSize int64, backups int) (*Writer, error) {
	if maxSize <= 0 {
		maxSize = DefaultMaxSize
	}
	if backups <= 0 {
		backups = DefaultBackups
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	w := &Writer{path: path, maxSize: maxSize, backups: backups}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.size = f, info.Size()
	return nil
}

// Write appends p, rotating first if p would push the file past MaxSize.
// A single write larger than MaxSize is written whole to a fresh file.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, os.ErrClosed
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxSize {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *Writer) rotate() error {
	if err := w.f.Close(); err != nil {
		return err
	}
	// name.(n-1) → name.n, …, name → name.1. The oldest is overwritten.
	for i := w.backups - 1; i >= 1; i-- {
		_ = os.Rename(Backup(w.path, i), Backup(w.path, i+1))
	}
	if err := os.Rename(w.path, Backup(w.path, 1)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return w.open()
}

// Close closes the active file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// Backup returns the path of backup number n (1 is the most recent).
func Backup(path string, n int) string { return fmt.Sprintf("%s.%d", path, n) }

// Files returns the existing log files for path, oldest first, so they can
// be read in chronological order.
func Files(path string, backups int) []string {
	if backups <= 0 {
		backups = DefaultBackups
	}
	var out []string
	for i := backups; i >= 1; i-- {
		if _, err := os.Stat(Backup(path, i)); err == nil {
			out = append(out, Backup(path, i))
		}
	}
	if _, err := os.Stat(path); err == nil {
		out = append(out, path)
	}
	return out
}

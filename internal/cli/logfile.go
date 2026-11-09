package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// The daemon's log file (DECISIONS D48): appended to, and when it reaches
// maxLogSize it is renamed to "<name>.1" (replacing an older one) and a new
// file is started — so at most about twice maxLogSize is ever used.

const maxLogSize = 10 << 20

// logFile is an io.Writer safe for concurrent use (slog handlers call
// Write from many goroutines).
type logFile struct {
	path string
	max  int64

	mu   sync.Mutex
	f    *os.File
	size int64
}

func openLogFile(path string, max int64) (*logFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("log file: %w", err)
	}
	l := &logFile{path: path, max: max}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *logFile) open() error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("log file: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("log file: %w", err)
	}
	l.f, l.size = f, fi.Size()
	return nil
}

func (l *logFile) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return 0, os.ErrClosed
	}
	if l.size > 0 && l.size+int64(len(p)) > l.max {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// rotate moves the current file to ".1" and starts a new one. Windows
// can't rename an open file or over an existing one, hence close and
// remove first.
func (l *logFile) rotate() error {
	l.f.Close()
	old := l.path + ".1"
	os.Remove(old)
	if err := os.Rename(l.path, old); err != nil {
		l.f = nil
		if oerr := l.open(); oerr != nil { // keep logging somewhere if we can
			return oerr
		}
		return nil
	}
	return l.open()
}

func (l *logFile) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

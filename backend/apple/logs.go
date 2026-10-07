package apple

import (
	"context"
	"io"
	"os"
	"sync"
	"time"
)

// A spool avoids blocking startup before Woodpecker calls TailStep, preserves
// early output, and lets the reader reach EOF without a WaitStep call.
type logSpool struct {
	mu   sync.Mutex
	file *os.File
}

func (s *logSpool) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.Write(p)
}
func (s *logSpool) close() error { s.mu.Lock(); defer s.mu.Unlock(); return s.file.Close() }

type logReader struct {
	file   *os.File
	done   <-chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
}

func (r *logReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		n, err := r.file.Read(p)
		if n > 0 || err != io.EOF {
			return n, err
		}
		select {
		case <-r.done:
			// Re-read after observing completion: the last write may have raced
			// the first EOF, but no more output can arrive now.
			return r.file.Read(p)
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (r *logReader) Close() error { r.cancel(); return r.file.Close() }

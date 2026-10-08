package jobs

import (
	"fmt"
	"io"
	"os"
	"sync"
)

const maxJobLogBytes int64 = 2 * 1024 * 1024

type rollingLog struct {
	mu sync.Mutex
	f  *os.File
}

func openRollingLog(path string) (*rollingLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return &rollingLog{f: f}, nil
}

func (l *rollingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	original := len(p)
	if int64(len(p)) > maxJobLogBytes/2 {
		p = p[len(p)-int(maxJobLogBytes/2):]
	}
	info, err := l.f.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size()+int64(len(p)) > maxJobLogBytes {
		keep := maxJobLogBytes / 2
		if info.Size() < keep {
			keep = info.Size()
		}
		tail := make([]byte, keep)
		if keep > 0 {
			if _, err := l.f.ReadAt(tail, info.Size()-keep); err != nil && err != io.EOF {
				return 0, err
			}
		}
		if err := l.f.Truncate(0); err != nil {
			return 0, err
		}
		if _, err := l.f.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		if _, err := fmt.Fprintln(l.f, "[older job output truncated by rurushu]"); err != nil {
			return 0, err
		}
		if len(tail) > 0 {
			if _, err := l.f.Write(tail); err != nil {
				return 0, err
			}
		}
	}
	if _, err := l.f.Write(p); err != nil {
		return 0, err
	}
	return original, nil
}

func (l *rollingLog) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	return l.f.Close()
}

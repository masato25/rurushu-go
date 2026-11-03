package projectstate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// HoldSession reserves the interactive session for a single process. The lock
// remains held until ReleaseSession or process exit (including exec for reload).
// It is distinct from the short-lived writer lock.
func (s *Store) HoldSession(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validSessionID(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	if s.forceSessionOwnership {
		return nil
	}
	if s.ownerID == id && s.ownerFile != nil {
		return nil
	}
	path := filepath.Join(s.Dir, "sessions", id+".owner.lock")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe session ownership file %s", id)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("session %s is already open in another process", id)
		}
		return err
	}
	// Acquire the new lease before releasing the previous one.
	previous := s.ownerFile
	s.ownerFile, s.ownerID = f, id
	if previous != nil {
		_ = syscall.Flock(int(previous.Fd()), syscall.LOCK_UN)
		_ = previous.Close()
	}
	return nil
}

// SetForceSessionOwnership bypasses interactive session ownership without
// disabling the short-lived per-save writer lock.
func (s *Store) SetForceSessionOwnership(force bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forceSessionOwnership = force
}

func (s *Store) ReleaseSession() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ownerFile != nil {
		_ = syscall.Flock(int(s.ownerFile.Fd()), syscall.LOCK_UN)
		_ = s.ownerFile.Close()
	}
	s.ownerFile, s.ownerID = nil, ""
}

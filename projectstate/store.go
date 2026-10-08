package projectstate

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const Version = 1

type State struct {
	Version     int    `json:"version"`
	ProjectID   string `json:"project_id"`
	LastSession string `json:"last_session,omitempty"`
}

type Session struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Model     string    `json:"model,omitempty"`
	Provider  string    `json:"provider,omitempty"`
}

type Store struct {
	ProjectRoot string
	Dir         string
}

func Open(projectRoot string) (*Store, error) {
	root, err := filepath.Abs(strings.TrimSpace(projectRoot))
	if err != nil {
		return nil, fmt.Errorf("resolve project root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve project root symlinks: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("stat project root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("project root is not a directory")
	}

	dir := filepath.Join(root, ".rurushu")
	for _, path := range []string{dir, filepath.Join(dir, "sessions"), filepath.Join(dir, "jobs")} {
		if info, lstatErr := os.Lstat(path); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("project state path %s must not be a symlink", path)
		} else if lstatErr != nil && !os.IsNotExist(lstatErr) {
			return nil, fmt.Errorf("inspect project state path %s: %w", path, lstatErr)
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", path, err)
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			if err == nil {
				err = fmt.Errorf("not a real directory")
			}
			return nil, fmt.Errorf("validate project state path %s: %w", path, err)
		}
		_ = os.Chmod(path, 0o700)
	}
	ignorePath := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(ignorePath); os.IsNotExist(err) {
		// Ignore the entire local state directory, including this ignore file,
		// without modifying the repository's root .gitignore.
		if err := os.WriteFile(ignorePath, []byte("*\n"), 0o600); err != nil {
			return nil, fmt.Errorf("write .rurushu/.gitignore: %w", err)
		}
	}

	store := &Store{ProjectRoot: root, Dir: dir}
	if _, err := store.ensureState(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) StartSession(model, provider string) (Session, error) {
	now := time.Now()
	id, err := newID("ses")
	if err != nil {
		return Session{}, err
	}
	session := Session{
		ID: id, CreatedAt: now, UpdatedAt: now,
		Model: strings.TrimSpace(model), Provider: strings.TrimSpace(provider),
	}
	if err := writeJSONAtomic(filepath.Join(s.Dir, "sessions", id+".json"), session); err != nil {
		return Session{}, err
	}
	state, err := s.ensureState()
	if err != nil {
		return Session{}, err
	}
	state.LastSession = id
	if err := writeJSONAtomic(filepath.Join(s.Dir, "state.json"), state); err != nil {
		return Session{}, err
	}
	return session, nil
}

func (s *Store) State() (State, error) { return s.ensureState() }

func (s *Store) ensureState() (State, error) {
	path := filepath.Join(s.Dir, "state.json")
	data, err := os.ReadFile(path)
	if err == nil {
		var state State
		if err := json.Unmarshal(data, &state); err != nil {
			return State{}, fmt.Errorf("decode project state: %w", err)
		}
		if state.Version <= 0 {
			state.Version = Version
		}
		if state.ProjectID == "" {
			state.ProjectID, err = newID("prj")
			if err != nil {
				return State{}, err
			}
			if err := writeJSONAtomic(path, state); err != nil {
				return State{}, err
			}
		}
		return state, nil
	}
	if !os.IsNotExist(err) {
		return State{}, fmt.Errorf("read project state: %w", err)
	}
	id, err := newID("prj")
	if err != nil {
		return State{}, err
	}
	state := State{Version: Version, ProjectID: id}
	if err := writeJSONAtomic(path, state); err != nil {
		return State{}, err
	}
	return state, nil
}

func newID(prefix string) (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(raw[:]), nil
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".rurushu-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", filepath.Base(path), err)
	}
	return nil
}

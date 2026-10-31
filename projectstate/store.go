package projectstate

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/masato25/rurushu-go/provider"
)

const Version = 1
const SessionVersion = 1
const maxSessionBytes int64 = 16 * 1024 * 1024
const sessionCompactJournalBytes int64 = 1 << 20

type State struct {
	Version     int    `json:"version"`
	ProjectID   string `json:"project_id"`
	LastSession string `json:"last_session,omitempty"`
}

type Session struct {
	Version        int                `json:"version"`
	ID             string             `json:"id"`
	ProjectID      string             `json:"project_id,omitempty"`
	ParentSession  string             `json:"parent_session,omitempty"`
	RewoundFromAsk int                `json:"rewound_from_ask,omitempty"`
	CreatedAt      time.Time          `json:"created_at"`
	UpdatedAt      time.Time          `json:"updated_at"`
	Model          string             `json:"model,omitempty"`
	Provider       string             `json:"provider,omitempty"`
	Messages       []provider.Message `json:"messages,omitempty"`
}

// Each complete newline-terminated record is a committed extension of the
// immutable JSON snapshot. Partial records left by a crash are ignored.
type sessionDelta struct {
	BaseHash  string             `json:"base_hash"`
	Previous  int                `json:"previous"`
	Messages  []provider.Message `json:"messages"`
	UpdatedAt time.Time          `json:"updated_at"`
	Model     string             `json:"model"`
	Provider  string             `json:"provider"`
}

type Store struct {
	ProjectRoot string
	Dir         string
	mu          sync.Mutex
	cache       map[string]sessionCache
	ownerFile   *os.File
	ownerID     string
}

type sessionCache struct {
	session        Session
	baseHash       string
	baseTime       time.Time
	baseSize       int64
	journalTime    time.Time
	journalSize    int64
	journalRecords int
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
	return s.startSession(model, provider, "", 0, nil)
}

func (s *Store) ForkSession(parentID string, rewoundFromAsk int, model, providerID string, messages []provider.Message) (Session, error) {
	if _, err := s.LoadSession(parentID); err != nil {
		return Session{}, err
	}
	if rewoundFromAsk <= 0 {
		return Session{}, fmt.Errorf("rewound ask must be positive")
	}
	return s.startSession(model, providerID, parentID, rewoundFromAsk, messages)
}

func (s *Store) startSession(model, providerID, parentID string, rewoundFromAsk int, messages []provider.Message) (Session, error) {
	now := time.Now()
	id, err := newID("ses")
	if err != nil {
		return Session{}, err
	}
	state, err := s.ensureState()
	if err != nil {
		return Session{}, err
	}
	session := Session{
		Version: SessionVersion, ID: id, ProjectID: state.ProjectID,
		ParentSession: parentID, RewoundFromAsk: rewoundFromAsk,
		CreatedAt: now, UpdatedAt: now,
		Model: strings.TrimSpace(model), Provider: strings.TrimSpace(providerID),
		Messages: clonePersistedMessages(messages),
	}
	if err := validateSessionSize(session); err != nil {
		return Session{}, err
	}
	if err := writeJSONAtomic(s.sessionPath(id), session); err != nil {
		return Session{}, err
	}
	state.LastSession = id
	if err := writeJSONAtomic(filepath.Join(s.Dir, "state.json"), state); err != nil {
		return Session{}, err
	}
	return session, nil
}

func (s *Store) LoadSession(id string) (Session, error) {
	id = strings.TrimSpace(id)
	if !validSessionID(id) {
		return Session{}, fmt.Errorf("invalid session id %q", id)
	}
	path := s.sessionPath(id)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return Session{}, fmt.Errorf("session %s not found", id)
	}
	if err != nil {
		return Session{}, fmt.Errorf("inspect session %s: %w", id, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Session{}, fmt.Errorf("session %s must be a regular file", id)
	}
	if info.Size() > maxSessionBytes {
		return Session{}, fmt.Errorf("session %s exceeds %d bytes", id, maxSessionBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Session{}, fmt.Errorf("read session %s: %w", id, err)
	}
	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return Session{}, fmt.Errorf("decode session %s: %w", id, err)
	}
	if session.ID != id {
		return Session{}, fmt.Errorf("session %s has mismatched id %q", id, session.ID)
	}
	if session.Version <= 0 {
		// Session files created before conversation persistence only contained
		// metadata. Treat them as v1 sessions with an empty history.
		session.Version = SessionVersion
	}
	if session.Version > SessionVersion {
		return Session{}, fmt.Errorf("session %s uses unsupported version %d", id, session.Version)
	}
	state, err := s.ensureState()
	if err != nil {
		return Session{}, err
	}
	if session.ProjectID != "" && session.ProjectID != state.ProjectID {
		return Session{}, fmt.Errorf("session %s belongs to a different project", id)
	}
	if err := s.loadSessionDeltas(id, data, &session); err != nil {
		return Session{}, err
	}
	session.Messages = cloneMessages(session.Messages)
	return session, nil
}

func (s *Store) ListSessions() ([]Session, error) {
	entries, err := os.ReadDir(filepath.Join(s.Dir, "sessions"))
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	sessions := make([]Session, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("session file %s must not be a symlink", entry.Name())
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !validSessionID(id) {
			continue
		}
		session, err := s.LoadSession(id)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].UpdatedAt.Equal(sessions[j].UpdatedAt) {
			return sessions[i].ID > sessions[j].ID
		}
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})
	return sessions, nil
}

func (s *Store) SaveSession(id, model, providerID string, messages []provider.Message) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockSession(id)
	if err != nil {
		return Session{}, err
	}
	defer unlock()
	entry, cached := s.cache[id]
	if cached && !s.cacheIsCurrent(id, entry) {
		cached = false
	}
	var session Session
	err = nil
	if cached {
		session = entry.session
	} else {
		session, err = s.LoadSession(id)
	}
	if err != nil {
		return Session{}, err
	}
	previous := clonePersistedMessages(session.Messages)
	session.Version = SessionVersion
	state, err := s.ensureState()
	if err != nil {
		return Session{}, err
	}
	session.ProjectID = state.ProjectID
	session.UpdatedAt = time.Now()
	session.Model = strings.TrimSpace(model)
	session.Provider = strings.TrimSpace(providerID)
	session.Messages = clonePersistedMessages(messages)
	// Append-only saves do not rewrite the entire conversation. If an older
	// message was edited, take a new snapshot instead.
	if len(session.Messages) > len(previous) && (len(previous) == 0 || reflect.DeepEqual(session.Messages[:len(previous)], previous)) {
		baseHash := entry.baseHash
		if !cached {
			base, err := os.ReadFile(s.sessionPath(id))
			if err != nil {
				return Session{}, err
			}
			baseHash = hashSessionSnapshot(base)
		}
		record := sessionDelta{BaseHash: baseHash, Previous: len(previous), Messages: session.Messages[len(previous):], UpdatedAt: session.UpdatedAt, Model: session.Model, Provider: session.Provider}
		if err := s.appendSessionDelta(id, record); err != nil {
			return Session{}, err
		}
		if err := s.setLastSession(id); err != nil {
			return Session{}, err
		}
		if err := s.maybeCompactSession(session); err != nil {
			return Session{}, err
		}
		s.rememberSession(session)
		return session, nil
	}
	// Encode once: the previous validation marshalled the entire history,
	// followed by a second full marshal for writing it to disk.
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return Session{}, fmt.Errorf("encode session %s: %w", id, err)
	}
	if int64(len(data)+1) > maxSessionBytes {
		return Session{}, fmt.Errorf("session %s exceeds %d bytes", id, maxSessionBytes)
	}
	if err := writeBytesAtomic(s.sessionPath(id), append(data, '\n')); err != nil {
		return Session{}, err
	}
	if err := s.setLastSession(id); err != nil {
		return Session{}, err
	}
	s.rememberSession(session)
	return session, nil
}

// Serialize writers across Store instances and processes. The lock file is
// never renamed or removed: replacing it would allow two different inodes
// to be locked simultaneously.
func (s *Store) lockSession(id string) (func(), error) {
	if !validSessionID(id) {
		return nil, fmt.Errorf("invalid session id %q", id)
	}
	path := filepath.Join(s.Dir, "sessions", id+".lock")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("unsafe session lock %s", id)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock session %s: %w", id, err)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

func (s *Store) cacheIsCurrent(id string, cached sessionCache) bool {
	base, err := os.Stat(s.sessionPath(id))
	if err != nil || base.Size() != cached.baseSize || !base.ModTime().Equal(cached.baseTime) {
		return false
	}
	journal, err := os.Stat(s.deltaPath(id))
	if os.IsNotExist(err) {
		return cached.journalSize == 0
	}
	return err == nil && journal.Size() == cached.journalSize && journal.ModTime().Equal(cached.journalTime)
}

func (s *Store) rememberSession(session Session) {
	info, err := os.Stat(s.sessionPath(session.ID))
	if err != nil {
		delete(s.cache, session.ID)
		return
	}
	hash := ""
	if old, ok := s.cache[session.ID]; ok && old.baseSize == info.Size() && old.baseTime.Equal(info.ModTime()) {
		hash = old.baseHash
	} else {
		base, err := os.ReadFile(s.sessionPath(session.ID))
		if err != nil {
			delete(s.cache, session.ID)
			return
		}
		hash = hashSessionSnapshot(base)
	}
	c := sessionCache{session: session, baseHash: hash, baseTime: info.ModTime(), baseSize: info.Size()}
	if journal, err := os.Stat(s.deltaPath(session.ID)); err == nil {
		c.journalTime, c.journalSize = journal.ModTime(), journal.Size()
	}
	if s.cache == nil {
		s.cache = make(map[string]sessionCache)
	}
	// Keep only the active session to bound retained history memory.
	clear(s.cache)
	s.cache[session.ID] = c
}

func (s *Store) maybeCompactSession(session Session) error {
	info, err := os.Stat(s.deltaPath(session.ID))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() < sessionCompactJournalBytes {
		return nil
	}
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}
	if int64(len(data)+1) > maxSessionBytes {
		return fmt.Errorf("session %s exceeds snapshot size limit", session.ID)
	}
	// Replace snapshot first: the old journal hash then becomes stale and
	// cannot replay a second time, even if interruption precedes cleanup.
	if err := writeBytesAtomic(s.sessionPath(session.ID), append(data, '\n')); err != nil {
		return err
	}
	if err := os.Remove(s.deltaPath(session.ID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func hashSessionSnapshot(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func (s *Store) deltaPath(id string) string { return filepath.Join(s.Dir, "sessions", id+".jsonl") }

func (s *Store) loadSessionDeltas(id string, base []byte, session *Session) error {
	path := s.deltaPath(id)
	entry, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !entry.Mode().IsRegular() || entry.Mode()&os.ModeSymlink != 0 || entry.Size() > maxSessionBytes {
		return fmt.Errorf("invalid session journal %s", id)
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxSessionBytes {
		return fmt.Errorf("invalid session journal %s", id)
	}
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			break
		} // Ignore an uncommitted trailing partial line.
		if err != nil {
			return err
		}
		var delta sessionDelta
		if err := json.Unmarshal(bytes.TrimSpace(line), &delta); err != nil {
			return fmt.Errorf("corrupt session journal %s: %w", id, err)
		}
		if delta.BaseHash != hashSessionSnapshot(base) {
			break
		} // Stale journal from a superseded snapshot.
		if delta.Previous != len(session.Messages) {
			return fmt.Errorf("session journal %s has discontinuous history", id)
		}
		if delta.UpdatedAt.IsZero() {
			return fmt.Errorf("session journal %s has invalid timestamp", id)
		}
		session.Messages = append(session.Messages, delta.Messages...)
		session.UpdatedAt, session.Model, session.Provider = delta.UpdatedAt, delta.Model, delta.Provider
	}
	return nil
}

func (s *Store) appendSessionDelta(id string, delta sessionDelta) error {
	path := s.deltaPath(id)
	// Remove stale records only after a new snapshot has become durable.
	base, err := os.ReadFile(s.sessionPath(id))
	if err != nil {
		return err
	}
	if delta.BaseHash != hashSessionSnapshot(base) {
		return fmt.Errorf("session snapshot changed during save")
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe session journal")
		}
		if info.Size() > maxSessionBytes {
			return fmt.Errorf("session journal exceeds size limit")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	// Truncate interrupted tail before appending a new committed record.
	contents, err := io.ReadAll(io.LimitReader(f, maxSessionBytes+1))
	if err != nil {
		return err
	}
	if int64(len(contents)) > maxSessionBytes {
		return fmt.Errorf("session journal exceeds size limit")
	}
	if len(contents) > 0 && contents[len(contents)-1] != '\n' {
		n := bytes.LastIndexByte(contents, '\n') + 1
		if err := f.Truncate(int64(n)); err != nil {
			return err
		}
	}
	// A journal for an earlier snapshot cannot be reused.
	if len(contents) > 0 {
		first := contents
		if n := bytes.IndexByte(first, '\n'); n >= 0 {
			first = first[:n]
		}
		var old sessionDelta
		if json.Unmarshal(first, &old) != nil || old.BaseHash != delta.BaseHash {
			if err := f.Truncate(0); err != nil {
				return err
			}
		}
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	encoded, err := json.Marshal(delta)
	if err != nil {
		return err
	}
	pos, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if pos+int64(len(encoded)+1) > maxSessionBytes {
		return fmt.Errorf("session journal exceeds %d bytes", maxSessionBytes)
	}
	if _, err := f.Write(append(encoded, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func (s *Store) ActivateSession(id string) (Session, error) {
	session, err := s.LoadSession(id)
	if err != nil {
		return Session{}, err
	}
	if err := s.setLastSession(session.ID); err != nil {
		return Session{}, err
	}
	return session, nil
}

func (s *Store) LastSession() (Session, error) {
	state, err := s.ensureState()
	if err != nil {
		return Session{}, err
	}
	if strings.TrimSpace(state.LastSession) == "" {
		return Session{}, fmt.Errorf("no previous session")
	}
	return s.LoadSession(state.LastSession)
}

func (s *Store) setLastSession(id string) error {
	if !validSessionID(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	state, err := s.ensureState()
	if err != nil {
		return err
	}
	// Most saves belong to the already active session. Avoid an extra
	// state.json rewrite and atomic rename on every assistant turn.
	if state.LastSession == id {
		return nil
	}
	state.LastSession = id
	return writeJSONAtomic(filepath.Join(s.Dir, "state.json"), state)
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
		if state.Version > Version {
			return State{}, fmt.Errorf("project state uses unsupported version %d", state.Version)
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

func validSessionID(id string) bool {
	if !strings.HasPrefix(id, "ses_") || len(id) != len("ses_")+16 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(id, "ses_"))
	return err == nil
}

func (s *Store) sessionPath(id string) string {
	return filepath.Join(s.Dir, "sessions", id+".json")
}

func cloneMessages(messages []provider.Message) []provider.Message {
	if len(messages) == 0 {
		return nil
	}
	cloned := make([]provider.Message, len(messages))
	copy(cloned, messages)
	for i := range cloned {
		if messages[i].Images != nil {
			cloned[i].Images = append([]string(nil), messages[i].Images...)
		}
		if messages[i].ToolCalls != nil {
			cloned[i].ToolCalls = append([]provider.ToolCall(nil), messages[i].ToolCalls...)
		}
	}
	return cloned
}

func clonePersistedMessages(messages []provider.Message) []provider.Message {
	cloned := cloneMessages(messages)
	for i := range cloned {
		// Reasoning streams are intentionally never written to project state.
		// Resume preserves visible text and tool history without exposing raw
		// model reasoning in the user-readable .rurushu session file.
		cloned[i].ReasoningContent = ""
	}
	return cloned
}

func validateSessionSize(session Session) error {
	data, err := json.Marshal(session)
	if err != nil {
		return fmt.Errorf("encode session %s: %w", session.ID, err)
	}
	if int64(len(data)) > maxSessionBytes {
		return fmt.Errorf("session %s exceeds %d bytes", session.ID, maxSessionBytes)
	}
	return nil
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	data = append(data, '\n')
	return writeBytesAtomic(path, data)
}

func writeBytesAtomic(path string, data []byte) error {
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

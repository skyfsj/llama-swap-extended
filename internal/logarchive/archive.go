// Package logarchive stores bounded incident snapshots on disk.
package logarchive

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	KindInferenceCrash = "inference-crash"
	KindRequestError   = "request-error"

	DefaultMaxFiles = 5
	MinMaxFiles     = 1
	MaxMaxFiles     = 100

	// MaxEntryBytes keeps one malformed or unexpectedly verbose process from
	// consuming the whole incident-log directory.
	MaxEntryBytes = 512 << 10
)

var validKinds = map[string]struct{}{
	KindInferenceCrash: {},
	KindRequestError:   {},
}

// Entry is the metadata needed to render and retrieve one incident snapshot.
// Name is an opaque, server-generated filename and must be passed back to
// Read without modification.
type Entry struct {
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Model     string    `json:"model,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	Size      int64     `json:"size"`
}

// Config controls one archive directory. MaxFiles applies independently to
// each incident kind, so a burst of request errors cannot evict every crash
// snapshot (and vice versa).
type Config struct {
	Dir      string
	MaxFiles int
}

// Store is safe for concurrent incident writers and readers.
type Store struct {
	mu       sync.Mutex
	dir      string
	maxFiles int
	sequence uint64
}

// New creates an incident archive and prunes files that exceed the configured
// retention. The directory is intentionally mode 0750; individual snapshots
// are written mode 0600 because they may contain runtime diagnostics.
func New(cfg Config) (*Store, error) {
	dir := strings.TrimSpace(cfg.Dir)
	if dir == "" {
		return nil, errors.New("incident log directory is required")
	}
	maxFiles := cfg.MaxFiles
	if maxFiles == 0 {
		maxFiles = DefaultMaxFiles
	}
	if err := validateMaxFiles(maxFiles); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create incident log directory: %w", err)
	}

	store := &Store{dir: dir, maxFiles: maxFiles}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.pruneAllLocked(); err != nil {
		return nil, fmt.Errorf("prune incident logs: %w", err)
	}
	return store, nil
}

func validateMaxFiles(maxFiles int) error {
	if maxFiles < MinMaxFiles || maxFiles > MaxMaxFiles {
		return fmt.Errorf("incident log maxFiles must be between %d and %d", MinMaxFiles, MaxMaxFiles)
	}
	return nil
}

// SetMaxFiles changes retention and immediately removes older snapshots.
func (s *Store) SetMaxFiles(maxFiles int) error {
	if s == nil {
		return errors.New("incident log store is nil")
	}
	if err := validateMaxFiles(maxFiles); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxFiles = maxFiles
	return s.pruneAllLocked()
}

// MaxFiles returns the current per-kind retention limit.
func (s *Store) MaxFiles() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxFiles
}

// Save durably writes one snapshot and then rotates the corresponding kind.
// The temporary file is created in the destination directory so Rename is an
// atomic replacement on the same filesystem.
func (s *Store) Save(kind, model string, content []byte) (Entry, error) {
	if s == nil {
		return Entry{}, errors.New("incident log store is nil")
	}
	if _, ok := validKinds[kind]; !ok {
		return Entry{}, fmt.Errorf("unsupported incident log kind %q", kind)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(content) > MaxEntryBytes {
		const marker = "[incident log truncated]\n"
		content = append([]byte(marker), content[len(content)-MaxEntryBytes+len(marker):]...)
	}
	now := time.Now().UTC()
	s.sequence++
	name := fmt.Sprintf("%s__%s__%06d__%s.log", kind, now.Format("20060102T150405.000000000Z"), s.sequence, safeModel(model))
	tmp, err := os.CreateTemp(s.dir, ".incident-*.tmp")
	if err != nil {
		return Entry{}, fmt.Errorf("create incident log temporary file: %w", err)
	}
	tmpName := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return Entry{}, fmt.Errorf("set incident log permissions: %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return Entry{}, fmt.Errorf("write incident log: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return Entry{}, fmt.Errorf("sync incident log: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Entry{}, fmt.Errorf("close incident log: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(s.dir, name)); err != nil {
		return Entry{}, fmt.Errorf("publish incident log: %w", err)
	}
	removeTemp = false
	if err := syncDirectory(s.dir); err != nil {
		return Entry{}, err
	}
	if err := s.pruneKindLocked(kind); err != nil {
		return Entry{}, fmt.Errorf("rotate %s incident logs: %w", kind, err)
	}
	return Entry{Name: name, Kind: kind, Model: safeModel(model), CreatedAt: now, Size: int64(len(content))}, nil
}

// List returns newest snapshots first. Unknown files and temporary files are
// ignored so an operator can keep unrelated files in the same directory.
func (s *Store) List() ([]Entry, error) {
	if s == nil {
		return nil, errors.New("incident log store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

// Read reads one server-generated snapshot filename after validating that it
// cannot escape the configured directory.
func (s *Store) Read(name string) ([]byte, Entry, error) {
	if s == nil {
		return nil, Entry{}, errors.New("incident log store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := parseName(name)
	if !ok || filepath.Base(name) != name {
		return nil, Entry{}, errors.New("invalid incident log name")
	}
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, Entry{}, os.ErrNotExist
		}
		return nil, Entry{}, fmt.Errorf("read incident log: %w", err)
	}
	entry.Size = int64(len(data))
	return data, entry, nil
}

func (s *Store) listLocked() ([]Entry, error) {
	files, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("list incident logs: %w", err)
	}
	entries := make([]Entry, 0, len(files))
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		entry, ok := parseName(file.Name())
		if !ok {
			continue
		}
		info, err := file.Info()
		if err != nil {
			return nil, fmt.Errorf("stat incident log %q: %w", file.Name(), err)
		}
		entry.Size = info.Size()
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].Name > entries[j].Name
		}
		return entries[i].CreatedAt.After(entries[j].CreatedAt)
	})
	return entries, nil
}

func (s *Store) pruneAllLocked() error {
	for _, kind := range []string{KindInferenceCrash, KindRequestError} {
		if err := s.pruneKindLocked(kind); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) pruneKindLocked(kind string) error {
	entries, err := s.listLocked()
	if err != nil {
		return err
	}
	kept := 0
	for _, entry := range entries {
		if entry.Kind != kind {
			continue
		}
		kept++
		if kept <= s.maxFiles {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, entry.Name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove old incident log %q: %w", entry.Name, err)
		}
	}
	return nil
}

func parseName(name string) (Entry, bool) {
	if filepath.Base(name) != name || !strings.HasSuffix(name, ".log") {
		return Entry{}, false
	}
	base := strings.TrimSuffix(name, ".log")
	parts := strings.Split(base, "__")
	if len(parts) != 4 {
		return Entry{}, false
	}
	if _, ok := validKinds[parts[0]]; !ok || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return Entry{}, false
	}
	createdAt, err := time.Parse("20060102T150405.000000000Z", parts[1])
	if err != nil {
		return Entry{}, false
	}
	return Entry{Name: name, Kind: parts[0], Model: parts[3], CreatedAt: createdAt.UTC()}, true
}

func safeModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range model {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
		if b.Len() >= 80 {
			break
		}
	}
	value := strings.Trim(b.String(), "-.")
	if value == "" {
		return "unknown"
	}
	return value
}

func syncDirectory(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open incident log directory for sync: %w", err)
	}
	defer file.Close()
	if err := file.Sync(); err != nil {
		// Windows and some filesystems do not support syncing a directory. The
		// file itself has already been synced, so keep the portable path useful.
		return nil
	}
	return nil
}

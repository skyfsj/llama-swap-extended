package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// ErrBlobNotFound reports that a referenced blob file is missing on disk.
// Read paths degrade to an empty body; callers must not treat this as a
// query failure.
var ErrBlobNotFound = errors.New("blob not found")

const blobSubdir = "blobs"

// unreferencedBlobGracePeriod shields blobs from GC while their referencing
// row may still be in flight: the audit writer stores the blob file BEFORE
// the INSERT commits, so a concurrent GC can read a reference set that does
// not yet include it. Deleting such a blob would permanently dangle the
// row's body. An unreferenced blob older than the grace period is a genuine
// orphan (row commits are milliseconds), and content addressing makes a
// later identical Put cheap.
const unreferencedBlobGracePeriod = 10 * time.Minute

// BlobStore is the content-addressed file store for audit conversation
// bodies. Each unique body exists exactly once at
// <db directory>/blobs/<first two hex chars>/<sha256>, so repeated media
// (retries, batch replays, duplicate images) is deduplicated automatically.
// Only file-backed stores have a BlobStore; in-memory stores keep bodies
// inline because they are not durable anyway.
type BlobStore struct {
	root string // directory containing the database file
}

func newBlobStore(dbPath string) *BlobStore {
	return &BlobStore{root: filepath.Dir(dbPath)}
}

func (b *BlobStore) dir() string {
	return filepath.Join(b.root, blobSubdir)
}

func (b *BlobStore) pathFor(hash string) (string, error) {
	if !validBlobHash(hash) {
		return "", fmt.Errorf("invalid blob reference %q", hash)
	}
	return filepath.Join(b.dir(), hash[:2], hash), nil
}

// Put stores the data under its sha256 if it is not present yet and returns
// the reference. It is idempotent: an existing file is left untouched, so
// concurrent writers for the same payload (identical content by
// construction) cannot corrupt each other.
func (b *BlobStore) Put(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	final, err := b.pathFor(hash)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(final); err == nil {
		return hash, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat blob %s: %w", hash, err)
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return "", fmt.Errorf("create blob directory: %w", err)
	}
	// The temp file lives next to the final path so the rename stays on one
	// filesystem; readers only ever see complete files.
	tmp, err := os.CreateTemp(filepath.Dir(final), "."+hash+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temp blob: %w", err)
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(data)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("write blob %s: %w", hash, werr)
	}
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("place blob %s: %w", hash, err)
	}
	return hash, nil
}

// Get returns the body stored under hash.
func (b *BlobStore) Get(hash string) ([]byte, error) {
	path, err := b.pathFor(hash)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrBlobNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read blob %s: %w", hash, err)
	}
	return data, nil
}

// Open returns a seekable reader for the blob plus its size without buffering
// it, so a caller can stream a multi-megabyte body (or hand it to
// http.ServeContent) instead of materializing it. The caller closes the file.
func (b *BlobStore) Open(hash string) (*os.File, int64, error) {
	path, err := b.pathFor(hash)
	if err != nil {
		return nil, 0, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, ErrBlobNotFound
	}
	if err != nil {
		return nil, 0, fmt.Errorf("open blob %s: %w", hash, err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, fmt.Errorf("stat blob %s: %w", hash, err)
	}
	return file, info.Size(), nil
}

// Size reports the stored size of a blob without reading it.
func (b *BlobStore) Size(hash string) (int64, error) {
	path, err := b.pathFor(hash)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, ErrBlobNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("stat blob %s: %w", hash, err)
	}
	return info.Size(), nil
}

// Delete removes one blob file. A missing file is not an error.
func (b *BlobStore) Delete(hash string) error {
	path, err := b.pathFor(hash)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete blob %s: %w", hash, err)
	}
	return nil
}

// ListAll returns the hashes of all stored blobs.
func (b *BlobStore) ListAll() ([]string, error) {
	var out []string
	err := filepath.WalkDir(b.dir(), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || !validBlobHash(d.Name()) {
			return nil
		}
		out = append(out, d.Name())
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list blobs: %w", err)
	}
	return out, nil
}

// DeleteUnreferenced removes every file whose hash is not in keep and prunes
// the now-empty fan-out directories. It returns the number of files removed.
// Blobs younger than unreferencedBlobGracePeriod are skipped: their INSERT
// may not have committed when the keep set was read.
func (b *BlobStore) DeleteUnreferenced(keep map[string]struct{}) (int, error) {
	all, err := b.ListAll()
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, hash := range all {
		if _, ok := keep[hash]; ok {
			continue
		}
		if path, pathErr := b.pathFor(hash); pathErr == nil {
			if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) < unreferencedBlobGracePeriod {
				continue
			}
		}
		if err := b.Delete(hash); err != nil {
			return deleted, err
		}
		deleted++
	}
	if err := removeEmptyDirs(b.dir()); err != nil {
		return deleted, err
	}
	return deleted, nil
}

// removeEmptyDirs prunes fan-out directories left empty by GC. The blob root
// itself is kept; a store with no blobs still owns the directory.
func removeEmptyDirs(root string) error {
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.IsDir() || path == root {
			return nil
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if len(entries) == 0 {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return nil
	})
	return err
}

func validBlobHash(hash string) bool {
	if len(hash) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(hash); i++ {
		c := hash[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

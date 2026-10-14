// Package auditstore persists conversation audit records in a dedicated
// database with compressed, content-addressed body segments.
//
// The audit trail is the one part of the store that grows without bound: every
// proxied request contributes a request body and a response body, and a single
// audio or image request can weigh megabytes. Keeping those bytes next to the
// operational tables made every dashboard page and every retention sweep share
// one SQLite connection with multi-megabyte writes.
//
// This package splits the two concerns:
//
//   - audit.db holds the row-level metadata (conversations, block index,
//     per-session accumulation) in its own connection, so audit maintenance
//     never blocks the operational store and the whole directory can be
//     archived or deleted on its own.
//   - segments/<date>.seg holds the body bytes as zstd frames, appended in
//     content-addressed order: an identical body — the same conversation
//     history re-sent on the next turn, a retried upload, a repeated system
//     prompt across sessions — is stored exactly once, and stored compressed.
//
// Bodies are additionally split at message boundaries before hashing, so a
// session that re-sends its whole history every turn only pays for the new
// messages: the accumulated context is shared, not duplicated.
package auditstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// blockRef locates one stored block inside a segment file.
type blockRef struct {
	Hash       string `json:"h"`
	Size       int    `json:"s"`
	Compressed int    `json:"c"`
	Segment    string `json:"g"`
	Offset     int64  `json:"o"`
	Length     int    `json:"l"`
}

const (
	segmentDirName = "segments"
	segmentSuffix  = ".seg"
	// segmentFlushBytes bounds how much unflushed data a crash can lose. The
	// writer calls Sync once per segment per write, which is cheap because the
	// OS already coalesces it; this threshold exists for the fdatasync cost on
	// spinning media.
	segmentFlushBytes = 8 << 20
)

// SegmentStore appends content-addressed zstd frames to day-partitioned files.
//
// A block is immutable once written: its hash is the sha256 of the raw bytes,
// so a reader that locates a block by hash always reconstructs the original
// payload. The mapping hash -> location lives in the audit database (the
// store's blocks table), which keeps this type free of index-consistency
// bookkeeping; the in-memory map is a cache of that table.
type SegmentStore struct {
	dir string

	mu      sync.Mutex
	writer  *os.File
	writing string // segment name currently open for append
	next    int64  // append offset in the open segment
	pending int64  // bytes appended since the last Sync

	refs map[string]blockRef

	encoder *zstd.Encoder
	decoder *zstd.Decoder
}

// openSegmentStore prepares the segment directory. Existing blocks are not
// scanned: the authoritative index is the database, loaded by the Store.
func openSegmentStore(root string) (*SegmentStore, error) {
	dir := filepath.Join(root, segmentDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create audit segment directory: %w", err)
	}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, fmt.Errorf("create zstd encoder: %w", err)
	}
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	if err != nil {
		encoder.Close()
		return nil, fmt.Errorf("create zstd decoder: %w", err)
	}
	return &SegmentStore{dir: dir, refs: make(map[string]blockRef), encoder: encoder, decoder: decoder}, nil
}

func (s *SegmentStore) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if s.writer != nil {
		err = s.writer.Close()
		s.writer = nil
	}
	if s.encoder != nil {
		s.encoder.Close()
	}
	if s.decoder != nil {
		s.decoder.Close()
	}
	return err
}

// loadIndex seeds the in-memory cache from the authoritative table.
func (s *SegmentStore) loadIndex(refs []blockRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ref := range refs {
		s.refs[ref.Hash] = ref
	}
}

func hashBlock(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// lookup returns the cached location of a block.
func (s *SegmentStore) lookup(hash string) (blockRef, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, ok := s.refs[hash]
	return ref, ok
}

// put stores data unless an identical block already exists. The second return
// value reports whether a new block was written, so callers can count real
// savings rather than re-uploads.
func (s *SegmentStore) put(data []byte, segment string) (blockRef, bool, error) {
	hash := hashBlock(data)
	if ref, ok := s.lookup(hash); ok {
		return ref, false, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Re-check under the lock: a concurrent writer may have stored it.
	if ref, ok := s.refs[hash]; ok {
		return ref, false, nil
	}

	compressed := s.encoder.EncodeAll(data, nil)
	file, err := s.openSegmentLocked(segment)
	if err != nil {
		return blockRef{}, false, err
	}
	offset, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		return blockRef{}, false, fmt.Errorf("seek audit segment: %w", err)
	}
	if _, err := file.Write(compressed); err != nil {
		return blockRef{}, false, fmt.Errorf("append audit segment: %w", err)
	}
	s.pending += int64(len(compressed))
	if s.pending >= segmentFlushBytes {
		if err := file.Sync(); err != nil {
			return blockRef{}, false, fmt.Errorf("sync audit segment: %w", err)
		}
		s.pending = 0
	}
	if offset+int64(len(compressed)) > s.next {
		s.next = offset + int64(len(compressed))
	}

	ref := blockRef{
		Hash:       hash,
		Size:       len(data),
		Compressed: len(compressed),
		Segment:    segment,
		Offset:     offset,
		Length:     len(compressed),
	}
	s.refs[hash] = ref
	return ref, true, nil
}

func (s *SegmentStore) openSegmentLocked(segment string) (*os.File, error) {
	if s.writer != nil && s.writing == segment {
		return s.writer, nil
	}
	if s.writer != nil {
		if err := s.writer.Sync(); err != nil {
			return nil, fmt.Errorf("sync audit segment: %w", err)
		}
		if err := s.writer.Close(); err != nil {
			return nil, fmt.Errorf("close audit segment: %w", err)
		}
		s.writer = nil
	}
	path := s.segmentPath(segment)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open audit segment: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("stat audit segment: %w", err)
	}
	s.writer = file
	s.writing = segment
	s.next = info.Size()
	return file, nil
}

func (s *SegmentStore) segmentPath(segment string) string {
	return filepath.Join(s.dir, segment+segmentSuffix)
}

// get reads and decompresses one block. A missing segment (pruned by
// retention, or a lost file) reports ErrBlockNotFound so the caller can
// degrade instead of failing the request.
func (s *SegmentStore) get(ref blockRef) ([]byte, error) {
	file, err := os.Open(s.segmentPath(ref.Segment))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrBlockNotFound
		}
		return nil, fmt.Errorf("open audit segment: %w", err)
	}
	defer file.Close()

	compressed := make([]byte, ref.Length)
	if _, err := file.ReadAt(compressed, ref.Offset); err != nil {
		return nil, fmt.Errorf("read audit block: %w", err)
	}
	data, err := s.decoder.DecodeAll(compressed, make([]byte, 0, ref.Size))
	if err != nil {
		return nil, fmt.Errorf("decode audit block: %w", err)
	}
	if len(data) != ref.Size {
		return nil, fmt.Errorf("audit block %s has %d bytes, want %d", ref.Hash, len(data), ref.Size)
	}
	return data, nil
}

// ErrBlockNotFound reports a block whose segment file is gone.
var ErrBlockNotFound = errors.New("audit block not found")

// segmentName returns the day partition a new block belongs to.
func segmentName(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// segments lists the segment files present on disk, oldest first.
func (s *SegmentStore) segments() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("list audit segments: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, segmentSuffix) {
			continue
		}
		names = append(names, strings.TrimSuffix(name, segmentSuffix))
	}
	sort.Strings(names)
	return names, nil
}

// sync flushes the open segment.
func (s *SegmentStore) sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writer == nil {
		return nil
	}
	if err := s.writer.Sync(); err != nil {
		return fmt.Errorf("sync audit segment: %w", err)
	}
	s.pending = 0
	return nil
}

// Bytes reports the on-disk size of every segment plus the block count held in
// the cache, for budget accounting and diagnostics.
func (s *SegmentStore) Bytes() (int64, error) {
	names, err := s.segments()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, name := range names {
		info, err := os.Stat(s.segmentPath(name))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return 0, fmt.Errorf("stat audit segment: %w", err)
		}
		total += info.Size()
	}
	return total, nil
}

// compact rewrites each segment keeping only the supplied live blocks, then
// removes segments that became empty. It returns the number of bytes reclaimed
// and the number of dropped blocks. Compaction is the only operation that
// physically shrinks a segment, so it runs on the maintenance cadence rather
// than per delete.
func (s *SegmentStore) compact(live map[string]bool) (int64, int, error) {
	if err := s.sync(); err != nil {
		return 0, 0, err
	}
	s.mu.Lock()
	if s.writer != nil {
		if err := s.writer.Close(); err != nil {
			s.mu.Unlock()
			return 0, 0, fmt.Errorf("close audit segment: %w", err)
		}
		s.writer = nil
		s.writing = ""
	}
	s.mu.Unlock()

	names, err := s.segments()
	if err != nil {
		return 0, 0, err
	}
	var reclaimed int64
	dropped := 0
	for _, name := range names {
		path := s.segmentPath(name)
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return reclaimed, dropped, fmt.Errorf("stat audit segment: %w", err)
		}

		s.mu.Lock()
		refs := make([]blockRef, 0, 64)
		for _, ref := range s.refs {
			if ref.Segment == name {
				refs = append(refs, ref)
			}
		}
		s.mu.Unlock()
		sort.Slice(refs, func(i, j int) bool { return refs[i].Offset < refs[j].Offset })

		keep := refs[:0]
		for _, ref := range refs {
			if live[ref.Hash] {
				keep = append(keep, ref)
			}
		}
		if len(keep) == len(refs) {
			continue
		}

		source, err := os.Open(path)
		if err != nil {
			return reclaimed, dropped, fmt.Errorf("open audit segment for compaction: %w", err)
		}
		temp, err := os.CreateTemp(s.dir, "."+name+".compact-*")
		if err != nil {
			source.Close()
			return reclaimed, dropped, fmt.Errorf("create compaction temp: %w", err)
		}

		updates := make([]blockRef, 0, len(keep))
		var offset int64
		for _, ref := range keep {
			compressed := make([]byte, ref.Length)
			if _, err := source.ReadAt(compressed, ref.Offset); err != nil {
				source.Close()
				temp.Close()
				os.Remove(temp.Name())
				return reclaimed, dropped, fmt.Errorf("read audit block for compaction: %w", err)
			}
			if _, err := temp.Write(compressed); err != nil {
				source.Close()
				temp.Close()
				os.Remove(temp.Name())
				return reclaimed, dropped, fmt.Errorf("write compacted audit segment: %w", err)
			}
			moved := ref
			moved.Offset = offset
			updates = append(updates, moved)
			offset += int64(len(compressed))
		}
		source.Close()
		if err := temp.Sync(); err != nil {
			temp.Close()
			os.Remove(temp.Name())
			return reclaimed, dropped, fmt.Errorf("sync compacted audit segment: %w", err)
		}
		if err := temp.Close(); err != nil {
			os.Remove(temp.Name())
			return reclaimed, dropped, fmt.Errorf("close compacted audit segment: %w", err)
		}
		reclaimed += info.Size() - offset
		dropped += len(refs) - len(keep)
		if offset == 0 {
			if err := os.Remove(temp.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
				return reclaimed, dropped, fmt.Errorf("remove empty audit segment: %w", err)
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return reclaimed, dropped, fmt.Errorf("remove audit segment: %w", err)
			}
			s.mu.Lock()
			for _, ref := range refs {
				delete(s.refs, ref.Hash)
			}
			s.mu.Unlock()
			continue
		}
		if err := os.Rename(temp.Name(), path); err != nil {
			return reclaimed, dropped, fmt.Errorf("replace audit segment: %w", err)
		}
		s.mu.Lock()
		for _, ref := range updates {
			s.refs[ref.Hash] = ref
		}
		s.mu.Unlock()
	}
	return reclaimed, dropped, nil
}

// dropSegment removes one day partition outright: retention drops whole days,
// which costs one unlink instead of a per-block rewrite.
func (s *SegmentStore) dropSegment(name string) (int64, error) {
	if err := s.sync(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writer != nil && s.writing == name {
		if err := s.writer.Close(); err != nil {
			return 0, fmt.Errorf("close audit segment: %w", err)
		}
		s.writer = nil
		s.writing = ""
	}
	info, err := os.Stat(s.segmentPath(name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("stat audit segment: %w", err)
	}
	if err := os.Remove(s.segmentPath(name)); err != nil {
		return 0, fmt.Errorf("remove audit segment: %w", err)
	}
	for hash, ref := range s.refs {
		if ref.Segment == name {
			delete(s.refs, hash)
		}
	}
	return info.Size(), nil
}

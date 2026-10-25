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

	// Compaction rewrites segment files in place, so appends to the segment
	// under rewrite must wait out the pass: bytes landing in the old file
	// while the temp is being built would be stranded by the rename, leaving
	// a block row that points at data that no longer exists.
	compactMu  sync.Mutex
	compacting map[string]bool
	appendGate *sync.Cond

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
	store := &SegmentStore{dir: dir, refs: make(map[string]blockRef), encoder: encoder, decoder: decoder}
	store.compacting = make(map[string]bool)
	store.appendGate = sync.NewCond(&store.mu)
	return store, nil
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
	// A segment under compaction is being rewritten: wait out the pass before
	// appending to it.
	for s.compacting[segment] {
		s.appendGate.Wait()
	}
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

// compact rewrites each segment keeping only the blocks the caller's keep
// predicate admits, then removes segments that became empty. It returns the
// references that moved to a new offset — the caller must persist them,
// because the database is the authoritative index a restart loads. Compaction
// is the only operation that physically shrinks a segment, so it runs on the
// maintenance cadence rather than per delete.
func (s *SegmentStore) compact(keep func(hash string) bool) ([]blockRef, int64, int, error) {
	s.compactMu.Lock()
	defer s.compactMu.Unlock()
	if err := s.sync(); err != nil {
		return nil, 0, 0, err
	}
	s.mu.Lock()
	if s.writer != nil {
		if err := s.writer.Close(); err != nil {
			s.mu.Unlock()
			return nil, 0, 0, fmt.Errorf("close audit segment: %w", err)
		}
		s.writer = nil
		s.writing = ""
	}
	s.mu.Unlock()

	s.removeStaleCompactionTemps()

	names, err := s.segments()
	if err != nil {
		return nil, 0, 0, err
	}
	var updates []blockRef
	var reclaimed int64
	dropped := 0
	for _, name := range names {
		moved, freed, droppedBlocks, err := s.compactSegment(name, keep)
		if err != nil {
			return updates, reclaimed, dropped, err
		}
		updates = append(updates, moved...)
		reclaimed += freed
		dropped += droppedBlocks
	}
	return updates, reclaimed, dropped, nil
}

// compactSegment rewrites one segment without the blocks keep rejects.
// Appends to the segment are gated out for the duration, so the refs snapshot
// stays stable until the rename lands. The returned references carry the new
// offsets.
func (s *SegmentStore) compactSegment(name string, keepFn func(hash string) bool) ([]blockRef, int64, int, error) {
	path := s.segmentPath(name)
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, 0, nil
		}
		return nil, 0, 0, fmt.Errorf("stat audit segment: %w", err)
	}

	s.mu.Lock()
	refs := make([]blockRef, 0, 64)
	for _, ref := range s.refs {
		if ref.Segment == name {
			refs = append(refs, ref)
		}
	}
	// Rewriting in file order keeps the segment's frame sequence stable across
	// passes: the repair scanner locates frames by walking that order.
	sort.Slice(refs, func(i, j int) bool { return refs[i].Offset < refs[j].Offset })
	keep := refs[:0]
	for _, ref := range refs {
		if keepFn(ref.Hash) {
			keep = append(keep, ref)
		}
	}
	if len(keep) == len(refs) {
		s.mu.Unlock()
		return nil, 0, 0, nil
	}
	s.compacting[name] = true
	// The open writer may hold the very file being replaced; its bytes are all
	// registered in refs already, so closing just drops the handle.
	if s.writing == name {
		if err := s.writer.Sync(); err != nil {
			delete(s.compacting, name)
			s.mu.Unlock()
			return nil, 0, 0, fmt.Errorf("sync audit segment: %w", err)
		}
		if err := s.writer.Close(); err != nil {
			delete(s.compacting, name)
			s.mu.Unlock()
			return nil, 0, 0, fmt.Errorf("close audit segment: %w", err)
		}
		s.writer = nil
		s.writing = ""
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.compacting, name)
		s.appendGate.Broadcast()
		s.mu.Unlock()
	}()

	source, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("open audit segment for compaction: %w", err)
	}
	temp, err := os.CreateTemp(s.dir, "."+name+".compact-*")
	if err != nil {
		source.Close()
		return nil, 0, 0, fmt.Errorf("create compaction temp: %w", err)
	}

	updates := make([]blockRef, 0, len(keep))
	var offset int64
	for _, ref := range keep {
		compressed := make([]byte, ref.Length)
		if _, err := source.ReadAt(compressed, ref.Offset); err != nil {
			source.Close()
			temp.Close()
			os.Remove(temp.Name())
			return nil, 0, 0, fmt.Errorf("read audit block for compaction: %w", err)
		}
		if _, err := temp.Write(compressed); err != nil {
			source.Close()
			temp.Close()
			os.Remove(temp.Name())
			return nil, 0, 0, fmt.Errorf("write compacted audit segment: %w", err)
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
		return nil, 0, 0, fmt.Errorf("sync compacted audit segment: %w", err)
	}
	if err := temp.Close(); err != nil {
		os.Remove(temp.Name())
		return nil, 0, 0, fmt.Errorf("close compacted audit segment: %w", err)
	}
	reclaimed := info.Size() - offset
	dropped := len(refs) - len(keep)
	if offset == 0 {
		if err := os.Remove(temp.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, reclaimed, dropped, fmt.Errorf("remove empty audit segment: %w", err)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, reclaimed, dropped, fmt.Errorf("remove audit segment: %w", err)
		}
		s.mu.Lock()
		for _, ref := range refs {
			delete(s.refs, ref.Hash)
		}
		s.mu.Unlock()
		return nil, reclaimed, dropped, nil
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return nil, reclaimed, dropped, fmt.Errorf("replace audit segment: %w", err)
	}
	s.mu.Lock()
	for _, ref := range updates {
		s.refs[ref.Hash] = ref
	}
	s.mu.Unlock()
	return updates, reclaimed, dropped, nil
}

// removeStaleCompactionTemps deletes temp files left behind by a compaction
// that crashed. The prefix keeps them out of segments(); the age check spares
// a temp an in-flight pass could still be writing.
func (s *SegmentStore) removeStaleCompactionTemps() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, ".") || !strings.Contains(name, ".compact-") {
			continue
		}
		info, err := entry.Info()
		if err != nil || time.Since(info.ModTime()) < time.Hour {
			continue
		}
		_ = os.Remove(filepath.Join(s.dir, name))
	}
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

// forget drops hashes from the in-memory index. The repair scanner uses it for
// blocks whose bytes are provably gone, so later reads report a clean miss
// instead of a decode error.
func (s *SegmentStore) forget(hashes []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, hash := range hashes {
		delete(s.refs, hash)
	}
}

// lockCompaction excludes compaction for the duration of the returned unlock.
// The repair scanner walks a segment's frames and rewrites nothing, but it
// must not race a compaction moving those same frames under it.
func (s *SegmentStore) lockCompaction() func() {
	s.compactMu.Lock()
	return s.compactMu.Unlock
}

// repairIndex rebuilds the offsets of one segment by locating every block's
// frame in the file itself. Blocks are content-addressed — the recorded hash
// is the sha256 of the decompressed bytes — so a match is self-verifying and
// no trust is placed in the (possibly stale) recorded offsets.
//
// The ordered walk exploits the layout invariants: compaction preserves file
// order and appends only ever grow the file, so frames appear in the order
// the caller supplies (creation time, then recorded offset). A hole — bytes
// lost to a crashed pass — is absorbed by probing a bounded window ahead.
// Whatever the walk cannot place goes through a full magic scan before it is
// declared absent, so a reference is only deleted when the whole file was
// examined.
func (s *SegmentStore) repairIndex(name string, ordered []blockRef) (found []blockRef, missing []string, err error) {
	path := s.segmentPath(name)
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			for _, ref := range ordered {
				missing = append(missing, ref.Hash)
			}
			return nil, missing, nil
		}
		return nil, nil, fmt.Errorf("open audit segment for repair: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("stat audit segment for repair: %w", err)
	}
	size := info.Size()

	matched := make(map[string]bool, len(ordered))
	found = make([]blockRef, 0, len(ordered))
	pos := int64(0)
walk:
	for idx := 0; idx < len(ordered); idx++ {
		ref := ordered[idx]
		if matched[ref.Hash] {
			continue
		}
		if s.trialFrame(file, size, pos, ref) {
			matched[ref.Hash] = true
			found = append(found, blockRef{Hash: ref.Hash, Size: ref.Size, Compressed: ref.Compressed, Segment: name, Offset: pos, Length: ref.Compressed})
			pos += int64(ref.Compressed)
			continue
		}
		// The frame the walk expects is not at pos: either an earlier block's
		// bytes were lost (its frame is gone but later ones remain) or the
		// frame belongs to a later row. Probe ahead before giving up on the
		// ordered pass — placement stays sequential either way.
		for probe := idx + 1; probe < len(ordered) && probe <= idx+repairProbeWindow; probe++ {
			cand := ordered[probe]
			if matched[cand.Hash] {
				continue
			}
			if s.trialFrame(file, size, pos, cand) {
				matched[cand.Hash] = true
				found = append(found, blockRef{Hash: cand.Hash, Size: cand.Size, Compressed: cand.Compressed, Segment: name, Offset: pos, Length: cand.Compressed})
				pos += int64(cand.Compressed)
				idx = probe
				continue walk
			}
		}
		// Neither the expected frame nor a probed one sits at pos: the tail's
		// layout violates the ordered assumption (a torn write, orphan frames).
		// Stop the walk and let the full scan place the remainder.
		break
	}

	// Every block the walk could not place gets one full-file scan: frames
	// start with the zstd magic, and each hit is tried against the remaining
	// rows at their exact recorded frame size. A budget keeps a file full of
	// false hits from turning into an unbounded decode loop.
	var unresolved []blockRef
	for _, ref := range ordered {
		if !matched[ref.Hash] {
			unresolved = append(unresolved, ref)
		}
	}
	if len(unresolved) == 0 {
		return found, nil, nil
	}

	scanned := 0
	const chunk = 1 << 20
	buf := make([]byte, chunk)
	var base int64
	for base < size && len(unresolved) > 0 && scanned < repairScanBudget {
		n, err := file.ReadAt(buf, base)
		if err != nil && err != io.EOF {
			return found, nil, fmt.Errorf("read audit segment for repair: %w", err)
		}
		window := buf[:n]
		for i := 0; i+4 <= len(window) && scanned < repairScanBudget; {
			if window[i] != 0x28 || window[i+1] != 0xB5 || window[i+2] != 0x2F || window[i+3] != 0xFD {
				i++
				continue
			}
			hit := base + int64(i)
			for idx := 0; idx < len(unresolved) && scanned < repairScanBudget; idx++ {
				ref := unresolved[idx]
				scanned++
				if s.trialFrame(file, size, hit, ref) {
					matched[ref.Hash] = true
					found = append(found, blockRef{Hash: ref.Hash, Size: ref.Size, Compressed: ref.Compressed, Segment: name, Offset: hit, Length: ref.Compressed})
					unresolved = append(unresolved[:idx], unresolved[idx+1:]...)
					break
				}
			}
			i++
		}
		base += chunk - 3
	}

	missing = make([]string, 0, len(unresolved))
	// Absence is only provable when the scan covered the whole file. A budget
	// overrun leaves the remainder untouched rather than guessing.
	if scanned < repairScanBudget || len(unresolved) == 0 {
		for _, ref := range unresolved {
			missing = append(missing, ref.Hash)
		}
	}
	return found, missing, nil
}

// trialFrame reports whether the recorded frame of ref sits exactly at pos:
// it reads the frame's recorded compressed size, decompresses it, and checks
// both the decoded length and the content hash.
func (s *SegmentStore) trialFrame(file *os.File, size, pos int64, ref blockRef) bool {
	if pos < 0 || ref.Compressed <= 0 || pos+int64(ref.Compressed) > size {
		return false
	}
	buf := make([]byte, ref.Compressed)
	if _, err := file.ReadAt(buf, pos); err != nil {
		return false
	}
	out, err := s.decoder.DecodeAll(buf, nil)
	if err != nil {
		return false
	}
	return len(out) == ref.Size && hashBlock(out) == ref.Hash
}

const (
	// repairProbeWindow bounds how far the ordered walk looks ahead when the
	// expected frame is missing; a crashed pass only ever punches small holes.
	repairProbeWindow = 256
	// repairScanBudget caps the fallback scan's decode trials so a pathological
	// file costs seconds, not minutes.
	repairScanBudget = 100000
)

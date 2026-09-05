package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// NoGeneration disables the ifGenerationMatch precondition on Blob.Put.
const NoGeneration int64 = -1

// snapshotTimeout bounds one VACUUM INTO + upload. It is independent of the HTTP request
// that triggered the write: a client that disconnects must not cancel the durability step.
const snapshotTimeout = 20 * time.Second

// Blob is the one object the store snapshots to. Implementations: FileBlob (dev) and
// GCSBlob (Cloud Run). Adding S3 later means one more implementation, nothing else.
type Blob interface {
	// Get downloads the object. Returns its bytes and current generation, or ErrNoObject.
	Get(ctx context.Context) (data []byte, generation int64, err error)
	// Put uploads data and returns the new generation. ifGenerationMatch fences the write:
	// NoGeneration = unconditional; 0 = only if the object does not exist yet; > 0 = only if
	// the current generation equals it (else ErrGenerationMismatch).
	Put(ctx context.Context, data []byte, ifGenerationMatch int64) (generation int64, err error)
}

// generationReader is implemented by Blobs that can report the current generation without
// downloading the object (both shipped Blobs do). It is used for boot-time fencing and for
// diagnostics when a Put is refused.
type generationReader interface {
	Generation(ctx context.Context) (int64, error)
}

// remoteGeneration returns the Blob's current generation, downloading the object only when
// the Blob cannot answer from metadata. ErrNoObject maps to 0.
func remoteGeneration(ctx context.Context, blob Blob) (int64, error) {
	var gen int64
	var err error
	if gr, ok := blob.(generationReader); ok {
		gen, err = gr.Generation(ctx)
	} else {
		_, gen, err = blob.Get(ctx)
	}
	if errors.Is(err, ErrNoObject) {
		return 0, nil
	}
	return gen, err
}

// RestoreIfMissing downloads the snapshot into path when no file exists there yet. main
// calls it before Open, only when a Blob is configured, and then passes the returned
// generation to Store.SetGeneration so the first upload is fenced against it. When the
// object does not exist (ErrNoObject) it returns (false, 0, nil) and Open seeds a fresh
// board. Any other error is fatal for boot: an unreachable bucket must not silently start
// an empty board that later overwrites the real one.
//
// When the database file already exists (a restart on the same disk; never the case on
// Cloud Run, whose /tmp is per instance) nothing is downloaded, but the remote generation
// is still read so the local file's first upload is fenced against the real object instead
// of against "must not exist".
func RestoreIfMissing(ctx context.Context, path string, blob Blob, log *slog.Logger) (restored bool, generation int64, err error) {
	if log == nil {
		log = slog.Default()
	}
	if _, err := os.Stat(path); err == nil {
		gen, err := remoteGeneration(ctx, blob)
		if err != nil {
			return false, 0, fmt.Errorf("read snapshot generation: %w", err)
		}
		log.WarnContext(ctx, "database present, skipping restore; the local file supersedes the snapshot on the next upload",
			"path", path, "remote_generation", gen)
		return false, gen, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, 0, fmt.Errorf("stat %s: %w", path, err)
	}
	data, gen, err := blob.Get(ctx)
	if errors.Is(err, ErrNoObject) {
		log.InfoContext(ctx, "no snapshot object, starting fresh", "path", path)
		return false, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("download snapshot: %w", err)
	}
	// A stale WAL next to a restored main file would corrupt it; the file was missing, so
	// any sidecars are leftovers.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, 0, fmt.Errorf("remove %s%s: %w", path, suffix, err)
		}
	}
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return false, 0, fmt.Errorf("write restored database: %w", err)
	}
	log.InfoContext(ctx, "restored database from snapshot", "path", path, "bytes", len(data), "generation", gen)
	return true, gen, nil
}

// SetGeneration seeds the ifGenerationMatch fence after a boot restore.
func (s *Store) SetGeneration(g int64) {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	s.generation = g
}

// Snapshot writes a consistent copy of the database to the Blob right now: VACUUM INTO a
// temp file next to the database, read it, Put with ifGenerationMatch = last known
// generation, delete the temp file. No-op (nil) when no Blob is configured. Used by
// afterWrite(sync=true).
//
// A refused Put (ErrGenerationMismatch) is final for this store: another writer owns the
// object, and uploading our copy over theirs would erase whatever they wrote. The store
// stays dirty, Health reports the failure (503) and every later snapshot attempt fails the
// same way until an operator decides which copy wins. There is deliberately no retry with
// the re-read generation.
func (s *Store) Snapshot(ctx context.Context) error {
	if s.blob == nil {
		return nil
	}
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	return s.snapshotLocked(ctx)
}

// SnapshotIfDirty snapshots only when there are writes the Blob has not seen. Used by main
// on SIGTERM so an idle instance shutting down never touches the object.
func (s *Store) SnapshotIfDirty(ctx context.Context) error {
	if s.blob == nil {
		return nil
	}
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	if !s.dirty {
		return nil
	}
	return s.snapshotLocked(ctx)
}

// SnapshotIfDue snapshots when there are unsaved writes and at least snapInterval has
// passed since the last successful snapshot. Used by afterWrite(sync=false).
func (s *Store) SnapshotIfDue(ctx context.Context) error {
	if s.blob == nil {
		return nil
	}
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	if !s.dirty {
		return nil
	}
	if !s.lastSnapshot.IsZero() && s.now().Sub(s.lastSnapshot) < s.snapInterval {
		return nil
	}
	return s.snapshotLocked(ctx)
}

// snapshotLocked does the work of Snapshot; the caller holds snapMu. dirty is cleared
// before the VACUUM so a write that commits while the upload is in flight (it blocks on
// snapMu in afterWrite) marks the store dirty again afterwards instead of being lost.
func (s *Store) snapshotLocked(ctx context.Context) error {
	s.dirty = false
	err := s.uploadSnapshot(ctx)
	if err != nil {
		s.dirty = true
		s.lastSnapErr = err
		return err
	}
	s.lastSnapErr = nil
	s.lastSnapshot = s.now()
	return nil
}

// uploadSnapshot performs VACUUM INTO + Put with the generation fence.
func (s *Store) uploadSnapshot(ctx context.Context) error {
	tmp := fmt.Sprintf("%s.snap-%d", s.path, time.Now().UnixNano())
	defer os.Remove(tmp)
	if _, err := s.w.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	data, err := os.ReadFile(tmp)
	if err != nil {
		return fmt.Errorf("read snapshot file: %w", err)
	}

	gen, err := s.blob.Put(ctx, data, s.generation)
	if errors.Is(err, ErrGenerationMismatch) {
		// Diagnostics only: the remote generation tells the operator which copy is newer.
		remote, remoteErr := remoteGeneration(ctx, s.blob)
		s.log.ErrorContext(ctx, "snapshot refused: another writer owns the object; keeping local writes unsynced",
			"had_generation", s.generation, "remote_generation", remote, "remote_err", remoteErr)
		return fmt.Errorf("upload snapshot (had generation %d, remote %d): %w", s.generation, remote, err)
	}
	if err != nil {
		return fmt.Errorf("upload snapshot: %w", err)
	}
	s.generation = gen
	s.log.DebugContext(ctx, "snapshot uploaded", "bytes", len(data), "generation", gen)
	return nil
}

// afterWrite is called by every mutating method after commit. It marks the store dirty and
// runs Snapshot (sync) or SnapshotIfDue (coalesced). Snapshot failures are logged and
// surfaced through Health, never returned to the caller: the write already committed.
//
// The upload runs on a context that inherits ctx's values (for logging) but not its
// cancellation: the request context is cancelled the moment the client disconnects, and a
// client going away must not abort the durability step of a write that already committed.
func (s *Store) afterWrite(ctx context.Context, sync bool) {
	if s.blob == nil {
		return
	}
	s.snapMu.Lock()
	s.dirty = true
	s.snapMu.Unlock()
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotTimeout)
	defer cancel()
	var err error
	if sync {
		err = s.Snapshot(sctx)
	} else {
		err = s.SnapshotIfDue(sctx)
	}
	if err != nil {
		s.log.ErrorContext(ctx, "snapshot failed", "err", err, "sync", sync)
	}
}

// Health runs SELECT 1 and reports snapshot state for /health. The error text of a failed
// snapshot stays out of the JSON (it names the bucket and echoes upstream error bodies);
// SnapshotFailing is the public signal and the logs carry the detail.
func (s *Store) Health(ctx context.Context) Health {
	h := Health{SnapshotEnabled: s.blob != nil}
	var one int
	if err := s.r.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err == nil && one == 1 {
		h.DB = true
	}
	s.snapMu.Lock()
	h.SnapshotDirty = s.dirty
	if !s.lastSnapshot.IsZero() {
		age := s.now().Sub(s.lastSnapshot).Seconds()
		h.SnapshotAgeS = &age
	}
	if s.lastSnapErr != nil {
		h.SnapshotFailing = true
		h.LastSnapshotError = s.lastSnapErr.Error()
	}
	s.snapMu.Unlock()
	h.OK = h.DB && !h.SnapshotFailing
	return h
}

// writeFileAtomic writes data to a temp file in the same directory and renames it over path.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmp := f.Name()
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tmp, perm); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

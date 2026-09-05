package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// FileBlob stores the snapshot at a local path (dev and tests). The generation is a counter
// kept in a sidecar file (<Path>.generation) so it is unique and monotonic regardless of
// the filesystem's mtime resolution; Put with a fence compares against it.
type FileBlob struct {
	Path string
}

// NewFileBlob returns a FileBlob writing to path.
func NewFileBlob(path string) *FileBlob { return &FileBlob{Path: path} }

func (b *FileBlob) generationPath() string { return b.Path + ".generation" }

// Generation returns the current generation, or ErrNoObject when no snapshot exists. A data
// file without a sidecar (written by something other than FileBlob) counts as generation 1.
func (b *FileBlob) Generation(ctx context.Context) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if _, err := os.Stat(b.Path); errors.Is(err, os.ErrNotExist) {
		return 0, ErrNoObject
	} else if err != nil {
		return 0, fmt.Errorf("stat %s: %w", b.Path, err)
	}
	raw, err := os.ReadFile(b.generationPath())
	if errors.Is(err, os.ErrNotExist) {
		return 1, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", b.generationPath(), err)
	}
	gen, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || gen < 1 {
		return 0, fmt.Errorf("parse %s: %q is not a generation", b.generationPath(), raw)
	}
	return gen, nil
}

// Get implements Blob.
func (b *FileBlob) Get(ctx context.Context) ([]byte, int64, error) {
	gen, err := b.Generation(ctx)
	if err != nil {
		return nil, 0, err
	}
	data, err := os.ReadFile(b.Path)
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", b.Path, err)
	}
	return data, gen, nil
}

// Put implements Blob. It writes to a temp file in the same directory and renames, then
// bumps the sidecar generation.
func (b *FileBlob) Put(ctx context.Context, data []byte, ifGenerationMatch int64) (int64, error) {
	current, err := b.Generation(ctx)
	exists := err == nil
	if err != nil && !errors.Is(err, ErrNoObject) {
		return 0, err
	}
	switch {
	case ifGenerationMatch == NoGeneration:
	case ifGenerationMatch == 0 && exists:
		return 0, ErrGenerationMismatch
	case ifGenerationMatch > 0 && (!exists || current != ifGenerationMatch):
		return 0, ErrGenerationMismatch
	}
	if err := writeFileAtomic(b.Path, data, 0o600); err != nil {
		return 0, fmt.Errorf("write %s: %w", b.Path, err)
	}
	next := current + 1
	if err := writeFileAtomic(b.generationPath(), []byte(strconv.FormatInt(next, 10)), 0o600); err != nil {
		return 0, fmt.Errorf("write %s: %w", b.generationPath(), err)
	}
	return next, nil
}

package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeGCS is enough of the metadata server and the JSON storage API to exercise GCSBlob:
// one object, generation counter, ifGenerationMatch precondition, bearer check.
type fakeGCS struct {
	mu         sync.Mutex
	data       []byte
	generation int64
	tokens     int
	uploads    int
}

func (f *fakeGCS) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /token", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "missing Metadata-Flavor", http.StatusForbidden)
			return
		}
		f.mu.Lock()
		f.tokens++
		f.mu.Unlock()
		fmt.Fprint(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	})
	auth := func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer tok" }
	mux.HandleFunc("GET /storage/v1/b/{bucket}/o/{object}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.PathValue("bucket") != "bkt" || r.PathValue("object") != "board.db" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.data == nil {
			http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("alt") == "media" {
			if g := r.URL.Query().Get("generation"); g != "" && g != strconv.FormatInt(f.generation, 10) {
				http.Error(w, "stale generation", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(f.data)
			return
		}
		fmt.Fprintf(w, `{"name":"board.db","generation":"%d"}`, f.generation)
	})
	mux.HandleFunc("POST /upload/storage/v1/b/{bucket}/o", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		if q.Get("uploadType") != "media" || q.Get("name") != "board.db" {
			http.Error(w, "bad query "+r.URL.RawQuery, http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		if g := q.Get("ifGenerationMatch"); g != "" {
			want, _ := strconv.ParseInt(g, 10, 64)
			if want != f.generation {
				http.Error(w, `{"error":{"code":412}}`, http.StatusPreconditionFailed)
				return
			}
		}
		f.uploads++
		f.data = body
		f.generation += 1000 // GCS generations are big, non-sequential numbers
		fmt.Fprintf(w, `{"name":"board.db","generation":"%d"}`, f.generation)
	})
	return mux
}

func TestGCSBlob(t *testing.T) {
	ctx := context.Background()
	fake := &fakeGCS{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	b := NewGCSBlob("bkt", "board.db")
	b.HTTP = srv.Client()
	b.MetadataURL = srv.URL + "/token"
	b.StorageURL = srv.URL

	if _, _, err := b.Get(ctx); !errors.Is(err, ErrNoObject) {
		t.Fatalf("Get(empty) err = %v, want ErrNoObject", err)
	}
	gen, err := b.Put(ctx, []byte("v1"), 0)
	if err != nil || gen != 1000 {
		t.Fatalf("Put(fresh) = %d, %v", gen, err)
	}
	if _, err := b.Put(ctx, []byte("v2"), 0); !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("Put(0) on existing err = %v, want ErrGenerationMismatch", err)
	}
	if _, err := b.Put(ctx, []byte("v2"), 5); !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("Put(stale) err = %v, want ErrGenerationMismatch", err)
	}
	gen2, err := b.Put(ctx, []byte("v2"), gen)
	if err != nil || gen2 != 2000 {
		t.Fatalf("Put(fenced) = %d, %v", gen2, err)
	}
	gen3, err := b.Put(ctx, []byte("v3"), NoGeneration)
	if err != nil || gen3 != 3000 {
		t.Fatalf("Put(unconditional) = %d, %v", gen3, err)
	}
	data, got, err := b.Get(ctx)
	if err != nil || string(data) != "v3" || got != gen3 {
		t.Fatalf("Get = %q, %d, %v", data, got, err)
	}
	if fake.tokens != 1 {
		t.Errorf("token fetched %d times, want 1 (cached)", fake.tokens)
	}

	// The store end-to-end: restore-on-boot, fenced upload, mismatch recovery.
	dir := t.TempDir()
	path := dir + "/board.db"
	restored, rgen, err := RestoreIfMissing(ctx, path, b, nil)
	if err != nil || !restored || rgen != gen3 {
		t.Fatalf("RestoreIfMissing = %v, %d, %v", restored, rgen, err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "v3" {
		t.Fatalf("restored file = %q", raw)
	}
	// "v3" is not a database; start a real one at another path and snapshot into the fake.
	s, err := Open(ctx, dir+"/real.db", Options{Blob: b})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	s.SetGeneration(gen3)
	before := fake.uploads
	invite(t, s, "alice") // sync snapshot
	if fake.uploads != before+1 {
		t.Fatalf("uploads = %d, want %d", fake.uploads, before+1)
	}
	if h := s.Health(ctx); !h.OK || h.LastSnapshotError != "" {
		t.Fatalf("health = %+v", h)
	}
	data, _, _ = b.Get(ctx)
	if !strings.HasPrefix(string(data), "SQLite format 3") {
		t.Fatalf("uploaded snapshot is not a SQLite file: %q", data[:16])
	}
	// Someone else uploaded: our fence is stale; Snapshot is refused and never retries over it.
	if _, err := b.Put(ctx, []byte("intruder"), NoGeneration); err != nil {
		t.Fatal(err)
	}
	before = fake.uploads
	if err := s.Snapshot(ctx); !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("Snapshot after external write err = %v, want ErrGenerationMismatch", err)
	}
	if fake.uploads != before {
		t.Fatalf("uploads after refusal = %d, want %d (no overwrite)", fake.uploads, before)
	}
	if data, _, _ = b.Get(ctx); string(data) != "intruder" {
		t.Fatalf("remote object replaced: %q", data[:16])
	}
	if h := s.Health(ctx); h.OK || !h.SnapshotFailing || !h.SnapshotDirty {
		t.Fatalf("health after refusal = %+v", h)
	}
	if fake.tokens != 1 {
		t.Errorf("token fetched %d times, want 1 (cached)", fake.tokens)
	}
}

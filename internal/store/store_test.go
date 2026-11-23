package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestStoreConcurrentWritersAndTryWith(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".counter.lock")
	dataPath := filepath.Join(dir, "counter.json")

	const workers = 16
	const increments = 10
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < increments; j++ {
				err := With(context.Background(), RankEvents, lockPath, func(ctx context.Context) error {
					var cur int
					if b, rErr := os.ReadFile(dataPath); rErr == nil {
						_ = json.Unmarshal(b, &cur)
					}
					cur++
					out, _ := json.Marshal(cur)
					return WriteAtomic(dataPath, out, 0o600)
				})
				if err != nil {
					t.Errorf("With failed: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	var got int
	b, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal counter: %v", err)
	}
	if got != workers*increments {
		t.Fatalf("counter = %d, want %d", got, workers*increments)
	}

	// Verify TryWith returns acquired=false without blocking while lock is held.
	err = With(context.Background(), RankEvents, lockPath, func(ctx context.Context) error {
		acquired, tryErr := TryWith(context.Background(), RankEvents, lockPath, func(context.Context) error {
			t.Fatal("TryWith callback should not execute while lock is held")
			return nil
		})
		if tryErr != nil || acquired {
			t.Fatalf("TryWith = (%v, %v), want (false, nil)", acquired, tryErr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("outer With failed: %v", err)
	}
}

func TestStoreRankOrderingEnforced(t *testing.T) {
	dir := t.TempDir()
	evLock := filepath.Join(dir, ".ev.lock")
	obLock := filepath.Join(dir, ".ob.lock")

	// Valid increasing order: RankEvents (40) -> RankOutbox (60)
	err := With(context.Background(), RankEvents, evLock, func(ctx context.Context) error {
		return With(ctx, RankOutbox, obLock, func(context.Context) error {
			return nil
		})
	})
	if err != nil {
		t.Fatalf("expected increasing lock rank to succeed, got %v", err)
	}

	// Invalid inverted order: RankOutbox (60) -> RankEvents (40) must panic in tests
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected lock rank inversion to panic in test mode")
		}
	}()
	_ = With(context.Background(), RankOutbox, obLock, func(ctx context.Context) error {
		return With(ctx, RankEvents, evLock, func(context.Context) error {
			return nil
		})
	})
}

func TestWriteAtomicRejectsSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	realTarget := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(realTarget, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(realTarget, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(link, []byte("overwritten"), 0o600); err == nil {
		t.Fatal("expected WriteAtomic to reject symlink target")
	}
	got, _ := os.ReadFile(realTarget)
	if string(got) != "original" {
		t.Fatalf("victim file mutated via symlink: %q", string(got))
	}
}

func TestRandomHex(t *testing.T) {
	if got := RandomHex(0); got != "" {
		t.Fatalf("RandomHex(0) = %q, want empty", got)
	}
	if got := RandomHex(-3); got != "" {
		t.Fatalf("RandomHex(-3) = %q, want empty", got)
	}
	a := RandomHex(8)
	b := RandomHex(8)
	if len(a) != 16 || len(b) != 16 {
		t.Fatalf("expected 16-char hex strings, got %q and %q", a, b)
	}
	if a == b {
		t.Fatalf("expected distinct random hex strings, both %q", a)
	}
	for _, r := range a {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("unexpected non-lowercase-hex char %q in %q", r, a)
		}
	}
}

func TestWriteJSONAtomicAndReadJSONFile(t *testing.T) {
	type payload struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "item.json")

	if _, err := ReadJSONFile[payload](path); !os.IsNotExist(err) {
		t.Fatalf("expected IsNotExist for missing file, got %v", err)
	}

	want := payload{Name: "alpha", Count: 42}
	if err := WriteJSONAtomic(path, want, 0o600); err != nil {
		t.Fatalf("WriteJSONAtomic: %v", err)
	}
	got, err := ReadJSONFile[payload](path)
	if err != nil {
		t.Fatalf("ReadJSONFile: %v", err)
	}
	if got != want {
		t.Fatalf("ReadJSONFile = %+v, want %+v", got, want)
	}

	if err := os.WriteFile(path, []byte("{bad-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadJSONFile[payload](path); err == nil {
		t.Fatal("expected error for corrupt JSON")
	}
}

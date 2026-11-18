package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Rank defines the strict increasing lock acquisition order across Vault stores.
// Acquiring a lock with Rank <= any Rank already held on ctx violates lock hierarchy.
type Rank int

const (
	RankExtensions    Rank = 10 // <vault>/extensions.json & <vault>/private/trust.json
	RankTriggerCur    Rank = 20 // <vault>/memory/trigger_cursors.json
	RankWatcher       Rank = 30 // <vault>/memory/watchers/<id>/<acct>.json
	RankTimers        Rank = 35 // <vault>/memory/timers.json (EvaluateDue -> events.Enqueue)
	RankEvents        Rank = 40 // <vault>/memory/events.json
	RankGoals         Rank = 50 // <vault>/memory/goals.json
	RankOutbox        Rank = 60 // <vault>/memory/outbox/items.jsonl
	RankChannels      Rank = 65 // <vault>/memory/channels/<bridge>.json
	RankTaint         Rank = 70 // <vault>/private/taint/*.json
	RankTasks         Rank = 75 // <vault>/memory/tasks.json & tasks_archive.jsonl
	RankSubagentQueue Rank = 80 // <vault>/memory/pending_*_subagents.json & nested_conversations.json
	RankQuota         Rank = 85 // <vault>/memory/quota*.json (leaf store)
)

// ErrLockOrderViolation is returned (or panicked in tests) when a nested store call
// attempts to acquire a Rank <= an already held Rank.
var ErrLockOrderViolation = errors.New("store: lock acquisition rank violation")

type ctxKeyHeldRanks struct{}

type heldRank struct {
	Rank Rank
	Path string
}

var (
	pathMuMap      sync.Map // map[string]*sync.Mutex
	goroutineRanks sync.Map // map[uint64][]heldRank (active in tests / HQ_STRICT_LOCK_RANK=1)

	inGoTestOnce = sync.OnceValue(func() bool {
		if len(os.Args) == 0 {
			return false
		}
		base := strings.ToLower(filepath.Base(os.Args[0]))
		if strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe") {
			return true
		}
		for _, arg := range os.Args[1:] {
			if arg == "--" {
				break
			}
			if strings.HasPrefix(arg, "-test.") {
				return true
			}
		}
		return false
	})
)

// InGoTest reports whether the current process is executing under `go test` without
// linking the standard library `testing` package into production `hq` binaries.
func InGoTest() bool {
	return inGoTestOnce()
}

func currentGoroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	line := bytes.TrimPrefix(buf[:n], []byte("goroutine "))
	idx := bytes.IndexByte(line, ' ')
	if idx <= 0 {
		return 0
	}
	gid, _ := strconv.ParseUint(string(line[:idx]), 10, 64)
	return gid
}

func pushGoroutineRank(rank Rank, lockKey string) (func(), error) {
	if !InGoTest() && os.Getenv("HQ_STRICT_LOCK_RANK") != "1" {
		return func() {}, nil
	}
	gid := currentGoroutineID()
	if gid == 0 {
		return func() {}, nil
	}
	var existing []heldRank
	if raw, ok := goroutineRanks.Load(gid); ok {
		existing = raw.([]heldRank)
	}
	for _, h := range existing {
		if rank <= h.Rank {
			err := fmt.Errorf("%w: attempted rank %d (%s) while holding rank %d (%s) on goroutine %d", ErrLockOrderViolation, rank, lockKey, h.Rank, h.Path, gid)
			panic(err)
		}
	}
	next := make([]heldRank, len(existing)+1)
	copy(next, existing)
	next[len(existing)] = heldRank{Rank: rank, Path: lockKey}
	goroutineRanks.Store(gid, next)
	return func() {
		if len(existing) == 0 {
			goroutineRanks.Delete(gid)
		} else {
			goroutineRanks.Store(gid, existing)
		}
	}, nil
}

func mutexForPath(cleanLockPath string) *sync.Mutex {
	if v, ok := pathMuMap.Load(cleanLockPath); ok {
		return v.(*sync.Mutex)
	}
	m := &sync.Mutex{}
	actual, _ := pathMuMap.LoadOrStore(cleanLockPath, m)
	return actual.(*sync.Mutex)
}

func canonicalLockKey(lockPath string) string {
	abs, err := filepath.Abs(lockPath)
	if err != nil {
		return filepath.Clean(lockPath)
	}
	parent := filepath.Dir(abs)
	if resolvedParent, rErr := filepath.EvalSymlinks(parent); rErr == nil && resolvedParent != "" {
		return filepath.Join(filepath.Clean(resolvedParent), filepath.Base(abs))
	}
	return filepath.Clean(abs)
}

func checkAndPushRank(ctx context.Context, rank Rank, lockKey string) (context.Context, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var existing []heldRank
	if raw := ctx.Value(ctxKeyHeldRanks{}); raw != nil {
		if slice, ok := raw.([]heldRank); ok {
			existing = slice
		}
	}
	for _, h := range existing {
		if rank <= h.Rank {
			err := fmt.Errorf("%w: attempted rank %d (%s) while holding rank %d (%s)", ErrLockOrderViolation, rank, lockKey, h.Rank, h.Path)
			if InGoTest() || os.Getenv("HQ_STRICT_LOCK_RANK") == "1" {
				panic(err)
			}
			return ctx, err
		}
	}
	next := make([]heldRank, len(existing)+1)
	copy(next, existing)
	next[len(existing)] = heldRank{Rank: rank, Path: lockKey}
	return context.WithValue(ctx, ctxKeyHeldRanks{}, next), nil
}

// With acquires both the per-path in-process mutex and an exclusive OS file lock (flock)
// at lockPath under the declared Rank, then invokes fn with the updated context.
func With(ctx context.Context, rank Rank, lockPath string, fn func(context.Context) error) error {
	lockKey := canonicalLockKey(lockPath)
	nextCtx, err := checkAndPushRank(ctx, rank, lockKey)
	if err != nil {
		return err
	}
	popGID, gErr := pushGoroutineRank(rank, lockKey)
	if gErr != nil {
		return gErr
	}
	defer popGID()

	mu := mutexForPath(lockKey)
	mu.Lock()
	defer mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(lockKey), 0o700); err != nil {
		return err
	}
	lf, err := openNoFollowLockFile(lockKey)
	if err != nil {
		return err
	}
	defer lf.Close()

	if err := flockExclusive(lf, false); err != nil {
		return err
	}
	defer func() {
		_ = flockUnlock(lf)
	}()

	return fn(nextCtx)
}

// TryWith attempts a non-blocking lock acquisition (TryLock + LOCK_EX|LOCK_NB).
// If another goroutine or process currently holds lockPath, it returns (false, nil) immediately.
func TryWith(ctx context.Context, rank Rank, lockPath string, fn func(context.Context) error) (bool, error) {
	lockKey := canonicalLockKey(lockPath)
	nextCtx, err := checkAndPushRank(ctx, rank, lockKey)
	if err != nil {
		return false, err
	}

	mu := mutexForPath(lockKey)
	if !mu.TryLock() {
		return false, nil
	}
	defer mu.Unlock()

	popGID, gErr := pushGoroutineRank(rank, lockKey)
	if gErr != nil {
		return false, gErr
	}
	defer popGID()

	if err := os.MkdirAll(filepath.Dir(lockKey), 0o700); err != nil {
		return false, err
	}
	lf, err := openNoFollowLockFile(lockKey)
	if err != nil {
		return false, err
	}
	defer lf.Close()

	if err := flockExclusive(lf, true); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return false, nil
		}
		return false, err
	}
	defer func() {
		_ = flockUnlock(lf)
	}()

	if err := fn(nextCtx); err != nil {
		return true, err
	}
	return true, nil
}

func randomSuffix() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// WriteAtomic writes data to path atomically via same-directory temp file + fsync + rename + dir fsync.
// If path already exists as a symlink, WriteAtomic refuses to overwrite it.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	clean := filepath.Clean(path)
	if st, err := os.Lstat(clean); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("store: refusing to overwrite symlink at %s", clean)
	}
	dir := filepath.Dir(clean)
	dirPerm := os.FileMode(0o700)
	if perm&0o077 != 0 {
		dirPerm = 0o755
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return err
	}

	tmpName := filepath.Join(dir, fmt.Sprintf(".tmp-%s-%s", filepath.Base(clean), randomSuffix()))
	f, err := os.OpenFile(tmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY|noFollowFlag, perm)
	if err != nil {
		return err
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, clean); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if df, dErr := os.Open(dir); dErr == nil {
		_ = df.Sync()
		_ = df.Close()
	}
	return nil
}

// OpenCreateExclNoFollow creates a brand-new file with O_CREATE|O_EXCL|O_WRONLY|O_NOFOLLOW
// and ensures parent directories exist (0700 when perm restricts group/other).
func OpenCreateExclNoFollow(path string, perm os.FileMode) (*os.File, error) {
	clean := filepath.Clean(path)
	if st, err := os.Lstat(clean); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("store: refusing symlink at %s", clean)
	}
	dir := filepath.Dir(clean)
	dirPerm := os.FileMode(0o700)
	if perm&0o077 != 0 {
		dirPerm = 0o755
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(clean, os.O_CREATE|os.O_EXCL|os.O_WRONLY|noFollowFlag, perm)
	if err != nil {
		return nil, err
	}
	if cErr := f.Chmod(perm); cErr != nil {
		_ = f.Close()
		_ = os.Remove(clean)
		return nil, cErr
	}
	return f, nil
}

// AppendLineNoFollow appends a single line (ensuring trailing '\n') to path using O_APPEND|O_NOFOLLOW and fsync.
func AppendLineNoFollow(path string, line []byte, perm os.FileMode) error {
	clean := filepath.Clean(path)
	if st, err := os.Lstat(clean); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("store: refusing to append to symlink at %s", clean)
	}
	dir := filepath.Dir(clean)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(clean, os.O_CREATE|os.O_WRONLY|os.O_APPEND|noFollowFlag, perm)
	if err != nil {
		return err
	}
	payload := line
	if len(payload) == 0 || payload[len(payload)-1] != '\n' {
		payload = append(append([]byte(nil), line...), '\n')
	}
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

# 3. Feature Flags

Feature flags decouple deployment from release. A flag lets you ship code behind a gate, enable it for a percentage of users, and kill the feature instantly if something goes wrong -- all without a new deploy. The hard parts are thread-safety under concurrent reads and writes, deterministic percentage assignment, and hot reloading configuration without restarts. This lesson builds a `flags` package that covers all three.

```text
feature-flags/
  go.mod
  flags.go
  flags_test.go
  cmd/demo/main.go
```

## Concepts

### Why Flags Instead Of Feature Branches

A feature branch requires a merge and a deploy to disable. A flag lets you disable instantly: the deployment artifact stays unchanged, and the change propagates in milliseconds. Kill switches and percentage rollouts share this property -- they operate on running code, not on a build pipeline.

### Consistent Hashing For Percentage Rollouts

A percentage flag must be deterministic: user A is always in the rollout or always out. Using `rand` per request breaks this -- the user's experience flickers between enabled and disabled. The correct approach hashes the user ID to a stable bucket number in `[0, 100)` and compares that to the rollout percentage. `hash/crc32` with `ChecksumIEEE` is fast, stdlib-only, and stable across runs:

```
bucket = crc32.ChecksumIEEE([]byte(userID)) % 100
enabled = bucket < percentage
```

Any user with a hash whose modulo falls below the percentage threshold is in the rollout. Raising the percentage from 10 to 20 adds users monotonically without shuffling the existing cohort -- a property that consistent hashing guarantees.

### Thread-Safety With sync.RWMutex

Many goroutines check flags on every request; flag writes happen rarely (reloads, admin API). `sync.RWMutex` matches this: `RLock`/`RUnlock` allow unlimited concurrent readers and block only when a writer holds the lock. A plain `sync.Mutex` serializes all reads unnecessarily. The critical invariant: every exported method that reads `s.flags` holds `RLock`; every method that writes holds `Lock`.

### Hot Reload Via LoadFromFile

`LoadFromFile` is the hot-reload path: reading a new JSON file and atomically swapping the flag map does not require a restart. Because the I/O and JSON decode happen outside the lock, the write lock is held only for the pointer swap -- keeping the critical section as short as possible. Concurrent readers never see a partially-reloaded state.

### Sentinel Errors And Validation

Callers that need to distinguish "flag not found" from "bad percentage" use `errors.Is`. Wrapping with `%w` preserves the sentinel through the error chain:

```go
return fmt.Errorf("SetPercentage %q: %w", name, ErrInvalidPercentage)
```

The test then asserts `errors.Is(err, ErrInvalidPercentage)` rather than matching a string, so renaming the error message does not break the test.

## Exercises

This is a library, not a program: there is no single `main`. You verify it with `go test`.

### Exercise 1: The Flag Types And Store

Create `flags.go`:

```go
package flags

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"sync"
)

// FlagType distinguishes boolean flags from percentage-rollout flags.
type FlagType string

const (
	// FlagBoolean is a simple on/off toggle.
	FlagBoolean FlagType = "boolean"
	// FlagPercentage enables the flag for a fraction of users determined by
	// consistent hashing of the user ID.
	FlagPercentage FlagType = "percentage"
)

// Sentinel errors returned by Store methods. Callers assert with errors.Is.
var (
	ErrFlagNotFound      = errors.New("flag not found")
	ErrInvalidPercentage = errors.New("percentage must be 0-100")
)

// Flag is a single feature flag definition. It is safe to copy.
type Flag struct {
	Name       string   `json:"name"`
	Type       FlagType `json:"type"`
	Enabled    bool     `json:"enabled"`
	Percentage int      `json:"percentage,omitempty"` // meaningful only for FlagPercentage
}

// Store holds feature flag definitions and protects them with a readers-writer
// mutex. The zero value is not usable; call NewStore.
type Store struct {
	mu    sync.RWMutex
	flags map[string]*Flag
}

// NewStore returns an empty flag store.
func NewStore() *Store {
	return &Store{flags: make(map[string]*Flag)}
}

// LoadFromFile reads a JSON array of Flag values from path and replaces the
// store contents atomically. Concurrent readers are not affected until the
// replacement is complete.
func (s *Store) LoadFromFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("LoadFromFile: %w", err)
	}
	var list []*Flag
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("LoadFromFile: parse: %w", err)
	}
	m := make(map[string]*Flag, len(list))
	for _, f := range list {
		m[f.Name] = f
	}
	s.mu.Lock()
	s.flags = m
	s.mu.Unlock()
	return nil
}

// Set inserts or replaces a flag. It is safe for concurrent use.
func (s *Store) Set(f Flag) {
	cp := f
	s.mu.Lock()
	s.flags[f.Name] = &cp
	s.mu.Unlock()
}

// IsEnabled reports whether the named boolean flag is enabled. Unknown flags
// return false.
func (s *Store) IsEnabled(name string) bool {
	s.mu.RLock()
	f, ok := s.flags[name]
	if !ok {
		s.mu.RUnlock()
		return false
	}
	enabled := f.Enabled
	s.mu.RUnlock()
	return enabled
}

// IsEnabledFor reports whether the named flag is enabled for userID. For
// FlagBoolean flags the userID is ignored. For FlagPercentage flags the bucket
// is derived from crc32.ChecksumIEEE(userID) % 100 and compared to the
// configured percentage. Unknown flags return false.
func (s *Store) IsEnabledFor(name, userID string) bool {
	s.mu.RLock()
	f, ok := s.flags[name]
	if !ok {
		s.mu.RUnlock()
		return false
	}
	// Copy fields we need under the lock before releasing it.
	enabled := f.Enabled
	ftype := f.Type
	pct := f.Percentage
	s.mu.RUnlock()
	if !enabled {
		return false
	}
	if ftype == FlagBoolean {
		return true
	}
	bucket := int(crc32.ChecksumIEEE([]byte(userID)) % 100)
	return bucket < pct
}

// SetEnabled toggles the Enabled field on an existing flag. Returns
// ErrFlagNotFound if the flag does not exist.
func (s *Store) SetEnabled(name string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.flags[name]
	if !ok {
		return fmt.Errorf("SetEnabled %q: %w", name, ErrFlagNotFound)
	}
	f.Enabled = enabled
	return nil
}

// SetPercentage updates the rollout percentage on an existing flag. Returns
// ErrFlagNotFound if the flag does not exist and ErrInvalidPercentage if pct
// is outside [0, 100].
func (s *Store) SetPercentage(name string, pct int) error {
	if pct < 0 || pct > 100 {
		return fmt.Errorf("SetPercentage %q: %w: got %d", name, ErrInvalidPercentage, pct)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.flags[name]
	if !ok {
		return fmt.Errorf("SetPercentage %q: %w", name, ErrFlagNotFound)
	}
	f.Percentage = pct
	return nil
}

// All returns a snapshot of all flags in the store. The order is not
// guaranteed.
func (s *Store) All() []Flag {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Flag, 0, len(s.flags))
	for _, f := range s.flags {
		out = append(out, *f)
	}
	return out
}

// Len returns the number of flags in the store.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.flags)
}
```

`NewStore` returns an empty store. `Set` is the single write path used in tests and by `LoadFromFile`. `IsEnabledFor` contains the core percentage logic: the bucket is `crc32.ChecksumIEEE(userID) % 100`, which is stable across calls for the same user ID.

### Exercise 2: The Test Suite

Create `flags_test.go`:

```go
package flags

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// writeJSON writes flags as a JSON array to a temp file and returns its path.
func writeJSON(t *testing.T, flags []Flag) string {
	t.Helper()
	b, err := json.Marshal(flags)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	f, err := os.CreateTemp(t.TempDir(), "flags-*.json")
	if err != nil {
		t.Fatalf("tempfile: %v", err)
	}
	if _, err := f.Write(b); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	return f.Name()
}

func TestIsEnabledReturnsFalseForUnknownFlag(t *testing.T) {
	t.Parallel()

	s := NewStore()
	if s.IsEnabled("no-such-flag") {
		t.Fatal("unknown flag should be false")
	}
}

func TestBooleanFlagToggle(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Set(Flag{Name: "feat-a", Type: FlagBoolean, Enabled: false})

	if s.IsEnabled("feat-a") {
		t.Fatal("expected disabled")
	}
	if err := s.SetEnabled("feat-a", true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if !s.IsEnabled("feat-a") {
		t.Fatal("expected enabled after toggle")
	}
}

func TestSetEnabledReturnsErrFlagNotFound(t *testing.T) {
	t.Parallel()

	s := NewStore()
	err := s.SetEnabled("ghost", true)
	if !errors.Is(err, ErrFlagNotFound) {
		t.Fatalf("err = %v, want ErrFlagNotFound", err)
	}
}

func TestSetPercentageRejectsOutOfRange(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Set(Flag{Name: "rollout", Type: FlagPercentage, Enabled: true, Percentage: 10})

	for _, pct := range []int{-1, 101, 200} {
		err := s.SetPercentage("rollout", pct)
		if !errors.Is(err, ErrInvalidPercentage) {
			t.Errorf("SetPercentage(%d): err = %v, want ErrInvalidPercentage", pct, err)
		}
	}
}

func TestPercentageRolloutIsDeterministic(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Set(Flag{Name: "beta", Type: FlagPercentage, Enabled: true, Percentage: 50})

	// The same user must always get the same answer.
	for i := 0; i < 100; i++ {
		first := s.IsEnabledFor("beta", "user-42")
		second := s.IsEnabledFor("beta", "user-42")
		if first != second {
			t.Fatal("non-deterministic result for the same user")
		}
	}
}

func TestPercentageRolloutAt0DisablesAll(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Set(Flag{Name: "none", Type: FlagPercentage, Enabled: true, Percentage: 0})

	for i := 0; i < 200; i++ {
		userID := fmt.Sprintf("user-%d", i)
		if s.IsEnabledFor("none", userID) {
			t.Fatalf("0%% rollout should never enable, but did for %q", userID)
		}
	}
}

func TestPercentageRolloutAt100EnablesAll(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Set(Flag{Name: "all", Type: FlagPercentage, Enabled: true, Percentage: 100})

	for i := 0; i < 200; i++ {
		userID := fmt.Sprintf("user-%d", i)
		if !s.IsEnabledFor("all", userID) {
			t.Fatalf("100%% rollout should always enable, but did not for %q", userID)
		}
	}
}

func TestIsEnabledForDisabledFlagReturnsFalse(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Set(Flag{Name: "off", Type: FlagPercentage, Enabled: false, Percentage: 100})

	if s.IsEnabledFor("off", "any-user") {
		t.Fatal("disabled flag with 100%% should still return false")
	}
}

func TestLoadFromFile(t *testing.T) {
	t.Parallel()

	input := []Flag{
		{Name: "flag-a", Type: FlagBoolean, Enabled: true},
		{Name: "flag-b", Type: FlagPercentage, Enabled: true, Percentage: 25},
	}
	path := writeJSON(t, input)

	s := NewStore()
	if err := s.LoadFromFile(path); err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}
	if s.Len() != 2 {
		t.Fatalf("Len = %d, want 2", s.Len())
	}
	if !s.IsEnabled("flag-a") {
		t.Fatal("flag-a should be enabled")
	}
}

func TestLoadFromFileReplacesExistingFlags(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Set(Flag{Name: "old-flag", Type: FlagBoolean, Enabled: true})

	path := writeJSON(t, []Flag{
		{Name: "new-flag", Type: FlagBoolean, Enabled: true},
	})
	if err := s.LoadFromFile(path); err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}
	if s.IsEnabled("old-flag") {
		t.Fatal("old-flag should have been removed after reload")
	}
	if !s.IsEnabled("new-flag") {
		t.Fatal("new-flag should be enabled")
	}
}

func TestLoadFromFileMissingFile(t *testing.T) {
	t.Parallel()

	s := NewStore()
	err := s.LoadFromFile(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestConcurrentReadsAndWrites(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Set(Flag{Name: "concurrent", Type: FlagBoolean, Enabled: true})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = s.IsEnabled("concurrent")
		}()
		go func(n int) {
			defer wg.Done()
			_ = s.SetEnabled("concurrent", n%2 == 0)
		}(i)
	}
	wg.Wait()
}

// ExampleStore_IsEnabled demonstrates the basic boolean flag check.
func ExampleStore_IsEnabled() {
	s := NewStore()
	s.Set(Flag{Name: "dark-mode", Type: FlagBoolean, Enabled: true})
	fmt.Println(s.IsEnabled("dark-mode"))
	fmt.Println(s.IsEnabled("unknown"))
	// Output:
	// true
	// false
}

// ExampleStore_IsEnabledFor demonstrates the percentage-rollout check. With
// Percentage: 100 every user is in the rollout.
func ExampleStore_IsEnabledFor() {
	s := NewStore()
	s.Set(Flag{Name: "beta", Type: FlagPercentage, Enabled: true, Percentage: 100})
	fmt.Println(s.IsEnabledFor("beta", "alice"))
	fmt.Println(s.IsEnabledFor("beta", ""))
	// Output:
	// true
	// true
}
```

The tests are table-driven where multiple inputs share the same logic (e.g., `TestSetPercentageRejectsOutOfRange`), use `t.Parallel()` throughout, and assert errors with `errors.Is`. `TestConcurrentReadsAndWrites` is the race-condition test: run it with `-race` and it will detect unsynchronized access if the mutex is ever removed.

Your turn: add `TestSetPercentageOnMissingFlag` that calls `s.SetPercentage("ghost", 10)` on an empty store and asserts `errors.Is(err, ErrFlagNotFound)`.

### Exercise 3: The Demo Program

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"log"
	"os"

	flags "example.com/feature-flags"
)

func main() {
	s := flags.NewStore()

	// Populate via Set for the demo; in production use LoadFromFile.
	s.Set(flags.Flag{Name: "new-ui", Type: flags.FlagBoolean, Enabled: true})
	s.Set(flags.Flag{Name: "beta-search", Type: flags.FlagPercentage, Enabled: true, Percentage: 50})
	s.Set(flags.Flag{Name: "dark-mode", Type: flags.FlagBoolean, Enabled: false})

	fmt.Printf("store contains %d flags\n", s.Len())
	fmt.Printf("new-ui enabled:          %v\n", s.IsEnabled("new-ui"))
	fmt.Printf("dark-mode enabled:       %v\n", s.IsEnabled("dark-mode"))
	fmt.Printf("beta-search for alice:   %v\n", s.IsEnabledFor("beta-search", "alice"))
	fmt.Printf("beta-search for bob:     %v\n", s.IsEnabledFor("beta-search", "bob"))

	// Show determinism: the same user always lands in the same bucket.
	first := s.IsEnabledFor("beta-search", "alice")
	second := s.IsEnabledFor("beta-search", "alice")
	if first != second {
		log.Fatal("non-deterministic: same user got different results")
	}
	fmt.Println("determinism check: OK")

	// File-based hot-reload example.
	if path := os.Getenv("FLAGS_FILE"); path != "" {
		if err := s.LoadFromFile(path); err != nil {
			log.Fatalf("LoadFromFile: %v", err)
		}
		fmt.Printf("reloaded; store now has %d flags\n", s.Len())
	}
}
```

Run it with:

```bash
go run ./cmd/demo
```

The demo only touches exported API (`NewStore`, `Set`, `Flag`, `Len`, `IsEnabled`, `IsEnabledFor`, `LoadFromFile`). No unexported fields are accessed from `cmd/demo`.

## Common Mistakes

### Using A Value Receiver On A Mutex-Containing Type

Wrong: `func (s Store) IsEnabled(name string) bool { s.mu.RLock() ... }`. Calling a value-receiver method copies the `Store`, including the `sync.RWMutex`. The copy's mutex is independent of the original's; concurrent callers using the original bypass the lock entirely.

Fix: all methods on `Store` use pointer receivers (`*Store`). The Go vet tool (`go vet ./...`) flags this with "method on value receiver copies lock".

### Reading flags After RUnlock

Wrong: releasing the read lock and then dereferencing the returned pointer: `s.mu.RUnlock(); return f.Enabled` where `f` is `*Flag`. A concurrent `Set` call can now overwrite `f.Enabled` while the caller reads it.

Fix: read the field before releasing the lock, or copy the struct under the lock and return the copy. `IsEnabled` and `IsEnabledFor` both read `f.Enabled` (or compute the bucket) while still holding `RLock`, then release.

### Non-Deterministic Percentage Assignment

Wrong: using `rand.Intn(100) < percentage` per request. User A gets different results on every request -- the experience flickers.

Fix: hash the user ID with `crc32.ChecksumIEEE`. The hash is deterministic for a given user ID, so the bucket is stable across every call, every process restart, and every server replica.

### Replacing The Map Without Holding The Write Lock

Wrong: `s.flags = m` without `s.mu.Lock()`. A concurrent reader can see the old map pointer after the assignment has been partially visible to some cores, or race with the map assignment itself.

Fix: hold `Lock()` for the entire map replacement. `LoadFromFile` builds the new map outside the lock (the expensive part: I/O and JSON decode) and only swaps the pointer inside the lock, keeping the lock hold time minimal.

## Verification

From `~/go-exercises/feature-flags`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The `-race` flag catches concurrent access bugs that normal tests miss. Run the demo to confirm the exported API works end to end:

```bash
go run ./cmd/demo
```

## Summary

- Feature flags decouple deployment from release; a kill switch stops a bad feature in milliseconds without a redeploy.
- `sync.RWMutex` allows unlimited concurrent readers; writers hold the exclusive lock only long enough to swap a pointer.
- Percentage rollouts use `crc32.ChecksumIEEE(userID) % 100` for deterministic, stable bucket assignment.
- `LoadFromFile` rebuilds the map outside the lock (I/O + JSON decode) and swaps the pointer under the write lock, keeping the critical section short.
- Sentinel errors wrapped with `%w` let callers use `errors.Is` rather than string matching.

## What's Next

Next: [Health Endpoints](../04-health-endpoints/04-health-endpoints.md).

## Resources

- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex) -- the readers-writer lock used throughout this lesson.
- [hash/crc32](https://pkg.go.dev/hash/crc32) -- `ChecksumIEEE` for deterministic user-ID hashing.
- [encoding/json](https://pkg.go.dev/encoding/json) -- `Unmarshal` for loading flag configuration from JSON files.
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) -- concurrency idioms underlying the lock-vs-channel design choice.

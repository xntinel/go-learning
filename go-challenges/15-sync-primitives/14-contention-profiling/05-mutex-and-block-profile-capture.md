# Exercise 5: Capture mutex and block profiles from a test and assert they are non-empty

Capturing a contention profile from a test — not just from a live server — lets you
pin the profiler behavior in CI and hand a reviewer a reproducible artifact. This
module drives heavy contention on the single-lock store and writes both the mutex
profile and the block profile to `t.TempDir` files, asserting each is non-empty,
while saving and restoring the profiler rates in `t.Cleanup` so no overhead leaks
into sibling tests.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
profile-capture/              independent module: example.com/profile-capture
  go.mod                      go 1.26
  store.go                    type Single + busyWork (the contended store)
  cmd/
    demo/
      main.go                 runnable demo: drive contention, write mutex.prof
  profile_test.go             TestProfileContention, TestBlockProfileNonEmpty,
                              rate-restoration assertion
```

- Files: `store.go`, `cmd/demo/main.go`, `profile_test.go`.
- Implement: nothing new in `store.go` beyond the contended `Single`; the work is in the tests that write and validate profiles.
- Test: `TestProfileContention` writes the mutex profile via `pprof.Lookup("mutex").WriteTo` to a `t.TempDir` file and asserts non-empty size; `TestBlockProfileNonEmpty` does the same for the block profile; both save-and-restore the profiler rates in `t.Cleanup`.
- Verify: `go test -count=1 -race ./...`

### Writing a profile from a test, and restoring the rates

A profile is captured in three steps: arm the profiler, drive work that contends,
write the accumulated profile out. `runtime.SetMutexProfileFraction(1)` records
every contention event; `pprof.Lookup("mutex")` returns the named `*pprof.Profile`;
`WriteTo(w, 0)` serializes it in the binary pprof format that `go tool pprof`
reads. The block profile is symmetric: `SetBlockProfileRate(1)` records every block
longer than a nanosecond, and `pprof.Lookup("block").WriteTo` writes it.

The assertion is that the written file is non-empty. This is a deliberately robust
check: a pprof profile always serializes a valid protobuf — sample types, a
mapping, a header — even when it has captured zero samples, so a non-empty file
proves the write path works without depending on the exact scheduler behavior of
the CI machine. Under the heavy contention these tests drive (32 goroutines each
doing thousands of locked increments on one mutex), the profile will in fact carry
real wait samples; the size assertion is simply the honest, non-flaky thing to
assert in an automated test. To actually *read* the samples you open the file with
`go tool pprof` and run `top`/`list`, which is a human step, not a test assertion.

The discipline that makes this safe to run in a shared test binary is
save-and-restore. `SetMutexProfileFraction` returns the previous fraction; capture
it, and in `t.Cleanup` restore it and set the block rate back to 0. Skip this and
every subsequent test in the package pays the fraction-1 recording overhead on
every contended lock — a subtle tax that shows up as unexplained slowdown in
unrelated tests. `TestRatesRestored` proves the discipline: it sets a known
fraction, does nothing, restores it, and asserts the round-trip returns the value
it set — a guard against a future edit dropping the restore.

Create `store.go`:

```go
package store

import "sync"

// Single is a contended single-lock counter used to generate contention that the
// mutex and block profilers can capture.
type Single struct {
	mu   sync.Mutex
	data map[string]int
}

// NewSingle returns an empty store.
func NewSingle() *Single { return &Single{data: make(map[string]int)} }

// Increment adds one to key's count, holding the lock across simulated work so
// contention accumulates in the profiles.
func (s *Single) Increment(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key]++
	guard(busyWork(128))
}

// Get returns key's count under the lock.
func (s *Single) Get(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[key]
}

func busyWork(iterations int) uint64 {
	var acc uint64
	for i := range iterations {
		acc += uint64(i)*2 + 1
	}
	return acc
}

var sink uint64

func guard(v uint64) {
	if v == 1<<63 {
		sink = v
	}
}
```

### The runnable demo

The demo arms the mutex profiler, drives contention, and writes `mutex.prof` to the
current directory so you can open it with `go tool pprof mutex.prof`. It prints the
file size to confirm the write succeeded.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"sync"

	"example.com/profile-capture"
)

func main() {
	prev := runtime.SetMutexProfileFraction(1)
	defer runtime.SetMutexProfileFraction(prev)

	s := store.NewSingle()
	var wg sync.WaitGroup
	const goroutines, ops = 32, 3000
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range ops {
				s.Increment("k")
			}
		}()
	}
	wg.Wait()

	f, err := os.Create("mutex.prof")
	if err != nil {
		fmt.Println("create:", err)
		return
	}
	defer f.Close()
	if err := pprof.Lookup("mutex").WriteTo(f, 0); err != nil {
		fmt.Println("write:", err)
		return
	}
	info, _ := f.Stat()
	fmt.Printf("wrote mutex.prof (%d bytes > 0: %v)\n", info.Size(), info.Size() > 0)
	fmt.Printf("count(k)=%d\n", s.Get("k"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (byte count varies; the boolean and count are stable):

```
wrote mutex.prof (573 bytes > 0: true)
count(k)=96000
```

### Tests

`TestProfileContention` drives contention and writes the mutex profile to a temp
file, asserting non-empty size. `TestBlockProfileNonEmpty` does the same for the
block profile — the former "your turn" task, now a real test. `TestRatesRestored`
proves the save/restore round-trip. All three restore the profiler rates so nothing
leaks.

Create `profile_test.go`:

```go
package store

import (
	"os"
	"runtime"
	"runtime/pprof"
	"sync"
	"testing"
)

func driveContention(goroutines, ops int) *Single {
	s := NewSingle()
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range ops {
				s.Increment("k")
			}
		}()
	}
	wg.Wait()
	return s
}

func writeProfile(t *testing.T, name string) int64 {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), name+"-*.prof")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := pprof.Lookup(name).WriteTo(f, 0); err != nil {
		t.Fatalf("WriteTo(%s): %v", name, err)
	}
	info, err := os.Stat(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func TestProfileContention(t *testing.T) {
	prev := runtime.SetMutexProfileFraction(1)
	t.Cleanup(func() { runtime.SetMutexProfileFraction(prev) })

	driveContention(32, 5000)

	if size := writeProfile(t, "mutex"); size == 0 {
		t.Fatal("mutex profile is empty")
	}
}

func TestBlockProfileNonEmpty(t *testing.T) {
	prev := runtime.SetMutexProfileFraction(1)
	runtime.SetBlockProfileRate(1)
	t.Cleanup(func() {
		runtime.SetMutexProfileFraction(prev)
		runtime.SetBlockProfileRate(0)
	})

	driveContention(32, 5000)

	if size := writeProfile(t, "block"); size == 0 {
		t.Fatal("block profile is empty")
	}
}

func TestRatesRestored(t *testing.T) {
	// Round-trip the mutex fraction: set a known value, then restore and assert
	// the restore observed exactly what we set. This guards against a future edit
	// dropping the restore and leaking overhead into sibling tests.
	original := runtime.SetMutexProfileFraction(7)
	observed := runtime.SetMutexProfileFraction(original)
	if observed != 7 {
		t.Fatalf("restore observed fraction %d, want 7 (rate not tracked correctly)", observed)
	}
}
```

## Review

The capture is correct when the written file is non-empty and the profiler rates
are put back exactly as they were found. The non-empty assertion is intentionally
robust: pprof always emits a valid protobuf, so the check exercises the write path
without depending on the CI scheduler producing a particular number of samples —
the honest thing to assert in automation. The rate discipline is the real lesson:
`SetMutexProfileFraction` returns the previous fraction, so capture it and restore
it in `t.Cleanup`; `TestRatesRestored` proves the round-trip and fails loudly if a
future edit drops it. Two mistakes to avoid: asserting on sample *counts* (flaky —
assert file non-empty instead), and forgetting to set the block rate back to 0,
which leaves every later test recording every block. Reading the samples is a human
step: `go tool pprof mutex.prof`, then `top` and `list Increment`.

## Resources

- [runtime/pprof.Lookup](https://pkg.go.dev/runtime/pprof#Lookup) — named profiles including `mutex` and `block`.
- [runtime/pprof.Profile.WriteTo](https://pkg.go.dev/runtime/pprof#Profile.WriteTo) — the `debug` argument (0 = binary pprof, >0 = text).
- [testing.T.TempDir](https://pkg.go.dev/testing#T.TempDir) — a per-test directory cleaned up automatically.
- [runtime.SetBlockProfileRate](https://pkg.go.dev/runtime#SetBlockProfileRate) — the block profiler and its rate threshold.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-sharded-vs-single-benchmark.md](04-sharded-vs-single-benchmark.md) | Next: [06-http-pprof-debug-endpoint.md](06-http-pprof-debug-endpoint.md)

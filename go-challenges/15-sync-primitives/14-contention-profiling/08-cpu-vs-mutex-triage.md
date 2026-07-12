# Exercise 8: Triage a slow path: pair the CPU profile with the mutex profile from one run

The most common contention-profiling mistake is not a bad capture — it is reading
the wrong profile. This module builds a triage harness that records the CPU
profile and the mutex profile from a single run of a workload, plus two synthetic
workloads that stall for opposite reasons, so you learn on rigged evidence which
profile answers which question before you have to do it on a pager.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
profile-triage/               independent module: example.com/profile-triage
  go.mod                      go 1.23+
  triage.go                   Capture(dir, work): one run, two profiles;
                              ErrCPUProfileActive double-capture guard
  workloads.go                WaitBound (many goroutines, one lock, short hold)
                              CPUBound (expensive checksum inside one goroutine's
                              critical section, zero waiters)
  cmd/
    demo/
      main.go                 runnable demo: capture both profiles around the
                              wait-bound workload
  triage_test.go              capture test, double-capture guard, fraction
                              restore, table-driven workload correctness, Example
```

- Files: `triage.go`, `workloads.go`, `cmd/demo/main.go`, `triage_test.go`.
- Implement: `Capture(dir string, work func()) (cpuPath, mutexPath string, err error)` that arms both profilers around exactly one execution of `work`, writes `cpu.prof` and `mutex.prof` into `dir`, and refuses to nest inside an active CPU profile with the sentinel `ErrCPUProfileActive`; the two contrasting workloads.
- Test: `Capture` produces two non-empty files from one run; a second capture attempted while a CPU profile is active fails with `errors.Is(err, ErrCPUProfileActive)`; the mutex fraction is restored; workload arithmetic is exact under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/14-contention-profiling/08-cpu-vs-mutex-triage/cmd/demo
cd go-solutions/15-sync-primitives/14-contention-profiling/08-cpu-vs-mutex-triage
```

### Wait-bound versus CPU-bound: the diagnosis that decides the fix

Two services can show the identical symptom — a locked path is slow under load —
for opposite reasons. In the *wait-bound* case, many goroutines contend for one
lock whose critical section is short; almost all of the elapsed time is spent
parked in the runtime's semaphore queue. The mutex profile lights up (wait time
attributed to the contended `Lock` stack) while the CPU profile shows the locked
function as cheap. The fix is a locking fix: shard, shrink, downgrade to RWMutex
or atomics. In the *CPU-bound* case, the work *inside* the critical section is
genuinely expensive — here, a checksum over hundreds of hashed blocks, standing
in for the JSON serialization or compression you meet in real handlers — and in
the worst version there is only one goroutine, so nobody ever waits. The mutex
profile is silent; the CPU profile shows the checksum burning cycles. A locking
fix here changes nothing, because the lock was never the problem — the fix is to
make the work cheaper or move it elsewhere.

You can only tell these apart if you have both profiles *from the same run*.
Capturing them in separate runs invites skew: load, cache state, and GC pressure
differ between runs, and the comparison stops being evidence. That is the whole
reason `Capture` exists as one function rather than "run it twice with different
flags".

### The capture protocol, and why nesting must fail loudly

CPU profiling is process-global and exclusive: `pprof.StartCPUProfile(w)` begins
100 Hz sampling into `w` and returns an error if a CPU profile is already being
written — there is exactly one CPU profiler per process. `Capture` surfaces that
as the sentinel `ErrCPUProfileActive` (wrapped with `%w`), because the tempting
alternative — silently skipping the CPU half and returning only the mutex profile
— would hand the caller exactly the one-eyed evidence this module warns against.
The mutex side is the familiar discipline: raise the fraction to 1 for the
duration of `work`, restore the previous value afterwards (a `defer` so a
panicking workload cannot leak fraction-1 overhead into the rest of the process).
One asymmetry deserves a comment in the code: the CPU profile contains only
samples taken between start and stop, but the mutex profile is *cumulative since
process start* — the write at the end includes contention from before `Capture`
was called. For triage that is acceptable (you are asking "does this stack show
up at all"), and the alternative — forking a fresh process per capture — is what
heavyweight benchmark rigs do; know the caveat and read accordingly.

Create `triage.go`:

```go
package triage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
)

// ErrCPUProfileActive reports that a CPU profile is already being captured.
// There is one CPU profiler per process; nesting must fail loudly rather than
// silently return half the evidence.
var ErrCPUProfileActive = errors.New("triage: a CPU profile is already active")

// Capture runs work exactly once while recording both the CPU profile and the
// mutex profile, writing cpu.prof and mutex.prof into dir. Both files are
// valid pprof protobufs even if a profile captured zero samples.
//
// Caveat: the CPU profile covers only this run; the mutex profile is
// cumulative since process start.
func Capture(dir string, work func()) (cpuPath, mutexPath string, err error) {
	cpuPath = filepath.Join(dir, "cpu.prof")
	mutexPath = filepath.Join(dir, "mutex.prof")

	cpuFile, err := os.Create(cpuPath)
	if err != nil {
		return "", "", fmt.Errorf("triage: create cpu profile: %w", err)
	}
	defer cpuFile.Close()

	if err := pprof.StartCPUProfile(cpuFile); err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrCPUProfileActive, err)
	}
	defer pprof.StopCPUProfile()

	prev := runtime.SetMutexProfileFraction(1)
	defer runtime.SetMutexProfileFraction(prev)

	work()

	mutexFile, err := os.Create(mutexPath)
	if err != nil {
		return "", "", fmt.Errorf("triage: create mutex profile: %w", err)
	}
	defer mutexFile.Close()
	if err := pprof.Lookup("mutex").WriteTo(mutexFile, 0); err != nil {
		return "", "", fmt.Errorf("triage: write mutex profile: %w", err)
	}
	return cpuPath, mutexPath, nil
}
```

Create `workloads.go`:

```go
package triage

import (
	"encoding/binary"
	"hash/fnv"
	"sync"
)

// WaitBound is the contention-dominated workload: goroutines workers fight for
// one lock held only long enough to increment a counter. Elapsed time goes to
// waiting, so the mutex profile lights up and the CPU profile stays quiet.
// It returns the final count for exact correctness assertions.
func WaitBound(goroutines, ops int) int {
	var (
		mu    sync.Mutex
		count int
		wg    sync.WaitGroup
	)
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range ops {
				mu.Lock()
				count++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return count
}

// CPUBound is the execution-dominated workload: one goroutine repeatedly runs
// an expensive checksum inside its critical section. Nobody waits on the lock,
// so the mutex profile stays quiet and the CPU profile shows checksum.
// It returns the final accumulator, which is deterministic for given ops.
func CPUBound(ops int) uint64 {
	var mu sync.Mutex
	var acc uint64
	for range ops {
		mu.Lock()
		acc = checksum(acc)
		mu.Unlock()
	}
	return acc
}

// checksum hashes 512 derived blocks — a stand-in for the serialization or
// compression work that makes real critical sections CPU-bound. The result is
// returned and used as the next seed, so the compiler cannot elide it.
func checksum(seed uint64) uint64 {
	h := fnv.New64a()
	var buf [8]byte
	for i := range 512 {
		binary.LittleEndian.PutUint64(buf[:], seed+uint64(i))
		h.Write(buf[:])
	}
	return h.Sum64()
}
```

### The runnable demo

The demo captures both profiles around the wait-bound workload and prints the
stable facts: the workload did all its increments and both files were written
non-empty. The human step it sets up is in the printed hint — open each file
with `go tool pprof` and compare where the time went.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	triage "example.com/profile-triage"
)

func main() {
	dir, err := os.MkdirTemp("", "triage-demo-*")
	if err != nil {
		fmt.Println("tempdir:", err)
		return
	}
	defer os.RemoveAll(dir)

	var total int
	cpuPath, mutexPath, err := triage.Capture(dir, func() {
		total = triage.WaitBound(16, 2000)
	})
	if err != nil {
		fmt.Println("capture:", err)
		return
	}

	cpuInfo, err := os.Stat(cpuPath)
	if err != nil {
		fmt.Println("stat:", err)
		return
	}
	mutexInfo, err := os.Stat(mutexPath)
	if err != nil {
		fmt.Println("stat:", err)
		return
	}

	fmt.Printf("wait-bound increments: %d\n", total)
	fmt.Printf("cpu.prof non-empty: %v\n", cpuInfo.Size() > 0)
	fmt.Printf("mutex.prof non-empty: %v\n", mutexInfo.Size() > 0)
	fmt.Println("read them with: go tool pprof <file>, then top and list")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
wait-bound increments: 32000
cpu.prof non-empty: true
mutex.prof non-empty: true
read them with: go tool pprof <file>, then top and list
```

### Tests

`TestCaptureWritesBothProfiles` runs the wait-bound workload under `Capture` and
asserts both files exist non-empty — the honest assertion, since a pprof file is
a valid protobuf even with zero samples, and asserting sample counts would chain
the test to the CI machine's scheduler. `TestDoubleCaptureGuard` starts an outer
CPU profile, then proves `Capture` refuses to nest with the sentinel.
`TestCaptureRestoresFraction` reads the fraction with the negative-argument form
before and after. `TestWorkloads` is the table: exact counts for `WaitBound`
under `-race`, determinism for `CPUBound`. Which stacks appear inside each
profile is the human step with `go tool pprof top` and `list` — documented, not
asserted. The profiler tests are deliberately not parallel: the CPU profiler and
the mutex fraction are process-global.

Create `triage_test.go`:

```go
package triage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"testing"
)

func TestCaptureWritesBothProfiles(t *testing.T) {
	// Process-global CPU profiler: not parallel.
	dir := t.TempDir()

	var total int
	cpuPath, mutexPath, err := Capture(dir, func() {
		total = WaitBound(16, 1500)
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if want := 16 * 1500; total != want {
		t.Fatalf("workload count = %d, want %d", total, want)
	}
	for _, path := range []string{cpuPath, mutexPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Size() == 0 {
			t.Fatalf("profile %s is empty", path)
		}
	}
}

func TestDoubleCaptureGuard(t *testing.T) {
	// Process-global CPU profiler: not parallel.
	outer, err := os.Create(filepath.Join(t.TempDir(), "outer.prof"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { outer.Close() })

	if err := pprof.StartCPUProfile(outer); err != nil {
		t.Fatalf("outer StartCPUProfile: %v", err)
	}
	t.Cleanup(pprof.StopCPUProfile)

	_, _, err = Capture(t.TempDir(), func() {})
	if !errors.Is(err, ErrCPUProfileActive) {
		t.Fatalf("nested Capture error = %v, want errors.Is(..., ErrCPUProfileActive)", err)
	}
}

func TestCaptureRestoresFraction(t *testing.T) {
	// Process-global mutex fraction: not parallel.
	before := runtime.SetMutexProfileFraction(-1)

	if _, _, err := Capture(t.TempDir(), func() { WaitBound(4, 100) }); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	if after := runtime.SetMutexProfileFraction(-1); after != before {
		t.Fatalf("mutex fraction after Capture = %d, want %d (leaked)", after, before)
	}
}

func TestWorkloads(t *testing.T) {
	t.Parallel()
	t.Run("wait-bound exact count", func(t *testing.T) {
		t.Parallel()
		tests := []struct{ goroutines, ops int }{
			{goroutines: 1, ops: 10},
			{goroutines: 8, ops: 500},
			{goroutines: 16, ops: 250},
		}
		for _, tt := range tests {
			if got, want := WaitBound(tt.goroutines, tt.ops), tt.goroutines*tt.ops; got != want {
				t.Fatalf("WaitBound(%d, %d) = %d, want %d", tt.goroutines, tt.ops, got, want)
			}
		}
	})
	t.Run("cpu-bound deterministic", func(t *testing.T) {
		t.Parallel()
		if CPUBound(8) != CPUBound(8) {
			t.Fatal("CPUBound is not deterministic for equal ops")
		}
		if CPUBound(8) == CPUBound(9) {
			t.Fatal("CPUBound checksum did not change with ops")
		}
	})
}

func ExampleCPUBound() {
	fmt.Println(CPUBound(4) == CPUBound(4))
	// Output: true
}
```

## Review

The harness is correct when one call yields both artifacts from the same
execution, refuses to nest inside an active CPU profile, and leaves the mutex
fraction exactly as it found it even if the workload panics — that is what the
three `defer`s in `Capture` buy. The reading discipline is the real skill: open
`cpu.prof` and `mutex.prof` side by side; if `top` in the mutex profile shows the
lock stack while the CPU profile barely mentions it, you are wait-bound and a
locking remedy applies; if the CPU profile shows `checksum` (or your real
serializer) hot while the mutex profile is quiet, the lock is innocent and you
must cheapen or relocate the work. The mistakes to avoid: capturing the two
profiles in separate runs and comparing across skew; swallowing the
`StartCPUProfile` error and shipping one-eyed evidence; forgetting the mutex
profile is cumulative since process start when you interpret it; and asserting
sample counts in CI — assert file non-emptiness and exact workload arithmetic,
and leave `top`/`list` to the human.

## Resources

- [runtime/pprof.StartCPUProfile](https://pkg.go.dev/runtime/pprof#StartCPUProfile) — the exclusive process-global CPU profiler and its already-active error.
- [Profiling Go Programs (Go blog)](https://go.dev/blog/pprof) — the canonical walkthrough of reading CPU profiles with top/list/web.
- [runtime/pprof](https://pkg.go.dev/runtime/pprof) — `Lookup` and `Profile.WriteTo` for the named mutex profile.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-lock-wait-slo-gauge.md](07-lock-wait-slo-gauge.md) | Next: [09-critical-section-shrink.md](09-critical-section-shrink.md)

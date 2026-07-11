# Exercise 2: Event-Triggered Capture with Cooldown and Dedup

An armed recorder is inert until something decides to snapshot. This exercise
turns it into a real flight recorder: HTTP middleware that times each request
and, when a request breaches the latency SLO or returns a 5xx, writes the last
few seconds of trace to a uniquely-named file — guarded by a cooldown so a bad
minute cannot write ten thousand snapshots and fill the disk.

This module is fully self-contained. It bundles a minimal recorder wrapper, so it
does not import Exercise 1, and it ships its own demo and tests.

## What you'll build

```text
tripwire/                  independent module: example.com/tripwire
  go.mod                   go 1.25 (FlightRecorder needs it)
  recorder.go              minimal Recorder: Arm, Enabled, Dump, Disarm
  tripwire.go              Tripwire middleware: SLO/5xx trigger, atomic cooldown, unique files
  cmd/
    demo/
      main.go              runnable demo: fast request writes nothing, slow burst writes one
  tripwire_test.go         httptest-driven: no-trigger, one-trigger, dedup, cooldown, unarmed
```

- Files: `recorder.go`, `tripwire.go`, `cmd/demo/main.go`, `tripwire_test.go`.
- Implement: a `Tripwire` with `Wrap(http.Handler) http.Handler` that snapshots on `elapsed >= slo || status >= 500`, gated by an atomic compare-and-swap cooldown, writing to `os.CreateTemp` files under a dump dir.
- Test: a fast handler produces zero files; one slow handler produces exactly one; a burst of slow requests within the cooldown still produces one (dedup); advancing past the cooldown produces a second; an unarmed recorder produces zero without panicking.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/tripwire/cmd/demo
cd ~/go-exercises/tripwire
go mod init example.com/tripwire
go mod edit -go=1.25
```

### The bundled recorder

Each exercise is standalone, so this one carries its own trimmed recorder — the
same shape as Exercise 1, minus the parts the middleware does not need. It arms a
single `trace.FlightRecorder`, reports `Enabled`, dumps the window, and disarms
idempotently.

Create `recorder.go`:

```go
// recorder.go
package tripwire

import (
	"errors"
	"io"
	"runtime/trace"
	"sync"
	"time"
)

// ErrNotArmed is returned by Dump when the recorder is not armed.
var ErrNotArmed = errors.New("tripwire: recorder not armed")

// Recorder is a minimal, service-owned wrapper around a flight recorder.
type Recorder struct {
	minAge time.Duration

	mu sync.Mutex
	fr *trace.FlightRecorder
}

// NewRecorder builds an inactive recorder with the given retention hint.
func NewRecorder(minAge time.Duration) *Recorder {
	return &Recorder{minAge: minAge}
}

// Arm starts recording. It returns the underlying Start error on failure.
func (r *Recorder) Arm() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fr != nil && r.fr.Enabled() {
		return nil
	}
	fr := trace.NewFlightRecorder(trace.FlightRecorderConfig{MinAge: r.minAge})
	if err := fr.Start(); err != nil {
		return err
	}
	r.fr = fr
	return nil
}

// Enabled reports whether the recorder is armed. Safe for concurrent use.
func (r *Recorder) Enabled() bool {
	r.mu.Lock()
	fr := r.fr
	r.mu.Unlock()
	return fr != nil && fr.Enabled()
}

// Dump snapshots the window into w, or returns ErrNotArmed if inactive.
func (r *Recorder) Dump(w io.Writer) (int64, error) {
	r.mu.Lock()
	fr := r.fr
	r.mu.Unlock()
	if fr == nil || !fr.Enabled() {
		return 0, ErrNotArmed
	}
	return fr.WriteTo(w)
}

// Disarm stops recording. Idempotent; blocks until any in-flight Dump finishes.
func (r *Recorder) Disarm() {
	r.mu.Lock()
	fr := r.fr
	r.fr = nil
	r.mu.Unlock()
	if fr != nil {
		fr.Stop()
	}
}
```

### The trigger, and why the cooldown is the whole point

The middleware wraps a handler, times it, and inspects the response status. The
trigger condition — `elapsed >= slo || status >= 500` — is easy. The hard part is
what happens next during an incident: a latency regression makes *every* request
slow, so the naive middleware fires the trigger on every request. That storm has
three failure modes, all bad: it fills the disk with near-identical snapshots, it
serializes on the single-in-flight-`WriteTo` constraint, and it can amplify the
outage by turning the diagnostic path into extra load.

The fix is a cooldown gate implemented with a single `atomic.Int64` holding the
Unix-nanosecond timestamp at which the next snapshot is allowed. On a trigger the
middleware reads the gate; if `now` is before it, it skips. Otherwise it attempts
a `CompareAndSwap` to push the gate forward by one cooldown; only the goroutine
whose CAS succeeds proceeds to snapshot, so a concurrent burst collapses to a
single write. This is lock-free and correct under `-race`: two goroutines that
observe the same old value both attempt the CAS, but exactly one succeeds and the
loser skips.

Snapshots go to `os.CreateTemp(dir, "trace-*.out")`, which atomically creates a
uniquely-named file — no two snapshots ever collide, and there is no filename
race to reason about.

The clock is injected as a `now func() time.Time` (defaulting to `time.Now`) so
the tests can drive latency and cooldown deterministically without real sleeps.
Status is captured with a small `http.ResponseWriter` wrapper that records the
first `WriteHeader` code, defaulting to 200 for handlers that only call `Write`.

Create `tripwire.go`:

```go
// tripwire.go
package tripwire

import (
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Tripwire is HTTP middleware that snapshots the flight recorder when a request
// is slow or fails, at most once per cooldown window.
type Tripwire struct {
	rec      *Recorder
	dir      string
	slo      time.Duration
	cooldown time.Duration

	now    func() time.Time // injectable clock; defaults to time.Now
	nextOK atomic.Int64     // Unix nanos: earliest time the next snapshot is allowed

	errMu   sync.Mutex
	lastErr error // most recent snapshot error, for observability
}

// New builds a Tripwire that writes snapshots under dir when a request takes at
// least slo or returns a 5xx, dropping repeat triggers within cooldown.
func New(rec *Recorder, dir string, slo, cooldown time.Duration) *Tripwire {
	return &Tripwire{
		rec:      rec,
		dir:      dir,
		slo:      slo,
		cooldown: cooldown,
		now:      time.Now,
	}
}

// statusRecorder captures the response status code for the trigger decision.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.written {
		s.status = code
		s.written = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.written {
		s.status = http.StatusOK
		s.written = true
	}
	return s.ResponseWriter.Write(b)
}

// Wrap returns next instrumented with the trigger.
func (t *Tripwire) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := t.now()
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sr, r)
		elapsed := t.now().Sub(start)
		if elapsed >= t.slo || sr.status >= 500 {
			t.trigger()
		}
	})
}

// trigger snapshots the window, unless the recorder is disarmed or the cooldown
// gate is closed. The atomic CAS guarantees at most one snapshot per cooldown.
func (t *Tripwire) trigger() {
	if !t.rec.Enabled() {
		return
	}
	now := t.now().UnixNano()
	gate := t.nextOK.Load()
	if now < gate {
		return // still cooling down
	}
	if !t.nextOK.CompareAndSwap(gate, now+t.cooldown.Nanoseconds()) {
		return // another goroutine won the race and is taking this snapshot
	}
	if err := t.snapshot(); err != nil {
		t.errMu.Lock()
		t.lastErr = err
		t.errMu.Unlock()
	}
}

// snapshot writes the current window to a uniquely-named file under the dump dir.
func (t *Tripwire) snapshot() error {
	f, err := os.CreateTemp(t.dir, "trace-*.out")
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = t.rec.Dump(f)
	return err
}

// LastError returns the most recent snapshot error, or nil if none has occurred.
func (t *Tripwire) LastError() error {
	t.errMu.Lock()
	defer t.errMu.Unlock()
	return t.lastErr
}
```

### The runnable demo

The demo uses the real clock and real (small) sleeps so you can watch the trigger
behave: a fast request writes nothing, then a burst of three slow requests writes
exactly one snapshot because the cooldown swallows the rest. It counts the files
in the dump directory after each phase.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"example.com/tripwire"
)

func countFiles(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return -1
	}
	return len(entries)
}

func get(url string) {
	resp, err := http.Get(url)
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func main() {
	rec := tripwire.NewRecorder(5 * time.Second)
	if err := rec.Arm(); err != nil {
		fmt.Println("arm:", err)
		return
	}
	defer rec.Disarm()

	dir, err := os.MkdirTemp("", "dumps-*")
	if err != nil {
		fmt.Println("mkdirtemp:", err)
		return
	}
	defer os.RemoveAll(dir)

	tw := tripwire.New(rec, dir, 10*time.Millisecond, time.Second)

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(30 * time.Millisecond)
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(tw.Wrap(h))
	defer srv.Close()

	get(srv.URL + "/fast")
	fmt.Println("fast request snapshots:", countFiles(dir))

	for range 3 {
		get(srv.URL + "/slow")
	}
	fmt.Println("slow burst snapshots:", countFiles(dir))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fast request snapshots: 0
slow burst snapshots: 1
```

### Tests

The deterministic tests inject a fake clock. A "slow" handler advances that clock
past the SLO, so latency is simulated with zero real waiting. `TestFastNoSnapshot`
drives a fast handler and asserts no files. `TestSlowWritesOne` drives one slow
request and asserts exactly one non-empty file. `TestServerErrorTriggers` exercises
the `status >= 500` branch: a fast request (1ms, well under the SLO) that returns a
500 must still write exactly one snapshot, proving the status trigger is independent
of latency. `TestDedupWithinCooldown` fires a
burst inside one cooldown and asserts still one file. `TestSecondAfterCooldown`
advances the clock past the cooldown and asserts a second file. `TestUnarmedSkips`
proves a disarmed recorder is skipped cleanly rather than panicking.
`TestConcurrentBurst` uses the real clock and real short sleeps to prove the
atomic CAS collapses a concurrent storm to a single file under `-race`. None of
these use `t.Parallel()`: each arms a flight recorder, and only one may be active
process-wide.

Create `tripwire_test.go`:

```go
// tripwire_test.go
package tripwire

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	return len(entries)
}

func get(t *testing.T, url string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// armedTripwire wires a fake clock into an armed recorder and a Tripwire whose
// SLO is 10ms and cooldown is one hour, so cooldown never lapses by accident.
func armedTripwire(t *testing.T, clk *fakeClock) (*Tripwire, string) {
	t.Helper()
	rec := NewRecorder(time.Second)
	if err := rec.Arm(); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	t.Cleanup(rec.Disarm)
	dir := t.TempDir()
	tw := New(rec, dir, 10*time.Millisecond, time.Hour)
	tw.now = clk.Now
	return tw, dir
}

// slowHandler advances the fake clock by d while serving, simulating latency.
func slowHandler(clk *fakeClock, d time.Duration, status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clk.advance(d)
		w.WriteHeader(status)
	})
}

func TestFastNoSnapshot(t *testing.T) {
	clk := newFakeClock()
	tw, dir := armedTripwire(t, clk)
	srv := httptest.NewServer(tw.Wrap(slowHandler(clk, time.Millisecond, http.StatusOK)))
	defer srv.Close()

	get(t, srv.URL)
	if n := countFiles(t, dir); n != 0 {
		t.Fatalf("fast request wrote %d snapshots; want 0", n)
	}
}

func TestSlowWritesOne(t *testing.T) {
	clk := newFakeClock()
	tw, dir := armedTripwire(t, clk)
	srv := httptest.NewServer(tw.Wrap(slowHandler(clk, 50*time.Millisecond, http.StatusOK)))
	defer srv.Close()

	get(t, srv.URL)
	if n := countFiles(t, dir); n != 1 {
		t.Fatalf("slow request wrote %d snapshots; want 1", n)
	}
	entries, _ := os.ReadDir(dir)
	info, err := entries[0].Info()
	if err != nil {
		t.Fatalf("stat snapshot: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("snapshot file is empty")
	}
	if err := tw.LastError(); err != nil {
		t.Fatalf("snapshot error: %v", err)
	}
}

func TestServerErrorTriggers(t *testing.T) {
	clk := newFakeClock()
	tw, dir := armedTripwire(t, clk)
	// Fast (1ms < SLO) but 500: the status branch must still trigger.
	srv := httptest.NewServer(tw.Wrap(slowHandler(clk, time.Millisecond, http.StatusInternalServerError)))
	defer srv.Close()

	get(t, srv.URL)
	if n := countFiles(t, dir); n != 1 {
		t.Fatalf("5xx request wrote %d snapshots; want 1", n)
	}
}

func TestDedupWithinCooldown(t *testing.T) {
	clk := newFakeClock()
	tw, dir := armedTripwire(t, clk)
	srv := httptest.NewServer(tw.Wrap(slowHandler(clk, 50*time.Millisecond, http.StatusOK)))
	defer srv.Close()

	for range 10 {
		get(t, srv.URL)
	}
	if n := countFiles(t, dir); n != 1 {
		t.Fatalf("burst of 10 slow requests wrote %d snapshots; want 1 (dedup)", n)
	}
}

func TestSecondAfterCooldown(t *testing.T) {
	clk := newFakeClock()
	tw, dir := armedTripwire(t, clk)
	srv := httptest.NewServer(tw.Wrap(slowHandler(clk, 50*time.Millisecond, http.StatusOK)))
	defer srv.Close()

	get(t, srv.URL) // first snapshot
	if n := countFiles(t, dir); n != 1 {
		t.Fatalf("after first trigger: %d snapshots; want 1", n)
	}
	clk.advance(2 * time.Hour) // past the one-hour cooldown
	get(t, srv.URL)            // second snapshot
	if n := countFiles(t, dir); n != 2 {
		t.Fatalf("after cooldown lapses: %d snapshots; want 2", n)
	}
}

func TestUnarmedSkips(t *testing.T) {
	clk := newFakeClock()
	rec := NewRecorder(time.Second) // never armed
	dir := t.TempDir()
	tw := New(rec, dir, 10*time.Millisecond, time.Hour)
	tw.now = clk.Now
	srv := httptest.NewServer(tw.Wrap(slowHandler(clk, 50*time.Millisecond, http.StatusOK)))
	defer srv.Close()

	get(t, srv.URL) // must not panic
	if n := countFiles(t, dir); n != 0 {
		t.Fatalf("unarmed recorder wrote %d snapshots; want 0", n)
	}
}

func TestConcurrentBurst(t *testing.T) {
	rec := NewRecorder(time.Second)
	if err := rec.Arm(); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	t.Cleanup(rec.Disarm)
	dir := t.TempDir()
	// Real clock, tiny SLO, long cooldown: every request breaches, but only one
	// snapshot should survive the cooldown gate.
	tw := New(rec, dir, time.Millisecond, time.Hour)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(tw.Wrap(h))
	defer srv.Close()

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			get(t, srv.URL)
		}()
	}
	wg.Wait()
	if n := countFiles(t, dir); n != 1 {
		t.Fatalf("concurrent burst wrote %d snapshots; want 1 (CAS dedup)", n)
	}
}
```

## Review

The middleware is correct when the trigger condition and the cooldown are both
honored: a snapshot is written exactly when `elapsed >= slo || status >= 500`
*and* the cooldown gate is open, and never more than once per cooldown window. The
mistakes worth calling out: firing `WriteTo` on every triggering request with no
gate — the storm that fills the disk and serializes on the single-in-flight
constraint; using a mutable filename instead of `os.CreateTemp`, so a second
snapshot clobbers the first; and treating a `Dump` error as fatal to the request
path — a snapshot failure should be recorded (here via `LastError`) and swallowed,
never allowed to break the user's request. Confirm the dedup with
`TestDedupWithinCooldown` and `TestConcurrentBurst` under `-race`: the CAS is the
only thing standing between a latency incident and a disk-full incident, and the
race detector is what proves the CAS is actually the serialization point rather
than a lucky interleaving.

## Resources

- [`runtime/trace` FlightRecorder](https://pkg.go.dev/runtime/trace#FlightRecorder) — `WriteTo`, `Enabled`, and the single-snapshot-at-a-time rule.
- [`sync/atomic.Int64.CompareAndSwap`](https://pkg.go.dev/sync/atomic#Int64.CompareAndSwap) — the lock-free cooldown gate.
- [`os.CreateTemp`](https://pkg.go.dev/os#CreateTemp) — atomically creates a uniquely-named dump file.
- [Flight recording in Go 1.25](https://go.dev/blog/flight-recorder) — trigger-on-event as the intended usage.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-armed-recorder-snapshot.md](01-armed-recorder-snapshot.md) | Next: [03-annotated-workload.md](03-annotated-workload.md)

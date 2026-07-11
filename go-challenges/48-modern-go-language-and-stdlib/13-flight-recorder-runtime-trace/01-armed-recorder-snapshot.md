# Exercise 1: An Armed Flight Recorder with On-Demand Snapshot

The building block behind everything else in this lesson is a small, correct
wrapper around `runtime/trace.FlightRecorder`: something a long-running service
arms once at boot and can snapshot on demand. This exercise builds that wrapper —
`Arm`, `Enabled`, `Dump`, `Disarm` — with the lifecycle modeled honestly, and
tests its behavior with pure stdlib.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
flightrec/                 independent module: example.com/flightrec
  go.mod                   go 1.25 (FlightRecorder needs it)
  recorder.go              type Recorder; New, Arm, Enabled, Dump, Disarm; sentinel errors
  cmd/
    demo/
      main.go              runnable demo: arm, do work, snapshot to a temp file
  recorder_test.go         lifecycle + snapshot tests (stdlib only, serial)
  recorder_tracecheck_test.go  //go:build tracecheck: parse the snapshot with x/exp/trace
```

- Files: `recorder.go`, `cmd/demo/main.go`, `recorder_test.go`, `recorder_tracecheck_test.go`.
- Implement: a `Recorder` wrapping `trace.FlightRecorder` with `Arm() error`, `Enabled() bool`, `Dump(io.Writer) (int64, error)`, and an idempotent `Disarm()`.
- Test: `Enabled` flips false→true→false across the lifecycle; `Dump` on an armed recorder returns `n>0`; `Dump` before arming returns `ErrNotArmed`; a second `Arm` returns `ErrAlreadyArmed`.
- Verify: `go test -count=1 -race ./...` (and, with the external reader, `go get golang.org/x/exp/trace && go test -tags tracecheck ./...`).

Set up the module. `runtime/trace.FlightRecorder` requires Go 1.25+, so pin the
language version:

```bash
mkdir -p ~/go-exercises/flightrec/cmd/demo
cd ~/go-exercises/flightrec
go mod init example.com/flightrec
go mod edit -go=1.25
```

### Why a wrapper, and what it must model

`trace.FlightRecorder` is deliberately low-level: `NewFlightRecorder`, `Start`,
`Enabled`, `WriteTo`, `Stop`. A service does not want to sprinkle those calls
around; it wants a single object it arms at boot and snapshots from a handler.
The wrapper's job is to encode the lifecycle rules from the concepts file so the
rest of the codebase cannot get them wrong:

- **Single active instance.** Only one recorder may be active process-wide. The
  wrapper refuses a second `Arm` while already armed, returning a sentinel
  `ErrAlreadyArmed` rather than letting the underlying `Start` fail with an opaque
  message. That is the "documented: error if already started" behavior.
- **Snapshot only when armed.** `WriteTo` errors if the recorder is inactive. The
  wrapper checks `Enabled()` first and returns a sentinel `ErrNotArmed`, so
  callers can branch on `errors.Is` instead of string-matching.
- **Idempotent teardown.** `Disarm` is safe to call whether or not the recorder
  is armed, and safe to call twice — exactly what a `defer` in `main` and a
  shutdown hook both want. Under the hood it calls `Stop`, which blocks until any
  in-flight `WriteTo` finishes, so a snapshot racing shutdown completes cleanly
  rather than being truncated.

One design decision worth stating: `Arm` constructs a *fresh* `FlightRecorder`
each time rather than reusing one across arm/disarm cycles. `Start` is
single-shot per recorder, so building a new one on each `Arm` is the clean way to
support arm → disarm → arm again without depending on undocumented restart
behavior.

### How Dump avoids holding the lock across the snapshot

`Dump` grabs the current `*FlightRecorder` under the mutex, releases the mutex,
and only then calls `WriteTo`. Holding the lock across `WriteTo` would serialize
snapshots against `Arm`/`Disarm` and could deadlock against `Stop` (which itself
waits for `WriteTo`). Releasing first means a concurrent `Disarm` can call `Stop`
while a `Dump` is mid-flight; that is safe by design — `Stop` blocks until the
snapshot completes, and if the recorder has already been stopped when `WriteTo`
runs, `WriteTo` simply returns its own inactive error, which `Dump` propagates.
`WriteTo` also allows only one snapshot at a time, so two concurrent `Dump` calls
result in one succeeding and the other receiving an in-progress error — the
caller decides whether that is fatal.

Create `recorder.go`:

```go
// recorder.go
package flightrec

import (
	"errors"
	"io"
	"runtime/trace"
	"sync"
	"time"
)

// Sentinel errors let callers branch with errors.Is instead of string matching.
var (
	// ErrNotArmed is returned by Dump when the recorder is not currently armed.
	ErrNotArmed = errors.New("flightrec: recorder not armed")
	// ErrAlreadyArmed is returned by Arm when the recorder is already armed.
	ErrAlreadyArmed = errors.New("flightrec: recorder already armed")
)

// Config mirrors the two FlightRecorder knobs. Both are hints: MaxBytes takes
// precedence over MinAge, and neither strictly bounds the snapshot or memory.
type Config struct {
	MinAge   time.Duration
	MaxBytes uint64
}

// Recorder is a service-owned wrapper around a single runtime/trace flight
// recorder. Arm it once at boot; Dump the window on demand; Disarm at shutdown.
type Recorder struct {
	cfg Config

	mu sync.Mutex
	fr *trace.FlightRecorder // non-nil only while armed
}

// New builds an inactive recorder. Call Arm to start recording.
func New(cfg Config) *Recorder {
	return &Recorder{cfg: cfg}
}

// Arm starts trace recording. It returns ErrAlreadyArmed if already armed, or
// the underlying Start error if the runtime refuses (for example, another
// flight recorder is active process-wide).
func (r *Recorder) Arm() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fr != nil && r.fr.Enabled() {
		return ErrAlreadyArmed
	}
	fr := trace.NewFlightRecorder(trace.FlightRecorderConfig{
		MinAge:   r.cfg.MinAge,
		MaxBytes: r.cfg.MaxBytes,
	})
	if err := fr.Start(); err != nil {
		return err
	}
	r.fr = fr
	return nil
}

// Enabled reports whether the recorder is currently armed. Safe for concurrent
// use; this is the natural check before attempting a Dump.
func (r *Recorder) Enabled() bool {
	r.mu.Lock()
	fr := r.fr
	r.mu.Unlock()
	return fr != nil && fr.Enabled()
}

// Dump snapshots the current trace window into w and returns the byte count.
// It returns ErrNotArmed if the recorder is not armed. Only one Dump may run at
// a time; a concurrent second call receives WriteTo's in-progress error.
func (r *Recorder) Dump(w io.Writer) (int64, error) {
	r.mu.Lock()
	fr := r.fr
	r.mu.Unlock()
	if fr == nil || !fr.Enabled() {
		return 0, ErrNotArmed
	}
	return fr.WriteTo(w)
}

// Disarm stops recording. It is idempotent: calling it when not armed, or twice,
// is a no-op. Stop blocks until any in-flight Dump completes.
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

### The runnable demo

The demo mirrors the real usage: arm the recorder at startup, generate a little
traceable activity, snapshot the window to a temp file, and report that the
snapshot was non-empty. The exact byte count is nondeterministic (it depends on
scheduler and GC events in the window), so the demo prints a boolean rather than
a number.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"os"
	"time"

	"example.com/flightrec"
)

func main() {
	rec := flightrec.New(flightrec.Config{MinAge: 5 * time.Second})
	if err := rec.Arm(); err != nil {
		fmt.Println("arm:", err)
		return
	}
	defer rec.Disarm()

	fmt.Println("armed:", rec.Enabled())

	generateActivity()

	f, err := os.CreateTemp("", "snapshot-*.trace")
	if err != nil {
		fmt.Println("createtemp:", err)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()

	n, err := rec.Dump(f)
	if err != nil {
		fmt.Println("dump:", err)
		return
	}
	fmt.Printf("snapshot bytes > 0: %v\n", n > 0)
}

// generateActivity produces a few scheduler and channel events so the window has
// something in it beyond the bracketing Sync events.
func generateActivity() {
	done := make(chan int, 1)
	go func() {
		sum := 0
		for i := range 1000 {
			sum += i
		}
		done <- sum
	}()
	<-done
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
armed: true
snapshot bytes > 0: true
```

### Tests

The tests are pure stdlib and deterministic. `TestLifecycle` checks that
`Enabled` is false before `Arm`, true after, and false again after `Disarm`, and
that `Disarm` is safe to call twice. `TestDumpWritesWindow` arms, generates
activity, snapshots into a `bytes.Buffer`, and asserts `n>0` with a non-empty
buffer. `TestDumpNotArmed` asserts `Dump` before arming returns `ErrNotArmed`.
`TestDoubleArm` asserts the second `Arm` returns `ErrAlreadyArmed`.

Note what these tests deliberately do *not* do: they never call `t.Parallel()`.
Only one flight recorder may be active process-wide, so two arming tests running
concurrently would make one `Arm` fail. Each test arms and disarms in sequence,
so the single-instance rule is never violated.

Create `recorder_test.go`:

```go
// recorder_test.go
package flightrec

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"
)

func generateActivity() {
	done := make(chan int, 1)
	go func() {
		sum := 0
		for i := range 1000 {
			sum += i
		}
		done <- sum
	}()
	<-done
}

func TestLifecycle(t *testing.T) {
	rec := New(Config{MinAge: time.Second})
	if rec.Enabled() {
		t.Fatal("Enabled true before Arm")
	}
	if err := rec.Arm(); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	if !rec.Enabled() {
		t.Fatal("Enabled false after Arm")
	}
	rec.Disarm()
	if rec.Enabled() {
		t.Fatal("Enabled true after Disarm")
	}
	rec.Disarm() // idempotent: must not panic
}

func TestDumpWritesWindow(t *testing.T) {
	rec := New(Config{MinAge: time.Second})
	if err := rec.Arm(); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	defer rec.Disarm()

	generateActivity()

	var buf bytes.Buffer
	n, err := rec.Dump(&buf)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if n <= 0 {
		t.Fatalf("Dump wrote %d bytes; want > 0", n)
	}
	if buf.Len() == 0 {
		t.Fatal("Dump reported bytes but buffer is empty")
	}
	if int64(buf.Len()) != n {
		t.Fatalf("Dump returned n=%d but buffer has %d bytes", n, buf.Len())
	}
}

func TestDumpNotArmed(t *testing.T) {
	rec := New(Config{MinAge: time.Second})
	var buf bytes.Buffer
	n, err := rec.Dump(&buf)
	if !errors.Is(err, ErrNotArmed) {
		t.Fatalf("Dump before Arm: err = %v; want ErrNotArmed", err)
	}
	if n != 0 {
		t.Fatalf("Dump before Arm wrote %d bytes; want 0", n)
	}
}

func TestDoubleArm(t *testing.T) {
	rec := New(Config{MinAge: time.Second})
	if err := rec.Arm(); err != nil {
		t.Fatalf("first Arm: %v", err)
	}
	defer rec.Disarm()
	if err := rec.Arm(); !errors.Is(err, ErrAlreadyArmed) {
		t.Fatalf("second Arm: err = %v; want ErrAlreadyArmed", err)
	}
}

func ExampleRecorder() {
	rec := New(Config{MinAge: time.Second})
	fmt.Println(rec.Enabled())
	_ = rec.Arm()
	fmt.Println(rec.Enabled())
	rec.Disarm()
	fmt.Println(rec.Enabled())
	// Output:
	// false
	// true
	// false
}
```

### Proving the window is a real trace

The stdlib tests prove the wrapper's contract, but they cannot prove the bytes
are a *structurally valid* trace — the parser lives in the experimental,
external `golang.org/x/exp/trace`. Keep that dependency behind a build tag so the
default build stays stdlib-only. With the tag on, feed the snapshot to
`trace.NewReader` and drain `ReadEvent` to `io.EOF`; a non-empty trace always
brackets itself with `Sync` events, so a valid window yields at least those.

Create `recorder_tracecheck_test.go`:

```go
// recorder_tracecheck_test.go
//go:build tracecheck

package flightrec

import (
	"bytes"
	"io"
	"testing"
	"time"

	exptrace "golang.org/x/exp/trace"
)

func TestSnapshotIsValidTrace(t *testing.T) {
	rec := New(Config{MinAge: time.Second})
	if err := rec.Arm(); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	defer rec.Disarm()

	generateActivity()

	var buf bytes.Buffer
	if _, err := rec.Dump(&buf); err != nil {
		t.Fatalf("Dump: %v", err)
	}

	r, err := exptrace.NewReader(&buf)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	var events int
	for {
		_, err := r.ReadEvent()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ReadEvent: %v", err)
		}
		events++
	}
	if events == 0 {
		t.Fatal("captured window contained no events")
	}
}
```

Run it (this one fetches the external module):

```bash
go get golang.org/x/exp/trace
go test -tags tracecheck ./...
```

## Review

The wrapper is correct when its four operations honor the lifecycle exactly:
`Arm` refuses a double-arm with `ErrAlreadyArmed`, `Dump` refuses an unarmed
recorder with `ErrNotArmed`, `Enabled` tracks the true active state, and `Disarm`
is idempotent and blocks on any in-flight snapshot through `Stop`. The most
common mistakes here are structural rather than syntactic. Do not hold the mutex
across `WriteTo` — that serializes snapshots against arm/disarm and can deadlock
against `Stop`, which itself waits for `WriteTo`. Do not run the arming tests with
`t.Parallel()`: the single-active-recorder rule means concurrent arms fail, and
the failure looks like a flaky test rather than the invariant it actually is. And
do not reuse one `FlightRecorder` across arm/disarm cycles — `Start` is
single-shot, so build a fresh one on each `Arm`. Confirm correctness by running
`go test -count=1 -race ./...`; the `-race` flag matters because `Enabled` and
`Dump` read `r.fr` under the mutex while `Arm`/`Disarm` write it.

## Resources

- [`runtime/trace` FlightRecorder](https://pkg.go.dev/runtime/trace#FlightRecorder) — `NewFlightRecorder`, `Start`, `Enabled`, `WriteTo`, `Stop`, and `FlightRecorderConfig`.
- [Flight recording in Go 1.25](https://go.dev/blog/flight-recorder) — the official introduction and the black-box model.
- [`golang.org/x/exp/trace`](https://pkg.go.dev/golang.org/x/exp/trace) — the experimental `NewReader`/`ReadEvent` used to parse a captured window.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-event-triggered-capture.md](02-event-triggered-capture.md)

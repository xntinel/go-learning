# Exercise 3: Deferred Function Literal that Instruments a Handler via Named Returns

The single most useful anonymous function in a backend is the deferred closure that
wraps a method: it reads the method's named return value, records how long the call
took, emits a structured log line with the outcome, and turns a panic into a
returned error so a bad row does not crash the process. This module builds that
wrapper around a repository `Save` and proves all three behaviors.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
saverepo/                     module example.com/saverepo
  go.mod
  repo.go                     Repo.Save(ctx, Record) (err error) with a deferred instrumenting closure
  repo_test.go                success records ok=true, panic becomes error, validation error observed
  cmd/demo/main.go            two Saves logged via slog
```

- Files: `repo.go`, `repo_test.go`, `cmd/demo/main.go`.
- Implement: `Save(ctx, Record) (err error)` whose single `defer func() { ... }()` reads the named `err`, converts a panic to an error via `recover`, records `time.Since` via an injected clock, and logs the outcome with `slog.LogAttrs`.
- Test: on success `err` is nil and the log records `ok=true` with the elapsed duration; a panicking write becomes a non-nil error wrapping `ErrPanic` and logs `ok=false`; a validation failure is observable through the named return.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/05-anonymous-functions/03-deferred-closure-named-return-observability/cmd/demo
cd go-solutions/04-functions/05-anonymous-functions/03-deferred-closure-named-return-observability
```

### Why the closure must capture the named return

`Save` declares a *named* return, `(err error)`. That name is a real variable in
the function's frame, and `defer` runs after that variable has been assigned its
final value but before `Save` returns to its caller. The deferred function literal
closes over `err` by reference, so it sees whatever `Save` last put there — the
validation error, the `nil` of the happy path, or the error the literal itself
assigns after `recover`. This is why instrumentation lives in a deferred closure
and why the return must be named: a bare `func Save(...) error` gives the closure
nothing to read or rewrite.

Three jobs share the one closure. It calls `recover()` *directly* in its own body —
the only place `recover` works — so a panic from a misbehaving driver becomes
`err = fmt.Errorf("%w: %v", ErrPanic, p)` instead of unwinding the goroutine. It
computes `r.clock().Sub(start)` for latency. And it emits one `slog` record whose
level and `ok` field are derived from the final `err`. Because the closure writes
to the named `err`, the recovered error is what the *caller* receives — the panic
is fully converted, not merely logged.

The clock is injected as `clock func() time.Time` (defaulting to `time.Now`) so a
test can make the elapsed duration deterministic. A `beforeWrite` hook, unexported
and `nil` in production, lets a same-package test inject a panic to exercise the
recover path without a real driver.

Create `repo.go`:

```go
package saverepo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// ErrInvalid marks a record rejected before any write.
var ErrInvalid = errors.New("invalid record")

// ErrPanic marks a write that panicked and was recovered into an error.
var ErrPanic = errors.New("save panicked")

// Record is a row to persist.
type Record struct {
	ID   string
	Data string
}

// Repo is an in-memory store whose Save is instrumented by a deferred closure.
type Repo struct {
	rows        map[string]string
	log         *slog.Logger
	clock       func() time.Time
	beforeWrite func() // test hook to simulate a panicking driver; nil in production
}

// New returns a Repo that logs to log and reads the wall clock via time.Now.
func New(log *slog.Logger) *Repo {
	return &Repo{rows: make(map[string]string), log: log, clock: time.Now}
}

func levelFor(err error) slog.Level {
	if err != nil {
		return slog.LevelError
	}
	return slog.LevelInfo
}

// Save persists rec. A single deferred closure reads the named return err,
// converts a panic into that error, times the call, and logs the outcome.
func (r *Repo) Save(ctx context.Context, rec Record) (err error) {
	start := r.clock()
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%w: %v", ErrPanic, p)
		}
		r.log.LogAttrs(ctx, levelFor(err), "repo.save",
			slog.String("id", rec.ID),
			slog.Bool("ok", err == nil),
			slog.Duration("elapsed", r.clock().Sub(start)),
		)
	}()

	if rec.ID == "" {
		return fmt.Errorf("%w: empty id", ErrInvalid)
	}
	if r.beforeWrite != nil {
		r.beforeWrite()
	}
	r.rows[rec.ID] = rec.Data
	return nil
}

// Get returns the stored data for id.
func (r *Repo) Get(id string) (string, bool) {
	v, ok := r.rows[id]
	return v, ok
}
```

### The runnable demo

The demo logs through a `slog` text handler whose `ReplaceAttr` drops the
nondeterministic `time` and `elapsed` fields, so the output is stable. It saves one
valid record (logged at INFO, `ok=true`) and one with an empty id (logged at ERROR,
`ok=false`), printing each returned error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"example.com/saverepo"
)

func main() {
	opts := &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey || a.Key == "elapsed" {
				return slog.Attr{}
			}
			return a
		},
	}
	r := saverepo.New(slog.New(slog.NewTextHandler(os.Stdout, opts)))
	ctx := context.Background()

	fmt.Println("saved:", r.Save(ctx, saverepo.Record{ID: "u1", Data: "alice"}))
	fmt.Println("rejected:", r.Save(ctx, saverepo.Record{ID: "", Data: "bob"}))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
level=INFO msg=repo.save id=u1 ok=true
saved: <nil>
level=ERROR msg=repo.save id="" ok=false
rejected: invalid record: empty id
```

### Tests

The tests capture the log into a `bytes.Buffer` and drive a fake clock that
advances a fixed 5ms per call, so `elapsed` is deterministic. `TestSuccess` asserts
`err` is nil, the record is stored, and the line records `ok=true elapsed=5ms`.
`TestPanicBecomesError` injects a panicking `beforeWrite`, asserts the returned
error wraps `ErrPanic` (so the caller sees it — proof the closure wrote the named
return), that the record was not stored, and that the line is ERROR with
`ok=false`. `TestValidationErrorObserved` asserts the empty-id path surfaces
`ErrInvalid` through the named return and logs `ok=false`.

Create `repo_test.go`:

```go
package saverepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time {
	f.t = f.t.Add(5 * time.Millisecond)
	return f.t
}

func newTestRepo(t *testing.T) (*Repo, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	r := New(slog.New(slog.NewTextHandler(&buf, nil)))
	r.clock = (&fakeClock{}).now
	return r, &buf
}

func TestSuccess(t *testing.T) {
	t.Parallel()
	r, buf := newTestRepo(t)

	if err := r.Save(context.Background(), Record{ID: "u1", Data: "alice"}); err != nil {
		t.Fatalf("Save = %v, want nil", err)
	}
	if v, ok := r.Get("u1"); !ok || v != "alice" {
		t.Fatalf("Get(u1) = %q,%v; want alice,true", v, ok)
	}
	line := buf.String()
	if !strings.Contains(line, "ok=true") {
		t.Fatalf("log line missing ok=true: %s", line)
	}
	if !strings.Contains(line, "elapsed=5ms") {
		t.Fatalf("log line missing elapsed=5ms: %s", line)
	}
}

func TestPanicBecomesError(t *testing.T) {
	t.Parallel()
	r, buf := newTestRepo(t)
	r.beforeWrite = func() { panic("driver connection reset") }

	err := r.Save(context.Background(), Record{ID: "u1", Data: "alice"})
	if !errors.Is(err, ErrPanic) {
		t.Fatalf("Save = %v, want error wrapping ErrPanic", err)
	}
	if _, ok := r.Get("u1"); ok {
		t.Fatal("record was stored despite the panic")
	}
	line := buf.String()
	if !strings.Contains(line, "level=ERROR") || !strings.Contains(line, "ok=false") {
		t.Fatalf("panic path did not log ERROR ok=false: %s", line)
	}
}

func TestValidationErrorObserved(t *testing.T) {
	t.Parallel()
	r, buf := newTestRepo(t)

	err := r.Save(context.Background(), Record{ID: "", Data: "bob"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Save = %v, want error wrapping ErrInvalid", err)
	}
	if !strings.Contains(buf.String(), "ok=false") {
		t.Fatalf("validation path did not log ok=false: %s", buf.String())
	}
}

func ExampleRepo_Save() {
	r := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	fmt.Println(r.Save(context.Background(), Record{ID: "u1", Data: "alice"}))
	// Output: <nil>
}
```

## Review

The instrumentation is correct when the deferred closure is the single point that
observes and finalizes the outcome. It reads the *named* `err`, so success, a
validation failure, and a recovered panic all flow through the same log line — the
tests prove `ok=true`/`ok=false` and the ERROR level track the final error. It
calls `recover()` directly in its own body, so the panic is converted rather than
propagated, and because it assigns the named return, the caller receives that error
(`errors.Is(err, ErrPanic)` in the test is the proof). The elapsed duration is real
`time.Since` work, made deterministic in tests only by injecting the clock — not by
abstracting time in production. The trap to avoid is moving `recover()` into a
helper the closure calls: it would return `nil` and the panic would keep unwinding.
Keep it in the deferred literal.

## Resources

- [Go blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover)
- [Go spec: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [log/slog: Logger.LogAttrs](https://pkg.go.dev/log/slog#Logger.LogAttrs)
- [time.Since](https://pkg.go.dev/time#Since)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-iife-immediate-invocation-scoped-init.md](02-iife-immediate-invocation-scoped-init.md) | Next: [04-lazy-init-oncevalue-resource.md](04-lazy-init-oncevalue-resource.md)

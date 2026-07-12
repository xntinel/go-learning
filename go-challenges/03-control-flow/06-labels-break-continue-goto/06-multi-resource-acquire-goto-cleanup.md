# Exercise 6: Acquire temp file + lock + handle with a goto unwind ladder

This is the one place `goto` is the honest tool: a function that acquires several
resources in order and, on any step's failure, releases only the ones already
acquired, in reverse order. You will write the C-style unwind ladder with `goto`,
then reimplement it with `defer` and compare. Along the way you meet the spec
restriction that a `goto` may not jump over an in-scope variable declaration.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
acquire/                   independent module: example.com/acquire
  go.mod                   go 1.24
  acquire.go               Resource, Deps, Pipeline; OpenGoto (ladder) + OpenDefer
  cmd/
    demo/
      main.go              runnable demo: success, then handle failure releases lock
  acquire_test.go          fault injection at each step; LIFO release; goto/defer parity
```

- Files: `acquire.go`, `cmd/demo/main.go`, `acquire_test.go`.
- Implement: `OpenGoto(deps)` that acquires a temp file, then a lock, then a handle, and on any failure `goto`-jumps to the label that releases exactly the resources acquired so far, in reverse order; plus `OpenDefer(deps)`, the same logic with `defer`.
- Test: fault-inject a failure at each acquisition step and assert exactly the earlier resources are released (spy closers count calls), the later ones are not, and the success path releases all in LIFO order; assert `OpenGoto` and `OpenDefer` behave identically.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The unwind ladder, and the declaration restriction

Acquire in order: temp file, then advisory lock, then downstream handle. If the
lock fails, only the temp file needs cleanup. If the handle fails, the lock and
the temp file need cleanup, in reverse order (lock first, then file). The ladder
encodes exactly this: each failure `goto`s the label matching the last resource
successfully acquired, and the labels fall through top to bottom, releasing in
reverse acquisition order.

```
cleanupLock:
	lock.Close()
cleanupFile:
	f.Close()
	os.Remove(f.Name())
	return nil, err
```

A handle failure jumps to `cleanupLock`, which closes the lock and then falls
through to `cleanupFile`. A lock failure jumps straight to `cleanupFile`, skipping
`cleanupLock` — so the lock, which was never acquired, is never closed. That is the
whole point: land on the right rung and fall the rest of the way down.

Now the spec restriction, which shapes how the function must be written: a `goto`
may not jump over the declaration of a variable that is still in scope at the
label. If `lock` and `handle` were declared with `:=` at their acquisition sites,
then `goto cleanupFile` (which happens before the `handle` line) would jump
*over* the declaration of `handle`, and `handle` is in scope at `cleanupFile`.
That is a compile error: `goto cleanupFile jumps over declaration of handle`. The
fix is to declare every variable the labels touch up front with `var`, before any
`goto`, so no jump crosses a declaration. This is not a workaround; it is the
idiom the restriction pushes you toward, and it makes the ladder's state explicit.

Create `acquire.go`:

```go
package acquire

import (
	"errors"
	"fmt"
	"os"
)

// Resource is any acquired thing that must be released with Close.
type Resource interface {
	Close() error
}

// Deps supplies the two acquisitions that follow the temp file. In production
// these dial a lock service and open a downstream handle; in tests they are
// fault-injectable.
type Deps struct {
	Lock   func() (Resource, error)
	Handle func() (Resource, error)
}

// Pipeline holds the three acquired resources on success.
type Pipeline struct {
	File   *os.File
	Lock   Resource
	Handle Resource
}

// Sentinel errors for the two fallible acquisitions.
var (
	ErrLock   = errors.New("acquire lock")
	ErrHandle = errors.New("acquire handle")
)

// OpenGoto acquires a temp file, then a lock, then a handle. On any failure it
// releases exactly the resources acquired so far, in reverse order, via a goto
// unwind ladder. The lock and handle are declared up front with var so no goto
// jumps over a declaration (which would not compile).
func OpenGoto(deps Deps) (*Pipeline, error) {
	var (
		lock   Resource
		handle Resource
		err    error
	)

	f, err := os.CreateTemp("", "pipeline-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create temp: %w", err)
	}

	lock, err = deps.Lock()
	if err != nil {
		err = fmt.Errorf("%w: %v", ErrLock, err)
		goto cleanupFile
	}

	handle, err = deps.Handle()
	if err != nil {
		err = fmt.Errorf("%w: %v", ErrHandle, err)
		goto cleanupLock
	}

	return &Pipeline{File: f, Lock: lock, Handle: handle}, nil

cleanupLock:
	lock.Close()
cleanupFile:
	f.Close()
	os.Remove(f.Name())
	return nil, err
}

// OpenDefer is the same logic written with defer: one guarded cleanup per
// resource, released LIFO when ok stays false. This is the modern idiom.
func OpenDefer(deps Deps) (*Pipeline, error) {
	f, err := os.CreateTemp("", "pipeline-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create temp: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(f.Name())
		}
	}()

	lock, err := deps.Lock()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLock, err)
	}
	defer func() {
		if !ok {
			lock.Close()
		}
	}()

	handle, err := deps.Handle()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandle, err)
	}

	ok = true
	return &Pipeline{File: f, Lock: lock, Handle: handle}, nil
}
```

The compile-fail case is worth stating precisely and is *not* something to test
(it would not build): if you wrote `lock, err := deps.Lock()` and `handle, err :=
deps.Handle()` with `:=` and kept the `goto cleanupFile`, the compiler rejects it
with `goto cleanupFile jumps over declaration of handle`. Hoisting the
declarations with `var` is what makes the ladder legal.

### The runnable demo

The demo injects an in-memory recorder so the release order is observable without
printing non-deterministic temp file names.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"os"

	"example.com/acquire"
)

type closer func() error

func (c closer) Close() error { return c() }

func main() {
	var released []string
	mk := func(name string) func() (acquire.Resource, error) {
		return func() (acquire.Resource, error) {
			return closer(func() error {
				released = append(released, name)
				return nil
			}), nil
		}
	}

	// Success: all three acquired, nothing released by the ladder.
	p, err := acquire.OpenGoto(acquire.Deps{Lock: mk("lock"), Handle: mk("handle")})
	if err != nil {
		fmt.Println("unexpected:", err)
		return
	}
	fmt.Println("acquired pipeline")
	p.Handle.Close()
	p.Lock.Close()
	p.File.Close()
	os.Remove(p.File.Name())
	released = released[:0] // reset before the failure demo

	// Failure at the handle step: the ladder releases only the lock.
	_, err = acquire.OpenGoto(acquire.Deps{
		Lock:   mk("lock"),
		Handle: func() (acquire.Resource, error) { return nil, errors.New("backend down") },
	})
	fmt.Printf("open failed: %v\n", err)
	fmt.Printf("released on failure: %v\n", released)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
acquired pipeline
open failed: acquire handle: backend down
released on failure: [lock]
```

Only `lock` is released on the handle failure: the temp file is released by the
ladder too, but through `os.Remove`, not the recorder. The handle was never
acquired, so it is never closed.

### Tests

The tests use a recorder that logs the name of every released resource, so the
assertions can pin *which* resources were released and in *what order*.
`TestReleaseOnStep` fault-injects a failure at each step and asserts exactly the
earlier resources are released, newest first. `TestParity` runs the same cases
through `OpenGoto` and `OpenDefer` and asserts identical release logs.

Create `acquire_test.go`:

```go
package acquire

import (
	"errors"
	"os"
	"slices"
	"testing"
)

type recorder struct {
	released []string
}

type res struct {
	name string
	rec  *recorder
}

func (r *res) Close() error {
	r.rec.released = append(r.rec.released, r.name)
	return nil
}

// mkDeps builds Deps whose lock/handle succeed unless named in failAt.
func mkDeps(rec *recorder, failAt string) Deps {
	acquire := func(name string) func() (Resource, error) {
		return func() (Resource, error) {
			if name == failAt {
				return nil, errors.New(name + " unavailable")
			}
			return &res{name: name, rec: rec}, nil
		}
	}
	return Deps{Lock: acquire("lock"), Handle: acquire("handle")}
}

// closeAll releases a successful pipeline so the test leaves no temp file behind.
func closeAll(p *Pipeline) {
	p.Handle.Close()
	p.Lock.Close()
	p.File.Close()
	os.Remove(p.File.Name())
}

func TestReleaseOnStep(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		failAt      string
		wantErr     error
		wantRelease []string
	}{
		{name: "lock fails", failAt: "lock", wantErr: ErrLock, wantRelease: nil},
		{name: "handle fails", failAt: "handle", wantErr: ErrHandle, wantRelease: []string{"lock"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &recorder{}
			p, err := OpenGoto(mkDeps(rec, tc.failAt))
			if p != nil {
				t.Fatal("expected nil pipeline on failure")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want it to wrap %v", err, tc.wantErr)
			}
			if !slices.Equal(rec.released, tc.wantRelease) {
				t.Fatalf("released = %v, want %v (reverse order, only acquired ones)", rec.released, tc.wantRelease)
			}
		})
	}
}

func TestSuccessAcquiresAll(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	p, err := OpenGoto(mkDeps(rec, ""))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if p == nil || p.Lock == nil || p.Handle == nil || p.File == nil {
		t.Fatal("expected a fully populated pipeline")
	}
	if len(rec.released) != 0 {
		t.Fatalf("released = %v on success, want none", rec.released)
	}
	closeAll(p)
}

func TestParity(t *testing.T) {
	t.Parallel()

	for _, failAt := range []string{"lock", "handle", ""} {
		gotoRec := &recorder{}
		pg, ge := OpenGoto(mkDeps(gotoRec, failAt))
		if pg != nil {
			closeAll(pg)
		}

		deferRec := &recorder{}
		pd, de := OpenDefer(mkDeps(deferRec, failAt))
		if pd != nil {
			closeAll(pd)
		}

		if (ge == nil) != (de == nil) {
			t.Fatalf("failAt=%q: goto err=%v defer err=%v disagree on success", failAt, ge, de)
		}
		if failAt != "" && !slices.Equal(gotoRec.released, deferRec.released) {
			t.Fatalf("failAt=%q: goto released %v, defer released %v", failAt, gotoRec.released, deferRec.released)
		}
	}
}
```

## Review

The ladder is correct when a failure at step N releases exactly steps 1..N-1 in
reverse order and nothing later, and when success releases nothing (the caller
owns the resources). The recorder-based tests pin both the set and the order:
`handle fails` must release `[lock]`, not `[handle, lock]` (the handle was never
acquired) and not `[lock, handle]` (wrong order). The `defer` version proves the
modern idiom produces the same behavior, which is why `defer` is what you reach
for in ordinary code — `goto` remains for hot paths and generated code that avoid
`defer`'s per-call cost. The declaration restriction is real and structural:
declaring `lock` and `handle` with `var` up front is what lets the `goto`s
compile.

## Resources

- [Go Specification: Goto statements](https://go.dev/ref/spec#Goto_statements) — the jump-over-declaration and jump-into-block rules.
- [os.CreateTemp](https://pkg.go.dev/os#CreateTemp) — the temp file acquisition and `File.Name`.
- [Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — why `defer` is the modern cleanup idiom.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-batch-import-labeled-continue.md](05-batch-import-labeled-continue.md) | Next: [07-config-validation-labeled-continue.md](07-config-validation-labeled-continue.md)

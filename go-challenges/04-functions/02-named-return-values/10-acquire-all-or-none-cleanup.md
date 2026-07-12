# Exercise 10: Partial-Acquisition Rollback with a Deferred Named-Err Guard

Setting up a pipeline means acquiring several resources in sequence — a temp file,
a connection, a lock. If the third acquisition fails, the first two must be
released, or you leak. The all-or-nothing guard is a deferred closure keyed on the
named `err`: on failure it releases exactly what was already acquired, in reverse
order, and hands back nothing.

This module is self-contained: its own `go mod init`, its own demo, its own tests.

## What you'll build

```text
pipeline/                   independent module: example.com/pipeline
  go.mod
  pipeline.go               Step; Pipeline; Open (deferred all-or-none unwind)
  cmd/demo/
    main.go                 runnable demo: a full open and a partial-failure unwind
  pipeline_test.go          success/no-cleanup, fail-at-2 releases 1, closed-exactly-once
```

- Files: `pipeline.go`, `cmd/demo/main.go`, `pipeline_test.go`.
- Implement: `Open(steps ...Step) (p *Pipeline, err error)` that acquires each step in order and, in a deferred closure keyed on the named `err`, releases exactly the resources already acquired if any step fails.
- Test: full success -> no cleanup, pipeline usable; failure at step 2 -> resource 1 released, resources 2+ never acquired, err wraps the failing step; each acquired resource closed exactly once.
- Verify: `go test -count=1 -race ./...`

### Release exactly what you grabbed

`Open` accumulates acquired resources onto the pipeline as it goes, and registers a
single deferred closure that only does something when the named `err` ends up
non-nil:

```go
func Open(steps ...Step) (p *Pipeline, err error) {
	p = &Pipeline{}
	defer func() {
		if err != nil {
			_ = p.Close() // release everything acquired so far
			p = nil       // hand back no half-built pipeline
		}
	}()
	for _, s := range steps {
		c, aerr := s.Acquire()
		if aerr != nil {
			return p, fmt.Errorf("acquire %s: %w", s.Name, aerr)
		}
		p.resources = append(p.resources, c)
	}
	return p, nil
}
```

The invariant is that `p.resources` holds exactly the resources acquired so far. On
a successful full run, `err` stays nil, the deferred closure does nothing, and the
caller gets a usable pipeline. If step N fails, the loop returns with a wrapped
error before appending step N's resource — so `p.resources` holds steps 1..N-1 —
and the deferred closure sees the non-nil `err`, calls `p.Close()` to release those
in reverse order, and sets `p = nil` so the caller cannot accidentally use a
half-built pipeline. Setting `p = nil` in the defer is itself only possible because
`p` is a named result; a defer cannot rewrite an anonymous return.

`Close` releases in reverse acquisition order (a lock acquired last is released
first) and joins any close errors with `errors.Join`, so a cleanup failure is
reported rather than dropped. Because the loop never appends a resource for the
failing step, that step is never acquired and never closed — no double-close, no
leak.

Create `pipeline.go`:

```go
package pipeline

import (
	"errors"
	"fmt"
	"io"
)

// Step is one acquisition in the setup sequence. Acquire returns a Closer to
// release the resource, or an error if it could not be acquired.
type Step struct {
	Name    string
	Acquire func() (io.Closer, error)
}

// Pipeline owns the resources acquired by Open, in acquisition order.
type Pipeline struct {
	resources []io.Closer
}

// Close releases every held resource in reverse acquisition order, joining any
// close errors so none is silently dropped.
func (p *Pipeline) Close() error {
	var errs []error
	for i := len(p.resources) - 1; i >= 0; i-- {
		if err := p.resources[i].Close(); err != nil {
			errs = append(errs, err)
		}
	}
	p.resources = nil
	return errors.Join(errs...)
}

// Open acquires each step in order. If any step fails, a deferred closure keyed on
// the named err releases exactly the resources already acquired and returns a nil
// pipeline — the all-or-nothing guard that prevents a leak on partial failure.
func Open(steps ...Step) (p *Pipeline, err error) {
	p = &Pipeline{}
	defer func() {
		if err != nil {
			_ = p.Close()
			p = nil
		}
	}()
	for _, s := range steps {
		c, aerr := s.Acquire()
		if aerr != nil {
			return p, fmt.Errorf("acquire %s: %w", s.Name, aerr)
		}
		p.resources = append(p.resources, c)
	}
	return p, nil
}
```

### The runnable demo

The demo opens a pipeline of three resources successfully, then opens one whose
middle step fails and prints how many resources were released by the unwind.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"io"

	"example.com/pipeline"
)

type res struct {
	name   string
	closed *[]string
}

func (r res) Close() error {
	*r.closed = append(*r.closed, r.name)
	return nil
}

func step(name string, closed *[]string, fail bool) pipeline.Step {
	return pipeline.Step{
		Name: name,
		Acquire: func() (io.Closer, error) {
			if fail {
				return nil, errors.New("unavailable")
			}
			return res{name: name, closed: closed}, nil
		},
	}
}

func main() {
	var okClosed []string
	p, err := pipeline.Open(
		step("tempfile", &okClosed, false),
		step("conn", &okClosed, false),
		step("lock", &okClosed, false),
	)
	fmt.Printf("full open: err=%v closedDuringOpen=%v\n", err, okClosed)
	_ = p.Close()
	fmt.Printf("after explicit close: %v\n", okClosed)

	var partClosed []string
	p2, err := pipeline.Open(
		step("tempfile", &partClosed, false),
		step("conn", &partClosed, true), // fails here
		step("lock", &partClosed, false),
	)
	fmt.Printf("partial open: err=%v nilPipeline=%v released=%v\n", err, p2 == nil, partClosed)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
full open: err=<nil> closedDuringOpen=[]
after explicit close: [lock conn tempfile]
partial open: err=acquire conn: unavailable nilPipeline=true released=[tempfile]
```

### Tests

The fake resource counts its own closes so the tests can prove each acquired
resource is closed exactly once and the failing step's resource is never acquired.

Create `pipeline_test.go`:

```go
package pipeline

import (
	"errors"
	"io"
	"strings"
	"testing"
)

type fakeResource struct {
	name   string
	closes int
}

func (r *fakeResource) Close() error {
	r.closes++
	return nil
}

func okStep(name string, r *fakeResource) Step {
	return Step{Name: name, Acquire: func() (io.Closer, error) { return r, nil }}
}

func failStep(name string) Step {
	return Step{Name: name, Acquire: func() (io.Closer, error) {
		return nil, errors.New("unavailable")
	}}
}

func TestOpenSuccessNoCleanup(t *testing.T) {
	t.Parallel()

	r1, r2 := &fakeResource{name: "a"}, &fakeResource{name: "b"}
	p, err := Open(okStep("a", r1), okStep("b", r2))
	if err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("Open returned nil pipeline on success")
	}
	if r1.closes != 0 || r2.closes != 0 {
		t.Fatalf("resources closed during a successful open: %d/%d", r1.closes, r2.closes)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if r1.closes != 1 || r2.closes != 1 {
		t.Fatalf("after Close: closes = %d/%d, want 1/1", r1.closes, r2.closes)
	}
}

func TestOpenPartialFailureUnwinds(t *testing.T) {
	t.Parallel()

	r1 := &fakeResource{name: "a"}
	p, err := Open(
		okStep("a", r1),
		failStep("b"), // fails at step 2
		okStep("c", &fakeResource{name: "c"}),
	)
	if err == nil {
		t.Fatal("Open: want error on partial failure")
	}
	if p != nil {
		t.Fatal("Open returned a non-nil pipeline after a failed acquisition")
	}
	if !strings.Contains(err.Error(), "acquire b") {
		t.Fatalf("error %q does not name the failing step", err.Error())
	}
	if r1.closes != 1 {
		t.Fatalf("resource a closed %d times, want exactly 1 (released by unwind)", r1.closes)
	}
}

func TestCloseIsIdempotentAfterUnwind(t *testing.T) {
	t.Parallel()

	r1 := &fakeResource{name: "a"}
	_, err := Open(okStep("a", r1), failStep("b"))
	if err == nil {
		t.Fatal("want error")
	}
	// The unwind released r1 once; there is no second owner to close it again.
	if r1.closes != 1 {
		t.Fatalf("resource a closed %d times, want exactly 1", r1.closes)
	}
}

func ExampleOpen() {
	r := &fakeResource{name: "a"}
	_, err := Open(okStep("a", r), failStep("b"))
	// a was released by the unwind, and the error names the failing step.
	_ = err
	// Output:
}
```

## Review

`Open` is correct when a full success leaves every resource open and the pipeline
usable, and any partial failure releases exactly the resources already acquired —
each closed exactly once, the failing step never acquired — while returning a nil
pipeline and an error naming the step. The all-or-nothing guarantee rides entirely
on the deferred closure keyed on the named `err`: it is the one place that can both
observe the failure and rewrite the returned `p` to nil, and it releases in reverse
order so a lock taken last is dropped first. The mistakes to avoid are appending the
resource before checking the acquisition error (which would try to close a nil or a
never-acquired resource) and a bare `defer p.Close()` that releases on the success
path too. Run `go test -race`, which also exercises the close accounting.

## Resources

- [`io.Closer`](https://pkg.go.dev/io#Closer)
- [`errors.Join`](https://pkg.go.dev/errors#Join)
- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-result-struct-over-positional.md](09-result-struct-over-positional.md) | Next: [11-multi-field-validation-joined-errors.md](11-multi-field-validation-joined-errors.md)

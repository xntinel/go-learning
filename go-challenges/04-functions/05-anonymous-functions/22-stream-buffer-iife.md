# Exercise 22: Streaming Buffer Processor Initialized via IIFE

**Nivel: Intermedio** — validacion rapida (un test corto).

Wiring up a small streaming pipeline — an input channel, an output channel, and the
worker goroutine that connects them — is setup logic that a constructor needs to run
once and never again, and none of those channels belong in the constructor's own
named locals once the pipeline exists. This module builds that pipeline inside an
IIFE so the setup is visually boxed off and only the finished `*Processor` escapes.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
streambuf/                    module example.com/streambuf
  go.mod
  streambuf.go                  Processor, New (IIFE-built), Send, Close, Results, Done
  streambuf_test.go              order preserved, empty input, handle applied to every value
  cmd/demo/main.go              send five values, collect squared results
```

- Files: `streambuf.go`, `streambuf_test.go`, `cmd/demo/main.go`.
- Implement: `New(bufSize, handle)` assembling buffered `in`/`out` channels and a worker goroutine inside an IIFE, returning only the finished `*Processor`; `Send`, `Close`, `Results`, `Done`.
- Test: values sent before `Close` are returned by `Results` in order, transformed by `handle`; an empty stream still closes `Results` cleanly; `handle` runs on every sent value exactly once.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Boxing the setup in an IIFE

`New`'s entire body is one immediately invoked function literal: `func() *Processor
{ ... }()`. Inside it, `in`, `out`, and `done` are created, the worker goroutine is
launched closing over them, and the finished `*Processor` is returned — all as one
unit. None of `in`, `out`, or `done` is ever a local variable of `New` itself; they
exist only inside the literal's own scope, and the only thing that crosses back into
`New`'s caller is the `*Processor` value the literal returns. That is a small
difference from writing the same three lines directly in `New`'s body — the behavior
is identical either way — but the IIFE makes "this block is the construction step,
and nothing here is meant to be reused elsewhere in `New`" visible at a glance,
which matters more as a constructor's setup grows past a couple of lines.

The worker itself is a single goroutine reading `in` and writing `out`, so a
`Processor` processes values strictly in the order they were sent — no partitioning,
no concurrency inside one `Processor` — which is what makes `Results` order-preserving
without any extra bookkeeping.

Create `streambuf.go`:

```go
package streambuf

// Processor is a single-worker streaming pipeline: values sent on Send are
// transformed by a handler and become available on Results, in order.
type Processor struct {
	in   chan int
	out  chan int
	done chan struct{}
}

// New builds a Processor whose buffered channels and background worker are
// assembled inside an immediately invoked function literal (IIFE). The
// channels, and the worker goroutine that reads one and writes the other,
// are local to that literal — nothing about them leaks into New's own
// scope, only the finished *Processor escapes. That keeps New's body free
// of setup-only locals that would otherwise linger for no reason once
// construction is done.
func New(bufSize int, handle func(int) int) *Processor {
	return func() *Processor {
		in := make(chan int, bufSize)
		out := make(chan int, bufSize)
		done := make(chan struct{})

		go func() {
			defer close(out)
			defer close(done)
			for v := range in {
				out <- handle(v)
			}
		}()

		return &Processor{in: in, out: out, done: done}
	}()
}

// Send enqueues v for processing. Callers must call Close when done sending.
func (p *Processor) Send(v int) { p.in <- v }

// Close signals no more values will be sent; the worker drains the
// remainder and then closes Results.
func (p *Processor) Close() { close(p.in) }

// Results yields processed values in the order they were sent. It closes
// once the worker has drained everything after Close.
func (p *Processor) Results() <-chan int { return p.out }

// Done is closed once the worker goroutine has exited.
func (p *Processor) Done() <-chan struct{} { return p.done }
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/streambuf"
)

func main() {
	p := streambuf.New(8, func(v int) int { return v * v })

	for i := 1; i <= 5; i++ {
		p.Send(i)
	}
	p.Close()

	var out []int
	for v := range p.Results() {
		out = append(out, v)
	}
	<-p.Done()

	fmt.Println("results:", out)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
results: [1 4 9 16 25]
```

### Tests

`TestProcessorPreservesOrder` sends six values and checks the doubled results come
back in the same order. `TestProcessorWithNoInput` closes immediately and checks
`Results` yields nothing and closes cleanly. `TestProcessorAppliesHandleToEveryValue`
sends twenty values and checks every one was transformed exactly once.

Create `streambuf_test.go`:

```go
package streambuf

import (
	"slices"
	"testing"
)

func TestProcessorPreservesOrder(t *testing.T) {
	t.Parallel()
	p := New(4, func(v int) int { return v * 2 })

	for i := 1; i <= 6; i++ {
		p.Send(i)
	}
	p.Close()

	var got []int
	for v := range p.Results() {
		got = append(got, v)
	}
	<-p.Done()

	want := []int{2, 4, 6, 8, 10, 12}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestProcessorWithNoInput(t *testing.T) {
	t.Parallel()
	p := New(2, func(v int) int { return v })
	p.Close()

	var got []int
	for v := range p.Results() {
		got = append(got, v)
	}
	<-p.Done()

	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

func TestProcessorAppliesHandleToEveryValue(t *testing.T) {
	t.Parallel()
	p := New(16, func(v int) int { return -v })

	for i := 0; i < 20; i++ {
		p.Send(i)
	}
	p.Close()

	var got []int
	for v := range p.Results() {
		got = append(got, v)
	}
	<-p.Done()

	if len(got) != 20 {
		t.Fatalf("len(got) = %d, want 20", len(got))
	}
	for i, v := range got {
		if v != -i {
			t.Fatalf("got[%d] = %d, want %d", i, v, -i)
		}
	}
}
```

## Review

The processor is correct when every sent value comes back exactly once, transformed,
and in the order it was sent — guaranteed here by a single worker goroutine, not by
any ordering guarantee `Results` would otherwise need to reconstruct. The IIFE in
`New` changes nothing about that behavior; what it buys is scope discipline; `in`,
`out`, and `done` cannot accidentally be referenced from anywhere in `New` except
inside the literal that creates them, which is exactly the set of places that should
ever see them before they are wrapped in a `*Processor`.

## Resources

- [Go Language Specification: Function literals](https://go.dev/ref/spec#Function_literals)
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [21-oncefunc-signal-handler.md](21-oncefunc-signal-handler.md) | Next: [23-deadline-afterfunc-enforcement.md](23-deadline-afterfunc-enforcement.md)

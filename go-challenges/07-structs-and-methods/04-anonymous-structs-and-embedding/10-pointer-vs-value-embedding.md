# Exercise 10: Value vs Pointer Embedding and the Nil-Promotion Panic

Whether you embed a component by value or by pointer changes two things that bite
in production: mutation semantics (independent copy vs shared alias) and safety (a
nil pointer-embed panics the moment a promoted method is called). This exercise
builds two wrappers over a shared logger-like component to make both differences
concrete, and a constructor that guarantees the pointer-embed is never nil.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
embedlog/                   independent module: example.com/embedlog
  go.mod                    module example.com/embedlog
  embedlog.go               Logger; ValueWrapper (embeds Logger), PointerWrapper (embeds *Logger)
  cmd/
    demo/
      main.go               show independent-copy vs shared-alias counts
  embedlog_test.go          value-copy independence, pointer sharing, nil panic, safe constructor
```

Files: `embedlog.go`, `cmd/demo/main.go`, `embedlog_test.go`.
Implement: a `Logger` with a pointer-receiver `Log` that increments a counter, a
`ValueWrapper` embedding `Logger` by value, a `PointerWrapper` embedding `*Logger`,
and `NewPointerWrapper` that guarantees a non-nil embedded pointer.
Test: mutating a copy of a value-embedded wrapper does not affect the original,
while mutating a copy of a pointer-embedded wrapper does; calling a promoted method
on a nil pointer-embed panics (asserted with `recover`); the constructor prevents
the nil case so promoted calls are safe.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/embedlog/cmd/demo
cd ~/go-exercises/embedlog
go mod init example.com/embedlog
```

### Copy vs alias, and the nil trap

`Logger` holds a call counter and its last line, mutated through a pointer-receiver
`Log`. Embed it two ways. `ValueWrapper` embeds `Logger` by *value*: the wrapper
contains its own `Logger`, so copying a `ValueWrapper` copies the logger too, and
the copy's counter moves independently of the original's. `PointerWrapper` embeds
`*Logger`: the wrapper holds a *pointer*, so copying a `PointerWrapper` copies the
pointer, and both copies share one `Logger` — a `Log` through either is visible
through the other. That is the mutation-semantics difference in one sentence: value
embedding gives independent state, pointer embedding gives shared state.

Pointer embedding carries a specific hazard. A `PointerWrapper{}` built without a
constructor has a *nil* embedded `*Logger`. The promoted `Log` is still in the
method set, so `w.Log("x")` compiles — and then dereferences the nil pointer at
runtime and panics. This is a classic production crash: the type looks usable, the
call compiles, and it blows up on first use. The fix is a constructor,
`NewPointerWrapper`, that always allocates the `Logger`, so every promoted call is
safe. Value embedding does not have this trap: the embedded `Logger`'s zero value
is a usable `Logger`, so even `ValueWrapper{}` can log.

One method-set note the compiler will enforce: `Log` has a pointer receiver, so
for `ValueWrapper` it is promoted onto `*ValueWrapper` (and callable on any
addressable `ValueWrapper` variable), but a non-addressable `ValueWrapper` value
would not have it in its method set. The tests use addressable variables, so the
promoted `Log` is always callable.

Create `embedlog.go`:

```go
package embedlog

// Logger is a minimal component: it counts calls and remembers the last line.
type Logger struct {
	prefix string
	count  int
	last   string
}

// Log records a line. Pointer receiver: it mutates the Logger.
func (l *Logger) Log(msg string) {
	l.count++
	l.last = l.prefix + msg
}

// Count reports how many lines were logged.
func (l *Logger) Count() int { return l.count }

// Last reports the most recent formatted line.
func (l *Logger) Last() string { return l.last }

// ValueWrapper embeds Logger BY VALUE: each wrapper owns an independent Logger.
type ValueWrapper struct {
	Logger
	name string
}

// PointerWrapper embeds *Logger BY POINTER: copies share one Logger, and a nil
// pointer panics on a promoted call.
type PointerWrapper struct {
	*Logger
	name string
}

// NewPointerWrapper guarantees the embedded pointer is non-nil, so promoted
// calls are always safe.
func NewPointerWrapper(name, prefix string) *PointerWrapper {
	return &PointerWrapper{Logger: &Logger{prefix: prefix}, name: name}
}
```

### The runnable demo

The demo logs once through each wrapper, copies each, logs again through the copy,
and prints the counts — showing the value copy stays independent while the pointer
copy shares state.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/embedlog"
)

func main() {
	// Value embedding: copy is independent.
	v := embedlog.ValueWrapper{}
	v.Log("a")
	vCopy := v
	vCopy.Log("b")
	fmt.Printf("value: original=%d copy=%d\n", v.Count(), vCopy.Count())

	// Pointer embedding: copy shares the Logger.
	p := embedlog.NewPointerWrapper("svc", "[svc] ")
	p.Log("a")
	pCopy := *p
	pCopy.Log("b")
	fmt.Printf("pointer: original=%d copy=%d\n", p.Count(), pCopy.Count())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
value: original=1 copy=2
pointer: original=2 copy=2
```

### Tests

The tests pin the copy/alias difference, the nil-promotion panic (via `recover`),
and that the constructor prevents the nil case.

Create `embedlog_test.go`:

```go
package embedlog

import "testing"

func TestValueEmbedCopyIsIndependent(t *testing.T) {
	t.Parallel()

	v := ValueWrapper{}
	v.Log("a")
	// cp copies the embedded Logger, so its counter is independent.
	cp := v
	cp.Log("b")

	if v.Count() != 1 {
		t.Fatalf("original count = %d, want 1 (value copy must be independent)", v.Count())
	}
	if cp.Count() != 2 {
		t.Fatalf("copy count = %d, want 2", cp.Count())
	}
}

func TestPointerEmbedCopyIsShared(t *testing.T) {
	t.Parallel()

	p := NewPointerWrapper("svc", "")
	p.Log("a")
	// cp copies the pointer, so it shares the same Logger as p.
	cp := *p
	cp.Log("b")

	if p.Count() != 2 {
		t.Fatalf("original count = %d, want 2 (pointer copy shares state)", p.Count())
	}
	if cp.Count() != 2 {
		t.Fatalf("copy count = %d, want 2", cp.Count())
	}
}

func TestNilPointerEmbedPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("calling a promoted method on a nil pointer-embed did not panic")
		}
	}()
	var w PointerWrapper // embedded *Logger is nil
	w.Log("boom")        // promoted call dereferences nil -> panic
}

func TestConstructorPreventsNilPanic(t *testing.T) {
	t.Parallel()

	w := NewPointerWrapper("svc", "[svc] ")
	w.Log("safe") // must not panic
	if w.Count() != 1 {
		t.Fatalf("count = %d, want 1", w.Count())
	}
	if w.Last() != "[svc] safe" {
		t.Fatalf("last = %q, want [svc] safe", w.Last())
	}
}
```

## Review

The two wrappers are correct when copying a value-embedded wrapper leaves the
original's counter untouched while copying a pointer-embedded one shares it, and
when a nil pointer-embed panics on a promoted call but a constructed one does not.
The production lesson is the nil trap: a pointer-embed compiles and looks usable
while being a latent crash, so pointer embedding always demands a constructor that
guarantees non-nil. The mistakes to avoid: embedding by pointer and exposing a
zero value that panics on first use; assuming a value-embedded copy shares state
(it does not); and expecting a pointer-receiver method promoted from a
value-embedded field to be in a non-addressable value's method set. Run
`go test -race` to confirm the shared pointer case has no data race in these
single-goroutine tests, and to keep the module honest under future concurrency.

## Resources

- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types) — value vs pointer embedded fields.
- [Go Specification: Method sets](https://go.dev/ref/spec#Method_sets) — how pointer-receiver methods promote through value and pointer embeds.
- [Effective Go: Pointers vs. Values](https://go.dev/doc/effective_go#pointers_vs_values) — when a method needs a pointer receiver.

---

Prev: [09-anonymous-struct-webhook-handler.md](09-anonymous-struct-webhook-handler.md) | Back to [00-concepts.md](00-concepts.md) | Next: [../05-struct-comparison-and-equality/00-concepts.md](../05-struct-comparison-and-equality/00-concepts.md)

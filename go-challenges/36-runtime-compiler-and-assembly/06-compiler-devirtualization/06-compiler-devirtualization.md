# 6. Compiler Devirtualization

Every Go interface call goes through an indirection: the runtime fetches a function pointer from the interface table (itab) and invokes it. That pointer chase prevents inlining and adds measurable overhead on hot paths. Devirtualization is the compiler optimization that eliminates the indirection by proving (statically) or guessing (with PGO profile data) the concrete type behind the interface and replacing the indirect call with a direct, potentially inlinable, call. The hard part is knowing which code patterns give the compiler enough information to devirtualize and which patterns permanently block it.

```text
devirt/
  go.mod
  dispatch.go
  dispatch_test.go
  cmd/demo/main.go
```

## Concepts

### How Interface Dispatch Works

A Go interface value is a two-word pair: an itab pointer (carrying the concrete type and a table of method pointers) and a data pointer (the concrete value). A call through an interface at machine level is:

1. Load the itab pointer from the interface value.
2. Load the method's function pointer from the itab slot.
3. Execute an indirect call through that pointer.

The indirect call instruction cannot be inlined by the compiler because the target address is only known at runtime. The itab pointer also forces a memory load sequence that, while CPU-cached on hot paths, still adds latency compared to a direct call.

### Static Devirtualization

The Go compiler performs static devirtualization when it can prove the concrete type at the call site. The most common trigger is when the concrete value is assigned to an interface in the same function (or an inlined caller): the compiler sees that only one type can inhabit the interface at that point, so it replaces the `InterCall` (indirect call in SSA) with a `StaticCall` (direct call). A `StaticCall` can be inlined; once inlined, further optimizations like escape analysis, constant folding, and bounds-check elimination can fire on the callee's body.

The compiler reports devirtualization decisions under `-gcflags='-m -m'`:

```
./dispatch.go:14:14: devirtualizing w.Write to *NullWriter
```

The key constraint: the concrete type must be visible in the SSA graph at the call site. If it flows in through a function parameter, a channel receive, a map lookup, or a return from another function that the compiler does not inline, the concrete type is lost and devirtualization fails.

### PGO-Assisted Devirtualization

Profile-Guided Optimization (PGO), introduced for general use in Go 1.21, extends devirtualization to call sites where the static compiler cannot prove a concrete type. The compiler reads a CPU pprof profile (`default.pgo` in the main package directory, or specified with `-pgo=<path>`) that records which concrete types actually appear at each interface call site. If one type dominates the profile, the compiler generates a guarded direct call:

```go
// conceptual transformation the compiler applies:
if concreteType, ok := iface.(*ConcreteType); ok {
    concreteType.Method()  // direct call, can be inlined
} else {
    iface.Method()         // fallback indirect call
}
```

The profile does not guarantee exclusivity, so the fallback path is always present. The fast path (the type-check branch) is eligible for further inlining and escape-analysis improvements.

Go 1.21 release notes confirm: "PGO builds can now devirtualize some interface method calls, adding a concrete call to the most common callee."

### Reading SSA Output

The `GOSSAFUNC` environment variable dumps the SSA for a named function into `ssa.html`. The two SSA operations to look for are:

- `InterCall` — an indirect call through an interface; the target is unknown at compile time.
- `StaticCall` — a direct call to a known function; eligible for inlining.

After devirtualization, an `InterCall` becomes a `StaticCall` in the late-optimization passes. Open `ssa.html` in a browser and compare the `devirtualize` phase with the `opt` phase to trace the transformation.

### What Prevents Devirtualization

These patterns permanently prevent static devirtualization (PGO may recover some of them if the profile is available):

- Interface value received through a function parameter.
- Interface value stored in a struct field and read back later.
- Interface value received from a channel, map, or slice element.
- Interface value returned from a non-inlined callee.
- Interface value stored in a global variable.

The common thread: any path that hides the concrete type from the compiler's view of the call site.

## Exercises

### Exercise 1: A Package With Direct and Indirect Call Sites

Create `dispatch.go`:

```go
package devirt

import (
	"fmt"
	"io"
)

// Sink is the interface under study.
type Sink interface {
	Write(p []byte) (int, error)
}

// NullSink discards every byte. Its Write is small enough to inline once
// devirtualized.
type NullSink struct{}

func (NullSink) Write(p []byte) (int, error) { return len(p), nil }

// CountSink counts bytes written.
type CountSink struct{ N int }

func (c *CountSink) Write(p []byte) (int, error) {
	c.N += len(p)
	return len(p), nil
}

// WriteLocal creates the interface value locally. The compiler sees that only
// NullSink can inhabit w at the call site, so it devirtualizes w.Write.
func WriteLocal(data []byte) (int, error) {
	var w Sink = NullSink{}
	return w.Write(data)
}

// WriteParam receives an opaque interface. The concrete type is unknown, so
// the compiler cannot devirtualize without PGO.
func WriteParam(w Sink, data []byte) (int, error) {
	return w.Write(data)
}

// WriteTo wraps WriteParam for the demo and tests.
func WriteTo(w io.Writer, data []byte) (int, error) {
	n, err := w.Write(data)
	if err != nil {
		return 0, fmt.Errorf("devirt: write: %w", err)
	}
	return n, nil
}
```

`WriteLocal` is the devirtualization target: the concrete type `NullSink` is assigned in the same function, so the compiler can prove it. `WriteParam` receives an opaque `Sink`; without PGO data the compiler cannot devirtualize it.

### Exercise 2: Observe Devirtualization in Compiler Output

From `~/go-exercises/devirt`:

```bash
go build -gcflags='-m -m' ./... 2>&1 | grep -i devirtualiz
```

You should see a line mentioning `WriteLocal` and `NullSink`. You will not see `WriteParam` in that output because the type is unknown there.

To dump the SSA for `WriteLocal`:

```bash
GOSSAFUNC=WriteLocal go build ./... 2>&1
```

Open `ssa.html` in a browser. In the `devirtualize` pass, the `InterCall` node for `w.Write` is replaced by a `StaticCall` targeting `NullSink.Write` directly.

### Exercise 3: Tests and Benchmarks

Create `dispatch_test.go`:

```go
package devirt

import (
	"bytes"
	"fmt"
	"testing"
)

func TestWriteLocalDiscards(t *testing.T) {
	t.Parallel()

	n, err := WriteLocal([]byte("hello"))
	if err != nil {
		t.Fatalf("WriteLocal error = %v", err)
	}
	if n != 5 {
		t.Fatalf("WriteLocal returned n=%d, want 5", n)
	}
}

func TestWriteParamCountSink(t *testing.T) {
	t.Parallel()

	cs := &CountSink{}
	n, err := WriteParam(cs, []byte("hello world"))
	if err != nil {
		t.Fatalf("WriteParam error = %v", err)
	}
	if n != 11 {
		t.Fatalf("WriteParam returned n=%d, want 11", n)
	}
	if cs.N != 11 {
		t.Fatalf("CountSink.N = %d, want 11", cs.N)
	}
}

func TestWriteToBuffer(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	n, err := WriteTo(&buf, []byte("devirt"))
	if err != nil {
		t.Fatalf("WriteTo error = %v", err)
	}
	if n != 6 {
		t.Fatalf("WriteTo returned n=%d, want 6", n)
	}
	if got := buf.String(); got != "devirt" {
		t.Fatalf("buf = %q, want %q", got, "devirt")
	}
}

func TestWriteParamNullSink(t *testing.T) {
	t.Parallel()

	n, err := WriteParam(NullSink{}, []byte("anything"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 8 {
		t.Fatalf("n = %d, want 8", n)
	}
}

func ExampleWriteLocal() {
	n, err := WriteLocal([]byte("hello"))
	if err != nil {
		panic(err)
	}
	fmt.Printf("wrote %d bytes\n", n)
	// Output: wrote 5 bytes
}

// BenchmarkWriteLocal measures a devirtualized call site.
func BenchmarkWriteLocal(b *testing.B) {
	data := []byte("benchmark data")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = WriteLocal(data)
	}
}

// BenchmarkWriteParam measures an indirect call site (no PGO in this run).
func BenchmarkWriteParam(b *testing.B) {
	data := []byte("benchmark data")
	var s Sink = NullSink{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = WriteParam(s, data)
	}
}
```

Your turn: add `TestCountSinkAccumulates` that calls `WriteParam` twice with the same `*CountSink` and asserts that `cs.N` equals the sum of both lengths.

### Exercise 4: A Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"os"

	"example.com/devirt"
)

func main() {
	// WriteLocal: devirtualized by the compiler.
	n, err := devirt.WriteLocal([]byte("devirtualized call"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "WriteLocal: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("WriteLocal discarded %d bytes\n", n)

	// WriteParam with a CountSink: indirect call.
	cs := &devirt.CountSink{}
	_, _ = devirt.WriteParam(cs, []byte("indirect call"))
	fmt.Printf("CountSink received %d bytes\n", cs.N)

	// WriteTo with a real buffer.
	var buf bytes.Buffer
	_, err = devirt.WriteTo(&buf, []byte("hello from demo"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "WriteTo: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("buffer contains: %q\n", buf.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Assuming a Parameter Interface Will Devirtualize

Wrong: passing a `*NullSink` into `WriteParam` and expecting the compiler to devirtualize the call because you know only one type flows in at runtime.

What happens: the compiler only sees the interface parameter type. It has no proof the concrete type is `*NullSink`; it emits an `InterCall`.

Fix: use `WriteLocal` (create the value and call through the interface in the same function), or provide a CPU profile so PGO can devirtualize based on observed runtime behavior.

### Storing the Interface in a Struct Field Before Calling

Wrong:

```go
type Processor struct{ w Sink }

func (p *Processor) process(data []byte) { p.w.Write(data) }
```

What happens: the concrete type assigned to `p.w` is not visible at `p.w.Write(data)` unless the compiler inlines the whole call chain, which it usually will not do for struct field accesses across package boundaries.

Fix: if the concrete type is always the same, accept a concrete type or use a generic function. If the type varies, accept the interface overhead or use PGO.

### Missing the `fmt` Import in the Test File

Wrong: writing an `Example` function that calls `fmt.Printf` but forgetting `"fmt"` in the import block. The compiler error is clear, but it is easy to omit when copying individual code blocks.

Fix: each file listed under a `Create` marker is a complete, self-contained Go source file with its own `package` declaration and `import` block.

### Running Only Benchmarks to Verify Correctness

Wrong: checking `go test -bench=.` and treating a non-panicking run as proof of correctness.

What happens: benchmarks do not check return values; a regression that changes `NullSink.Write` to always return `(0, nil)` passes the benchmark silently.

Fix: run `go test -count=1 -race ./...` first. Benchmarks measure performance, not correctness.

## Verification

From `~/go-exercises/devirt`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

To see devirtualization in action:

```bash
go build -gcflags='-m -m' ./... 2>&1 | grep -i devirtualiz
```

Confirm that `WriteLocal` appears in the output (devirtualized to `NullSink`) and `WriteParam` does not.

To compare devirtualized vs. indirect call performance:

```bash
go test -bench=. -benchmem -count=5 ./...
```

## Summary

- An interface call is an indirect call through a function pointer in the itab; it cannot be inlined.
- Static devirtualization fires when the compiler can prove the concrete type at the call site, typically when the value is assigned locally in the same function or an inlined caller.
- Devirtualized calls appear as `StaticCall` (not `InterCall`) in SSA and are eligible for inlining and further optimization.
- PGO devirtualization (Go 1.21+) handles call sites where the static compiler lacks proof: it inserts a guarded direct call for the most common concrete type observed in a CPU profile.
- The `GOSSAFUNC=<FuncName>` environment variable and `-gcflags='-m -m'` are the primary tools for observing devirtualization.
- Function parameters, struct fields, channels, maps, and global variables all hide the concrete type and prevent static devirtualization.

## What's Next

Next: [Dead Code Elimination](../07-dead-code-elimination/07-dead-code-elimination.md).

## Resources

- [Go 1.21 release notes: PGO devirtualization](https://go.dev/doc/go1.21#pgo-devirtualization)
- [Profile-Guided Optimization user guide](https://go.dev/doc/pgo)
- [Interface dispatch internals (Russ Cox)](https://research.swtch.com/interfaces)
- [Go compiler SSA README](https://github.com/golang/go/blob/master/src/cmd/compile/internal/ssa/README.md)
- [Go specification: Interface types](https://go.dev/ref/spec#Interface_types)

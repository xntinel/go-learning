# Exercise 10: Order Type-Switch Cases For A Hot-Path Message Decoder

A wire protocol decoder dispatches on the dynamic type of each decoded frame with
a type switch. Because that switch is a linear scan, the order of its cases is a
performance knob: put the production-dominant frame type last and you pay for
every rarer case first, on every message. This module benchmarks two orderings to
prove the linear scan and quantify the win.

This module is fully self-contained: its own module, code, demo, and tests.

## What you'll build

```text
wire/                       independent module: example.com/wire
  go.mod                    go 1.26
  wire.go                   frame types; ClassifyCommonFirst / ClassifyCommonLast
  cmd/
    demo/
      main.go               classifies one of each frame type
  wire_test.go              both orderings agree; two benchmarks show the ns/op gap
```

- Files: `wire.go`, `cmd/demo/main.go`, `wire_test.go`.
- Implement: frame types (`Data`, `Ack`, `Ping`, `Pong`, `Close`), a `ClassifyCommonFirst` (dominant `Data` case first) and a `ClassifyCommonLast` (dominant `Data` case last), returning identical labels.
- Test: a correctness table asserts both orderings classify every frame identically; two benchmarks over a `Data`-dominated workload show a measurable ns/op difference.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### A type switch is a linear scan; order is a performance knob

`switch v := frame.(type)` compiles to a sequence of type-word comparisons: the
runtime compares the frame's dynamic type against the first case, then the second,
and so on, taking the first match. There is no hash table and no computed jump —
unlike an integer `switch` over dense constants, which the compiler *can* turn into
a jump table, a type switch has no such optimization because the case values are
type descriptors, not integers. So classifying a `Data` frame costs one comparison
if `case Data` is first and five comparisons if it is last. When `Data` is 90% of
the traffic, that difference is paid on nearly every message.

`ClassifyCommonFirst` and `ClassifyCommonLast` contain the exact same cases and
return the exact same labels; they differ only in order. `CommonFirst` puts the
dominant `Data` case first, so the common path matches on comparison one.
`CommonLast` puts `Data` last, so the common path falls through every other case
first. The benchmark runs a `Data`-dominated workload through both and shows
`CommonFirst` is faster — direct evidence of the linear scan. The correctness test
proves reordering changes only speed, never the result: both orderings classify
every frame type identically.

The senior takeaway is to profile the real traffic mix and order the switch by
frequency, dominant type first — the same instinct as ordering `if`/`else if`
branches by likelihood on a hot path. Do not assume the compiler will fix the
order for you; for type switches it will not.

Create `wire.go`:

```go
package wire

// Frame is a decoded wire-protocol message. The concrete types below are the
// frame kinds the decoder dispatches on.
type Frame any

// Data carries an application payload. It dominates real traffic.
type Data struct{ Payload []byte }

// Ack acknowledges a sequence number.
type Ack struct{ Seq uint64 }

// Ping is a keepalive request.
type Ping struct{}

// Pong is a keepalive reply.
type Pong struct{}

// Close signals connection teardown.
type Close struct{ Reason string }

// ClassifyCommonFirst dispatches with the dominant Data case first, so the common
// path matches on the first comparison.
func ClassifyCommonFirst(f Frame) string {
	switch f.(type) {
	case Data:
		return "data"
	case Ack:
		return "ack"
	case Ping:
		return "ping"
	case Pong:
		return "pong"
	case Close:
		return "close"
	default:
		return "unknown"
	}
}

// ClassifyCommonLast dispatches with the dominant Data case last, so the common
// path falls through every other case first. Same labels, worse order.
func ClassifyCommonLast(f Frame) string {
	switch f.(type) {
	case Close:
		return "close"
	case Pong:
		return "pong"
	case Ping:
		return "ping"
	case Ack:
		return "ack"
	case Data:
		return "data"
	default:
		return "unknown"
	}
}
```

### The runnable demo

The demo classifies one of each frame type with both functions, showing they agree.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/wire"
)

func main() {
	frames := []wire.Frame{
		wire.Data{Payload: []byte("hi")},
		wire.Ack{Seq: 7},
		wire.Ping{},
		wire.Pong{},
		wire.Close{Reason: "bye"},
		"junk",
	}
	for _, f := range frames {
		fmt.Printf("%-24T first=%-8s last=%s\n",
			f, wire.ClassifyCommonFirst(f), wire.ClassifyCommonLast(f))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
wire.Data                first=data     last=data
wire.Ack                 first=ack      last=ack
wire.Ping                first=ping     last=ping
wire.Pong                first=pong     last=pong
wire.Close               first=close    last=close
string                   first=unknown  last=unknown
```

### Tests

`TestOrderingsAgree` is the correctness guarantee: over a mixed workload including
an unknown type, both orderings return the same label for every frame — reordering
is a pure performance change. `BenchmarkClassify` runs a `Data`-dominated workload
through both functions; `CommonFirst` should report a lower ns/op because the
dominant frame matches on the first comparison instead of the fifth. Run the
benchmarks with `go test -bench=. -benchmem` to see the gap; the correctness test
runs under `-race` as usual.

Create `wire_test.go`:

```go
package wire

import "testing"

func mixedWorkload() []Frame {
	return []Frame{
		Data{Payload: []byte("a")},
		Ack{Seq: 1},
		Ping{},
		Pong{},
		Close{Reason: "x"},
		"junk",
	}
}

func TestOrderingsAgree(t *testing.T) {
	t.Parallel()

	for _, f := range mixedWorkload() {
		first := ClassifyCommonFirst(f)
		last := ClassifyCommonLast(f)
		if first != last {
			t.Fatalf("orderings disagree for %T: first=%q last=%q", f, first, last)
		}
	}
}

func TestClassifyLabels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		frame Frame
		want  string
	}{
		{Data{}, "data"},
		{Ack{}, "ack"},
		{Ping{}, "ping"},
		{Pong{}, "pong"},
		{Close{}, "close"},
		{42, "unknown"},
	}
	for _, tc := range tests {
		if got := ClassifyCommonFirst(tc.frame); got != tc.want {
			t.Fatalf("ClassifyCommonFirst(%T) = %q, want %q", tc.frame, got, tc.want)
		}
	}
}

// dataDominated is a workload that is ~90% Data frames, matching real traffic.
func dataDominated() []Frame {
	w := make([]Frame, 0, 100)
	for i := range 100 {
		if i%10 == 0 {
			w = append(w, Ack{Seq: uint64(i)})
		} else {
			w = append(w, Data{Payload: []byte("payload")})
		}
	}
	return w
}

func BenchmarkClassify(b *testing.B) {
	work := dataDominated()

	b.Run("common-first", func(b *testing.B) {
		var sink int
		for b.Loop() {
			for _, f := range work {
				sink += len(ClassifyCommonFirst(f))
			}
		}
		_ = sink
	})

	b.Run("common-last", func(b *testing.B) {
		var sink int
		for b.Loop() {
			for _, f := range work {
				sink += len(ClassifyCommonLast(f))
			}
		}
		_ = sink
	})
}
```

## Review

The decoder is correct when both orderings produce identical labels — that is the
invariant `TestOrderingsAgree` locks down, so the benchmark can compare speed
without any risk that a faster ordering is silently wrong. The benchmark makes the
linear scan visible: on a `Data`-dominated workload, `common-first` matches the
frequent frame on comparison one while `common-last` walks past four cases first,
and the ns/op gap is the cost of those extra comparisons multiplied across every
message. Do not expect the compiler to reorder a type switch into a jump table —
that optimization exists for dense integer switches, not for type descriptors — so
ordering by measured frequency is a real, manual optimization. Keep the `default`
last (it matches everything), profile the true traffic mix, and put the dominant
type first.

## Resources

- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches) — the semantics that make case order a linear scan.
- [testing.B.Loop](https://pkg.go.dev/testing#B.Loop) — the Go 1.24 benchmark loop used by both sub-benchmarks.
- [Go blog: Go Data Structures: Interfaces (Russ Cox)](https://research.swtch.com/interfaces) — why each case is a type-pointer comparison, not a hash lookup.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-stringer-formatter-precedence.md](09-stringer-formatter-precedence.md) | Next: [../10-dependency-injection-with-interfaces/00-concepts.md](../10-dependency-injection-with-interfaces/00-concepts.md)

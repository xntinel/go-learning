# Exercise 10: Copy Cost — Pointer Receivers for Large Structs in a Hot Path

Mutation is not the only reason to reach for a pointer receiver. A value receiver
copies the *entire* struct — every field, including any fixed-size array — on
every call, and in a hot path that copy is measurable. This module builds a fat
`AuditRecord`, gives it two `Validate` variants (one by value, one by pointer),
and uses a benchmark to make the copy cost observable, teaching the size-based
rule even when no mutation is involved.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
audit/                     independent module: example.com/audit
  go.mod
  audit.go                 fat AuditRecord; ValidateByValue vs ValidateByPointer
  cmd/
    demo/
      main.go              print the struct size and validate a record
  audit_test.go            correctness for both variants; b.Loop benchmarks with ReportAllocs
```

Files: `audit.go`, `cmd/demo/main.go`, `audit_test.go`.
Implement: a large `AuditRecord` (several string fields, a `time.Time`, and a `[256]byte` payload), with `ValidateByValue() error` (value receiver, copies the struct) and `ValidateByPointer() error` (pointer receiver, no copy); an `ErrInvalidRecord` sentinel.
Test: correctness for both variants (valid record accepted, bad record rejected via `errors.Is`); `BenchmarkValidateByValue` vs `BenchmarkValidateByPointer` using `b.Loop()` and `b.ReportAllocs()`.
Verify: `go test -count=1 -race ./...` and `go test -bench=. -benchtime=1x ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/audit/cmd/demo
cd ~/go-exercises/audit
go mod init example.com/audit
```

### Why a value receiver is not free

A value receiver passes the receiver *by value*: the compiler copies the whole
struct onto the call boundary each time the method runs. For a two-field struct
that is negligible. For `AuditRecord` — several 16-byte string headers, a 24-byte
`time.Time`, and a 256-byte inline array — one call copies on the order of 376
bytes (the exact size is printed by the demo). A validator invoked once per
request, per audit event, in a tight ingest loop, pays that copy on every
invocation for no benefit: validation only *reads* the record.

`ValidateByPointer` takes `*AuditRecord`, so each call passes a single 8-byte
pointer regardless of how fat the struct grows. Same logic, no copy. This is the
third pointer-receiver rule from the concepts — large structs — and it is
independent of mutation: even a purely read-only method on a big struct should
take a pointer receiver in a hot path. The rule of thumb from the Go project is
that once a struct is more than a few machine words, or you are unsure, prefer a
pointer receiver; here the `[256]byte` payload makes the difference unmistakable.

`b.Loop()` (Go 1.24+) is the modern benchmark loop: `for b.Loop() { ... }` runs
the body the right number of times and, unlike the older `for range b.N`, keeps
its arguments and results alive so the compiler cannot optimize the call away —
exactly what you want when measuring a copy. Pairing it with `b.ReportAllocs()`
surfaces bytes-and-allocs per operation so the value variant's extra copying shows
up in the numbers.

Create `audit.go`:

```go
package audit

import (
	"errors"
	"time"
)

// ErrInvalidRecord marks a structurally invalid audit record.
var ErrInvalidRecord = errors.New("audit: invalid record")

// AuditRecord is deliberately fat: string headers, a time.Time, and an inline
// 256-byte payload. Copying it by value on every method call is wasteful.
type AuditRecord struct {
	ID        string
	Actor     string
	Action    string
	Resource  string
	Timestamp time.Time
	IP        string
	UserAgent string
	Payload   [256]byte
}

// valid reports whether the record is well-formed. Shared by both variants so
// the only difference under test is the receiver kind.
func valid(r *AuditRecord) bool {
	return r.Actor != "" && r.Action != "" && !r.Timestamp.IsZero()
}

// ValidateByValue uses a VALUE receiver: every call copies the whole struct,
// including the 256-byte payload, even though validation only reads it.
func (r AuditRecord) ValidateByValue() error {
	if !valid(&r) {
		return ErrInvalidRecord
	}
	return nil
}

// ValidateByPointer uses a POINTER receiver: every call passes one 8-byte
// pointer, no matter how large the struct grows. Same logic, no copy.
func (r *AuditRecord) ValidateByPointer() error {
	if !valid(r) {
		return ErrInvalidRecord
	}
	return nil
}
```

### The runnable demo

The demo prints the struct's size (so the copy cost is concrete) and validates one
good and one bad record.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"
	"unsafe"

	"example.com/audit"
)

func main() {
	fmt.Printf("AuditRecord size: %d bytes (copied per value-receiver call)\n",
		unsafe.Sizeof(audit.AuditRecord{}))

	good := audit.AuditRecord{Actor: "alice", Action: "login", Timestamp: time.Now()}
	if err := good.ValidateByPointer(); err != nil {
		fmt.Println("unexpected:", err)
	} else {
		fmt.Println("good record: valid")
	}

	var bad audit.AuditRecord // zero: no actor, no action, zero time
	if err := bad.ValidateByValue(); errors.Is(err, audit.ErrInvalidRecord) {
		fmt.Println("bad record: rejected")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
AuditRecord size: 376 bytes (copied per value-receiver call)
good record: valid
bad record: rejected
```

(The size is for a 64-bit build; it may differ on 32-bit targets.)

### Tests

The correctness tests run both variants against a valid and an invalid record so
the receiver change cannot alter behavior. The two benchmarks measure the copy:
`BenchmarkValidateByValue` copies 376 bytes per call, `BenchmarkValidateByPointer`
copies 8. Run them with `go test -bench=. -benchtime=1x` — the default
`go test -race` compiles them but does not execute the loops, so they must at
least build cleanly, which the gate checks.

Create `audit_test.go`:

```go
package audit

import (
	"errors"
	"testing"
	"time"
)

func validRecord() AuditRecord {
	return AuditRecord{
		ID:        "evt-1",
		Actor:     "alice",
		Action:    "login",
		Timestamp: time.Unix(1000, 0).UTC(),
	}
}

func TestValidateAcceptsValidRecord(t *testing.T) {
	t.Parallel()

	r := validRecord()
	if err := r.ValidateByValue(); err != nil {
		t.Fatalf("ValidateByValue on valid record: %v", err)
	}
	if err := r.ValidateByPointer(); err != nil {
		t.Fatalf("ValidateByPointer on valid record: %v", err)
	}
}

func TestValidateRejectsBadRecord(t *testing.T) {
	t.Parallel()

	var bad AuditRecord // missing actor/action, zero timestamp
	if err := bad.ValidateByValue(); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("ValidateByValue err = %v, want ErrInvalidRecord", err)
	}
	if err := bad.ValidateByPointer(); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("ValidateByPointer err = %v, want ErrInvalidRecord", err)
	}
}

func BenchmarkValidateByValue(b *testing.B) {
	r := validRecord()
	b.ReportAllocs()
	for b.Loop() {
		_ = r.ValidateByValue() // copies the whole struct each call
	}
}

func BenchmarkValidateByPointer(b *testing.B) {
	r := validRecord()
	b.ReportAllocs()
	for b.Loop() {
		_ = r.ValidateByPointer() // passes one pointer each call
	}
}
```

On an Apple M-series machine a representative run shows the value variant a few
nanoseconds slower per op with more bytes moved; the exact delta depends on
hardware, but the direction is stable: copying 376 bytes is never cheaper than
copying 8.

## Review

Both variants are correct when they accept a well-formed record and reject a zero
one — the receiver kind must not change behavior, only cost. The benchmarks make
the cost visible: `ValidateByValue` copies the entire `AuditRecord` (the size the
demo prints) on every call, while `ValidateByPointer` copies a single pointer. The
takeaway is that "value receiver on a big struct is free" is false — it copies
every field, including the inline array, each call. For a large struct in a hot
path, choose a pointer receiver even when the method only reads, and let a
`b.Loop()` benchmark with `b.ReportAllocs()` confirm the difference rather than
guessing.

## Resources

- [`testing.B.Loop`](https://pkg.go.dev/testing#B.Loop) — the Go 1.24 benchmark loop that keeps calls from being optimized away.
- [`testing.B.ReportAllocs`](https://pkg.go.dev/testing#B.ReportAllocs) — per-op allocation reporting in benchmarks.
- [Go Code Review Comments: Receiver Type](https://go.dev/wiki/CodeReviewComments#receiver-type) — large structs are a reason to use a pointer receiver.
- [`unsafe.Sizeof`](https://pkg.go.dev/unsafe#Sizeof) — the compile-time size of a struct value.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-map-element-addressability.md](09-map-element-addressability.md) | Next: [../04-anonymous-structs-and-embedding/00-concepts.md](../04-anonymous-structs-and-embedding/00-concepts.md)

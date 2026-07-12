# Exercise 10: Splat Aliasing — Merging Default and Per-Request Headers Safely

Splatting a slice into a variadic shares its backing array, so a callee that
appends can silently overwrite the caller's data. You build `MergeHeaders(defaults
[]string, extra ...string) []string` that merges a service's default headers with
per-request extras *without* corrupting the caller's `defaults` — and you write a
deliberately buggy twin to make the corruption concrete, so the copy-first fix is
never abstract again.

This module is self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
headers/                   independent module: example.com/headers
  go.mod                   go 1.25
  headers.go               MergeHeaders (safe) and MergeHeadersBuggy (demonstrates the bug)
  cmd/
    demo/
      main.go              runnable demo: safe merge vs. backing-array corruption
  headers_test.go          defaults-unchanged, no-shared-backing, buggy-corrupts proof
```

- Files: `headers.go`, `cmd/demo/main.go`, `headers_test.go`.
- Implement: `MergeHeaders(defaults []string, extra ...string) []string` that copies first (`slices.Clone`) so it never mutates the caller's array; plus a buggy `MergeHeadersBuggy` that appends onto `defaults` directly.
- Test: after `MergeHeaders`, `defaults` is byte-for-byte unchanged and the result shares no backing array with `defaults`; the buggy variant corrupts a spare-capacity `defaults`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### The append-onto-splatted-slice bug, and the copy-first fix

Consider the tempting one-liner: `return append(defaults, extra...)`. When
`defaults` has spare capacity — which happens whenever it is a sub-slice of a
larger array, e.g. `full[:2]` of a four-element `full` — `append` writes the new
elements *into that spare capacity*, which is `full`'s backing array. The caller
never appended anything, yet `full[2]` silently changes. In a header-merge helper
this means one request's `X-Request-Id` overwrites another's header, a
heisenbug that only appears when the defaults slice happens to have room.

The fix is to copy before appending, so the callee owns its array:

```go
func MergeHeaders(defaults []string, extra ...string) []string {
	out := slices.Clone(defaults) // fresh backing array, len == cap
	return append(out, extra...)  // reallocates; defaults untouched
}
```

`slices.Clone` returns a slice whose length equals its capacity, so the subsequent
`append` cannot fit in `out`'s array and must reallocate a new one — guaranteeing
the result shares no backing array with `defaults`, and `defaults` itself is never
touched. (`make([]string, len(defaults)); copy(...)` is the pre-1.21 spelling of
the same idea.) The two properties the tests assert are exactly these: `defaults`
is byte-for-byte unchanged, and mutating the result does not bleed back into
`defaults`.

To make the danger undeniable, the buggy twin `MergeHeadersBuggy` does the naive
`append(defaults, extra...)`, and its test hands it a spare-capacity slice and
proves the caller's backing array is corrupted. Seeing the corruption in a passing
assertion is what turns "always copy before appending a splatted slice" from a rule
you memorize into one you understand.

Create `headers.go`:

```go
// headers.go
package headers

import "slices"

// MergeHeaders returns defaults followed by extra, without mutating the caller's
// defaults slice or its backing array. It copies first, so the result is always
// an independent slice.
func MergeHeaders(defaults []string, extra ...string) []string {
	out := slices.Clone(defaults)
	return append(out, extra...)
}

// MergeHeadersBuggy is the WRONG version, kept to demonstrate the aliasing bug: it
// appends onto defaults directly, so when defaults has spare capacity the append
// overwrites the caller's backing array. Do not use this in real code.
func MergeHeadersBuggy(defaults []string, extra ...string) []string {
	return append(defaults, extra...)
}
```

### The runnable demo

The demo runs both versions against a `defaults` that is a spare-capacity
sub-slice, so you watch the buggy one corrupt the caller's array while the safe one
leaves it intact.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/headers"
)

func main() {
	// full has 4 elements; defaults views the first 2 but keeps cap 4.
	full := []string{"Accept", "Authorization", "X-Trace", "X-Span"}
	defaults := full[:2]

	merged := headers.MergeHeaders(defaults, "X-Request-Id")
	fmt.Println("safe merged:  ", merged)
	fmt.Println("full intact:  ", full)

	// The buggy version appends into full's backing array, clobbering full[2].
	full2 := []string{"Accept", "Authorization", "X-Trace", "X-Span"}
	defaults2 := full2[:2]

	buggy := headers.MergeHeadersBuggy(defaults2, "X-Request-Id")
	fmt.Println("buggy merged: ", buggy)
	fmt.Println("full clobbered:", full2)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
safe merged:   [Accept Authorization X-Request-Id]
full intact:   [Accept Authorization X-Trace X-Span]
buggy merged:  [Accept Authorization X-Request-Id]
full clobbered: [Accept Authorization X-Request-Id X-Span]
```

The last line is the bug: `full2[2]` changed from `X-Trace` to `X-Request-Id`
because `MergeHeadersBuggy` appended into the shared backing array.

### Tests

`TestMergeHeadersDoesNotMutate` asserts the caller's `defaults` is unchanged.
`TestMergeHeadersNoSharedBacking` mutates the result and confirms `defaults` does
not move with it. `TestBuggyVariantCorrupts` documents the failure the safe version
prevents, proving the spare-capacity corruption happens.

Create `headers_test.go`:

```go
// headers_test.go
package headers

import (
	"fmt"
	"slices"
	"testing"
)

func TestMergeHeadersDoesNotMutate(t *testing.T) {
	t.Parallel()

	full := []string{"Accept", "Authorization", "X-Trace", "X-Span"}
	defaults := full[:2]
	before := slices.Clone(defaults)

	_ = MergeHeaders(defaults, "X-Request-Id")

	if !slices.Equal(defaults, before) {
		t.Fatalf("defaults mutated: %v, want %v", defaults, before)
	}
	if full[2] != "X-Trace" {
		t.Fatalf("caller backing array corrupted: full[2] = %q, want X-Trace", full[2])
	}
}

func TestMergeHeadersNoSharedBacking(t *testing.T) {
	t.Parallel()

	defaults := []string{"Accept", "Authorization"}
	result := MergeHeaders(defaults, "X-Request-Id")

	result[0] = "MUTATED"
	if defaults[0] != "Accept" {
		t.Fatalf("result shares backing array with defaults: defaults[0] = %q", defaults[0])
	}
}

func TestMergeHeadersContent(t *testing.T) {
	t.Parallel()

	got := MergeHeaders([]string{"Accept"}, "X-A", "X-B")
	want := []string{"Accept", "X-A", "X-B"}
	if !slices.Equal(got, want) {
		t.Fatalf("MergeHeaders = %v, want %v", got, want)
	}
}

func TestBuggyVariantCorrupts(t *testing.T) {
	t.Parallel()

	full := []string{"Accept", "Authorization", "X-Trace", "X-Span"}
	defaults := full[:2]

	_ = MergeHeadersBuggy(defaults, "X-Request-Id")

	// The buggy append wrote into full's spare capacity, corrupting full[2].
	if full[2] != "X-Request-Id" {
		t.Fatalf("expected buggy corruption of full[2], got %q", full[2])
	}
}

func ExampleMergeHeaders() {
	got := MergeHeaders([]string{"Accept"}, "X-A", "X-B")
	fmt.Println(got)
	// Output: [Accept X-A X-B]
}
```

## Review

`MergeHeaders` is correct when the caller's `defaults` is byte-for-byte unchanged
after the call and the returned slice shares no backing array with it — both
asserted directly. The mechanism is copy-first: `slices.Clone` produces a
len-equals-cap slice so the follow-on `append` is forced to reallocate, isolating
the result. The buggy twin is not dead code; it is the proof that the spare-
capacity corruption is real, which is why its test asserts the corruption happens.
The rule to carry everywhere: when a function receives a slice (or a splatted
variadic) it intends to append to or mutate, it must copy first unless it explicitly
owns that memory. Run `go test -race`, which is especially apt here since aliasing
bugs and data races share the same root cause.

## Resources

- [`slices.Clone`](https://pkg.go.dev/slices#Clone)
- [`append` and slice growth (Go Slices: usage and internals)](https://go.dev/blog/slices-intro)
- [Go Spec: Appending to and copying slices](https://go.dev/ref/spec#Appending_and_copying_slices)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-hot-path-slice-vs-variadic.md](09-hot-path-slice-vs-variadic.md) | Next: [11-kv-audit-line-builder.md](11-kv-audit-line-builder.md)

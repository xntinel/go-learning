# Exercise 1: Offset/Limit Log Window That Returns an Isolated Copy

A log-viewer endpoint pages through captured log lines: given an `(offset, limit)`
request it returns that window. The window is served to a handler that may sort,
redact, or append to it, so the producer must hand back an *owned copy*, never a
view into its own backing array. This exercise builds that artifact and pins its
copy contract with a mutation test.

This module is fully self-contained: its own `go mod init`, its own types, demo,
and tests. Nothing here imports another exercise.

## What you'll build

```text
logwin/                    independent module: example.com/logwin
  go.mod                   go 1.24
  logwin.go                Window(lines, offset, limit) ([]string, error); ErrInvalidOffset, ErrInvalidLimit
  cmd/
    demo/
      main.go              runnable demo: page a small log
  logwin_test.go           table tests, mutation-isolation test, empty-input test
```

- Files: `logwin.go`, `cmd/demo/main.go`, `logwin_test.go`.
- Implement: `Window` returning the clamped `(offset, limit)` window as an
  independent copy; reject negative/past-end offset and non-positive limit with
  wrapped sentinel errors; return a non-nil empty slice at end of input.
- Test: normal window, clamp-to-end, empty-at-end, invalid offset/limit, mutation
  isolation, and empty input (`nil` and `[]string{}`).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/logwin/cmd/demo
cd ~/go-exercises/logwin
go mod init example.com/logwin
go mod edit -go=1.24
```

## Why a copy is the correct default

`Window` owns `lines`. If it returned `lines[low:high]`, the caller would receive a
view welded to the producer's backing array: sorting the page would reorder the
source, redacting an entry would redact the source, and — subtlest — an `append`
into the page's inherited spare capacity would overwrite source lines the producer
still holds. For a producer that hands data across an API boundary, the safe
default is an owned copy. `slices.Clone` copies exactly the `high - low` elements of
the requested range into a fresh, right-sized array (`cap == len`), so the caller
can mutate and append freely with no reach-back into the source.

Two boundary decisions matter. First, *clamp before slicing*: `offset + limit` may
run past `len(lines)`, so `high` is capped at `len(lines)`; without the clamp the
slice expression would panic on a large limit. The offset itself is validated up
front — negative or strictly past `len(lines)` is `ErrInvalidOffset` (note
`offset == len(lines)` is allowed and yields an empty window, which is what a client
paging exactly to the end expects). Second, *return non-nil empty, not nil*: at end
of input the window is empty, and returning `[]string{}` keeps the caller's `range`
and JSON marshaling uniform (`[]`, not `null`).

Both sentinels are wrapped by nothing here — they are returned directly — but they
are package-level `error` values so callers assert them with `errors.Is`, which is
the contract the test pins.

Create `logwin.go`:

```go
package logwin

import (
	"errors"
	"slices"
)

// ErrInvalidOffset is returned when offset is negative or past the end of the
// input. offset == len(lines) is valid and yields an empty window.
var ErrInvalidOffset = errors.New("logwin: invalid offset")

// ErrInvalidLimit is returned when limit is not positive.
var ErrInvalidLimit = errors.New("logwin: invalid limit")

// Window returns the [offset, offset+limit) window of lines, clamped to the end
// of the input, as an independent copy. The caller may mutate or append to the
// result without affecting lines.
func Window(lines []string, offset, limit int) ([]string, error) {
	if offset < 0 || offset > len(lines) {
		return nil, ErrInvalidOffset
	}
	if limit <= 0 {
		return nil, ErrInvalidLimit
	}
	low := offset
	high := offset + limit
	if high > len(lines) {
		high = len(lines)
	}
	if low == high {
		return []string{}, nil
	}
	return slices.Clone(lines[low:high]), nil
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/logwin"
)

func main() {
	lines := []string{
		"08:00 boot",
		"08:01 ready",
		"08:02 request /health",
		"08:03 request /orders",
		"08:04 shutdown",
	}

	page, err := logwin.Window(lines, 1, 3)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("page (offset 1, limit 3): %d lines\n", len(page))
	for _, l := range page {
		fmt.Println(" ", l)
	}

	// Mutating the returned page does not touch the source.
	page[0] = "REDACTED"
	fmt.Println("source line 1 still:", lines[1])

	tail, _ := logwin.Window(lines, 5, 10)
	fmt.Printf("page at end: %d lines (non-nil: %v)\n", len(tail), tail != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
page (offset 1, limit 3): 3 lines
  08:01 ready
  08:02 request /health
  08:03 request /orders
source line 1 still: 08:01 ready
page at end: 0 lines (non-nil: true)
```

## Tests

The mutation test is the heart of the lesson: it writes into the returned slice and
asserts the source is unchanged, pinning the copy contract at the expression level.
The empty-input test pins that `nil` and `[]string{}` inputs both yield a non-nil
empty result with no error.

Create `logwin_test.go`:

```go
package logwin

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

func TestWindow(t *testing.T) {
	t.Parallel()
	lines := []string{"a", "b", "c", "d", "e"}
	tests := []struct {
		name   string
		offset int
		limit  int
		want   []string
	}{
		{"normal window", 1, 2, []string{"b", "c"}},
		{"clamp to end", 1, 100, []string{"b", "c", "d", "e"}},
		{"empty at end", 5, 10, []string{}},
		{"single element", 0, 1, []string{"a"}},
		{"whole slice", 0, 5, []string{"a", "b", "c", "d", "e"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Window(lines, tc.offset, tc.limit)
			if err != nil {
				t.Fatalf("Window(%d,%d) unexpected error: %v", tc.offset, tc.limit, err)
			}
			if got == nil {
				t.Fatal("Window returned nil; want non-nil slice")
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Window(%d,%d) = %v, want %v", tc.offset, tc.limit, got, tc.want)
			}
		})
	}
}

func TestWindowRejectsInvalidInputs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		offset  int
		limit   int
		wantErr error
	}{
		{"negative offset", -1, 1, ErrInvalidOffset},
		{"offset past end", 100, 1, ErrInvalidOffset},
		{"zero limit", 0, 0, ErrInvalidLimit},
		{"negative limit", 0, -1, ErrInvalidLimit},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Window([]string{"a", "b", "c"}, tc.offset, tc.limit)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestWindowReturnsIndependentCopy(t *testing.T) {
	t.Parallel()
	lines := []string{"a", "b", "c"}
	got, err := Window(lines, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	got[0] = "modified"
	got = append(got, "extra") // must not reach into lines
	if lines[0] != "a" {
		t.Fatalf("mutation leaked into source: lines[0] = %q", lines[0])
	}
	if len(lines) != 3 {
		t.Fatalf("append leaked into source: len(lines) = %d", len(lines))
	}
}

func TestWindowHandlesEmptyInput(t *testing.T) {
	t.Parallel()
	for _, in := range [][]string{nil, {}} {
		got, err := Window(in, 0, 10)
		if err != nil {
			t.Fatalf("Window(%v) error: %v", in, err)
		}
		if got == nil {
			t.Fatalf("Window(%v) = nil; want non-nil empty slice", in)
		}
		if len(got) != 0 {
			t.Fatalf("Window(%v) len = %d; want 0", in, len(got))
		}
	}
}

func ExampleWindow() {
	lines := []string{"a", "b", "c", "d"}
	page, _ := Window(lines, 1, 2)
	fmt.Println(page)
	// Output: [b c]
}
```

## Review

The window is correct when its length is exactly `min(offset+limit, len) - offset`,
its contents are the source lines in order, and mutating or appending to it cannot
be observed in the source. The independence test proves the last point at the
expression level: it both overwrites `got[0]` and appends past `got`'s length, and
neither reaches `lines`, which holds only because `slices.Clone` returns a
right-sized array with no shared capacity. The classic wrong turns are returning
`lines[low:high]` (a view the caller mutates), pre-sizing with `make`+`copy` (which
silently zero-fills the tail if the source range is shorter than expected), and
returning `nil` for the empty case (which forces the caller into a `nil` check and
marshals as `null`). Run `go test -race` to confirm nothing shares state across the
parallel subtests.

## Resources

- [Go Specification: Slice expressions](https://go.dev/ref/spec#Slice_expressions)
- [`slices.Clone`](https://pkg.go.dev/slices#Clone)
- [`errors.Is`](https://pkg.go.dev/errors#Is)
- [Go blog: Go Slices: usage and internals](https://go.dev/blog/slices-intro)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-three-index-batch-handoff.md](02-three-index-batch-handoff.md)

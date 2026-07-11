# Exercise 6: Zero-Copy Field Slicing Over a Scanner Buffer That Must Clone Before Keeping

A line-oriented ingest splits each `[]byte` line from a reused `bufio.Scanner`
buffer into field sub-slices using byte-index slice expressions — zero allocation on
the hot path. Then it hits the classic bug: keeping those field sub-slices past the
next `Scan()` corrupts them, because the scanner reuses its buffer underneath you.
This exercise builds the zero-copy split, reproduces the corruption, and fixes the
retained fields with `bytes.Clone`.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
csvscan/                   independent module: example.com/csvscan
  go.mod                   go 1.24
  csvscan.go               SplitFields(dst, line); CollectFirstFields(r, clone)
  cmd/
    demo/
      main.go              runnable demo: retained vs cloned first fields
  csvscan_test.go          corruption test, clone-stable test, zero-alloc test
```

- Files: `csvscan.go`, `cmd/demo/main.go`, `csvscan_test.go`.
- Implement: `SplitFields` splitting a line into comma-separated sub-slice views
  into a reused `dst`; `CollectFirstFields` collecting each line's first field,
  either as raw views (the bug) or `bytes.Clone`d (the fix).
- Test: retained views are corrupted after later scans; cloned fields are stable;
  the hot-path split allocates zero via `testing.AllocsPerRun`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/csvscan/cmd/demo
cd ~/go-exercises/csvscan
go mod init example.com/csvscan
go mod edit -go=1.24
```

## Why the fields are ephemeral, and when to clone

`bufio.Scanner.Bytes()` returns the current token as a sub-slice of the scanner's
internal buffer. Its documentation is explicit: the underlying array "may be
overwritten by a subsequent call to Scan." The scanner reuses one buffer, and when
it needs room for the next line it slides the remaining bytes to the front of that
buffer, overwriting the region where earlier tokens lived. So a field sub-slice you
carved out of line 1 is valid only until the next `Scan()` — after that it may read
line 4's bytes.

This is exactly the behavior we want on the hot path and exactly the trap when we
retain. `SplitFields` carves a line into its comma-separated fields with byte-index
slice expressions (`line[start:i]`), copying nothing: each field is a view into the
scanner's buffer. Processing a field *now*, before the next `Scan`, is free and
correct. But the moment you keep a field — append it to a slice you return, stash it
in a map, send it on a channel — you are retaining a view into a buffer that is
about to be reused, and it will be corrupted.

The fix is surgical: `bytes.Clone` only the fields that outlive the scan. Clone
copies the field's bytes into a fresh, right-sized array the caller owns; the
scanner can churn its buffer freely. `CollectFirstFields(r, clone)` shows both
paths — with `clone == false` it retains raw views and returns garbage; with
`clone == true` it clones and returns stable data. The rule generalizes to every
ephemeral buffer: `sync.Pool` byte slices, reused scratch buffers, memory-mapped
regions — split and process in place for speed, `Clone` whatever you keep.

To make the corruption deterministic (a scanner with a large buffer might not slide
within a short input), the ingest constrains the scanner buffer to 16 bytes with
`Scanner.Buffer`, forcing a slide on every line so the reuse hazard is visible and
reproducible.

Create `csvscan.go`:

```go
package csvscan

import (
	"bufio"
	"bytes"
	"io"
)

// SplitFields splits line on commas into sub-slice VIEWS, reusing dst as the
// destination (pass dst[:0] or a nil slice). It copies no bytes: each returned
// field aliases line, so it is valid only as long as line's bytes are stable.
func SplitFields(dst [][]byte, line []byte) [][]byte {
	dst = dst[:0]
	start := 0
	for {
		i := bytes.IndexByte(line[start:], ',')
		if i < 0 {
			return append(dst, line[start:])
		}
		dst = append(dst, line[start:start+i])
		start += i + 1
	}
}

// CollectFirstFields returns the first comma-field of every line read from r.
// With clone == false it retains raw Scanner.Bytes views (the bug): later scans
// overwrite the shared buffer and corrupt earlier fields. With clone == true it
// bytes.Clone's each field, so the retained values are stable.
func CollectFirstFields(r io.Reader, clone bool) ([][]byte, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 16), 16) // tiny buffer: forces a buffer slide per line

	var out [][]byte
	for sc.Scan() {
		line := sc.Bytes()
		field := line
		if i := bytes.IndexByte(line, ','); i >= 0 {
			field = line[:i]
		}
		if clone {
			field = bytes.Clone(field)
		}
		out = append(out, field)
	}
	return out, sc.Err()
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/csvscan"
)

func main() {
	const data = "aaaa,1111\nbbbb,2222\ncccc,3333\ndddd,4444\neeee,5555\n"

	// Hot-path split of a single line: zero copies, views into the line.
	line := []byte("id,name,region")
	fields := csvscan.SplitFields(nil, line)
	fmt.Printf("split %d fields: ", len(fields))
	for i, f := range fields {
		if i > 0 {
			fmt.Print(" | ")
		}
		fmt.Print(string(f))
	}
	fmt.Println()

	// Retained views are corrupted by later scans.
	views, _ := csvscan.CollectFirstFields(strings.NewReader(data), false)
	fmt.Print("retained views:")
	for _, f := range views {
		fmt.Printf(" %s", f)
	}
	fmt.Println()

	// Cloned fields survive.
	owned, _ := csvscan.CollectFirstFields(strings.NewReader(data), true)
	fmt.Print("cloned fields: ")
	for _, f := range owned {
		fmt.Printf(" %s", f)
	}
	fmt.Println()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
split 3 fields: id | name | region
retained views: eeee eeee eeee eeee eeee
cloned fields:  aaaa bbbb cccc dddd eeee
```

## Tests

The first test documents the bug: retained views are not what they were parsed as.
The second proves `bytes.Clone` makes the retained fields stable. The third pins the
hot-path guarantee — splitting into a reused `dst` allocates zero.

Create `csvscan_test.go`:

```go
package csvscan

import (
	"bytes"
	"strings"
	"testing"
)

const sample = "aaaa,1111\nbbbb,2222\ncccc,3333\ndddd,4444\neeee,5555\n"

// TestRetainedViewsCorrupted documents the bug: keeping Scanner.Bytes views past
// later scans corrupts them because the scanner reuses its buffer.
func TestRetainedViewsCorrupted(t *testing.T) {
	t.Parallel()
	views, err := CollectFirstFields(strings.NewReader(sample), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 5 {
		t.Fatalf("got %d fields, want 5", len(views))
	}
	if bytes.Equal(views[0], []byte("aaaa")) {
		t.Fatal("expected retained view to be corrupted by later scans, but it survived")
	}
}

// TestClonedFieldsStable proves the fix.
func TestClonedFieldsStable(t *testing.T) {
	t.Parallel()
	owned, err := CollectFirstFields(strings.NewReader(sample), true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"aaaa", "bbbb", "cccc", "dddd", "eeee"}
	if len(owned) != len(want) {
		t.Fatalf("got %d fields, want %d", len(owned), len(want))
	}
	for i, w := range want {
		if string(owned[i]) != w {
			t.Fatalf("field %d = %q, want %q", i, owned[i], w)
		}
	}
}

func TestSplitFieldsViewsAndValues(t *testing.T) {
	t.Parallel()
	line := []byte("id,name,region")
	fields := SplitFields(nil, line)
	want := []string{"id", "name", "region"}
	if len(fields) != len(want) {
		t.Fatalf("got %d fields, want %d", len(fields), len(want))
	}
	for i, w := range want {
		if string(fields[i]) != w {
			t.Fatalf("field %d = %q, want %q", i, fields[i], w)
		}
	}
	// A field is a VIEW into line: mutating line changes the field.
	line[0] = 'X'
	if fields[0][0] != 'X' {
		t.Fatal("SplitFields did not return a view into line")
	}
}

func TestSplitFieldsZeroAlloc(t *testing.T) {
	line := []byte("aaaa,1111,2222,3333,4444")
	dst := make([][]byte, 0, 8)
	n := testing.AllocsPerRun(1000, func() {
		dst = SplitFields(dst, line)
	})
	if n != 0 {
		t.Fatalf("hot-path split allocated %v times per run; want 0", n)
	}
}
```

## Review

The ingest is correct when the hot-path split copies nothing (every field aliases
the line, verified by mutating the line and seeing the field change, and by the
zero-alloc assertion) and when retained fields are cloned so they survive buffer
reuse. The corruption test is the teaching moment: it asserts that retaining raw
`Scanner.Bytes` views is broken, so the fix is not optional. The wrong instinct is
to `Clone` everything "to be safe," which throws away the zero-copy win; the right
discipline is to process in place and clone only at the boundary where a field
escapes the scan loop. Note that converting a field to `string` also copies, so a
`map[string]int` keyed on `string(field)` is already safe — it is retaining the
`[]byte` view that bites. Run `go test -race`.

## Resources

- [`bufio.Scanner.Bytes`](https://pkg.go.dev/bufio#Scanner.Bytes)
- [`bytes.Clone`](https://pkg.go.dev/bytes#Clone)
- [`bytes.IndexByte`](https://pkg.go.dev/bytes#IndexByte)
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-sliding-window-rate-limiter.md](05-sliding-window-rate-limiter.md) | Next: [07-subslice-memory-leak-truncation.md](07-subslice-memory-leak-truncation.md)

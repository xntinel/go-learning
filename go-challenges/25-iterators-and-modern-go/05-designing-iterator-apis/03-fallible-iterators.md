# Exercise 3: Fallible Iterators (Seq2[V, error])

A sequence that can fail mid-iteration cannot return an error — it already returned the iterator. The convention is to stream `(value, error)` pairs as `iter.Seq2[V, error]`: each step yields `(value, nil)` on success and `(zero, err)` exactly once on failure, then stops. This exercise builds a single-use `Lines` iterator over a file plus an eager `Collect` sink, so both the loop form and the drain-to-slice form of consuming a fallible sequence are covered.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
stream.go            Lines (iter.Seq2[string, error]), Collect, ErrNilFS sentinel
cmd/
  demo/
    main.go          read lines from an in-memory FS, then surface a missing-file error
stream_test.go       reads lines, nil-FS and missing-file errors, Collect stops at first error
```

- Files: `stream.go`, `cmd/demo/main.go`, `stream_test.go`.
- Implement: `Lines(fsys fs.FS, path string) iter.Seq2[string, error]` and `Collect[V any](seq iter.Seq2[V, error]) ([]V, error)`, plus the `ErrNilFS` sentinel.
- Test: `stream_test.go` reads lines from an `fstest.MapFS`, asserts a nil filesystem yields `ErrNilFS` and a missing file yields `fs.ErrNotExist` (both via `errors.Is`), and that `Collect` returns the partial slice and the error on failure.
- Verify: `go test -run 'TestLines|TestCollect' -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/25-iterators-and-modern-go/05-designing-iterator-apis/03-fallible-iterators/cmd/demo && cd go-solutions/25-iterators-and-modern-go/05-designing-iterator-apis/03-fallible-iterators
```

### Why `Seq2[V, error]`, and the three rules that make it safe

`Lines` opens a file and yields its text one line at a time, but opening can fail and scanning can fail partway through. It cannot report that with a return value, because the value it returns is the iterator itself, produced before a single byte is read. Logging the error and quietly ending the sequence is worse: the caller's `for ... range` loop ends normally and treats a truncated file as a complete one — silent data loss. The convention is to make the error part of the sequence: `iter.Seq2[string, error]`, where the second component is the per-step error. The caller writes `for line, err := range Lines(fsys, path) { if err != nil { ...; break }; ... }`, which puts error handling exactly where the data is consumed.

Three rules make this pattern trustworthy, and the implementation follows all three. First, an error is terminal: after `yield("", err)` the iterator `return`s, so a non-nil error is always the last thing the caller sees on that pass and no value follows it. Second, errors are wrapped with `%w` against a sentinel — `fmt.Errorf("open %s: %w", path, err)` — so the caller can classify them with `errors.Is(err, fs.ErrNotExist)` or `errors.Is(err, ErrNilFS)` instead of matching error strings. Third, the iterator is honest about its lifetime: it is single-use per range because each pass opens the file, scans it once with a `bufio.Scanner`, and closes it via `defer`; a second range re-opens the file. The doc comment says so, because a caller who assumed reuse would otherwise get a surprising second read or a surprising error.

`Collect` is the eager counterpart. A caller who wants the whole file as a `[]string` should not hand-roll the loop every time, so `Collect` drains any `iter.Seq2[V, error]` and stops at the first error, returning the partial slice gathered so far plus that error — exactly the behavior of a careful manual loop. Returning the partial slice (rather than `nil`) lets a caller inspect what was read before the failure if it wants to; the non-nil error tells it the slice is incomplete. The check `if err != nil { return out, err }` must come before appending, so the zero value that rides along with an error never lands in the result.

Create `stream.go`:

```go
package stream

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"iter"
)

// ErrNilFS is returned (wrapped) when Lines is called with a nil filesystem.
var ErrNilFS = errors.New("filesystem must not be nil")

// Lines returns an iterator over the lines of path within fsys. It yields
// (line, nil) for each line and yields ("", err) exactly once if the file
// cannot be opened or a read fails, after which iteration stops.
//
// The sequence is single-use: each range opens the file again, scans it once,
// and closes it. Errors are wrapped with %w so callers can classify them with
// errors.Is (for example against fs.ErrNotExist or ErrNilFS).
func Lines(fsys fs.FS, path string) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		if fsys == nil {
			yield("", fmt.Errorf("lines %s: %w", path, ErrNilFS))
			return
		}

		f, err := fsys.Open(path)
		if err != nil {
			yield("", fmt.Errorf("open %s: %w", path, err))
			return
		}
		defer f.Close()

		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if !yield(sc.Text(), nil) {
				return
			}
		}
		if err := sc.Err(); err != nil {
			yield("", fmt.Errorf("scan %s: %w", path, err))
		}
	}
}

// Collect drains a fallible sequence into a slice, stopping at the first error.
// It returns the values gathered before the failure and that error; on success
// it returns all values and a nil error. It is the eager counterpart to ranging
// over seq by hand.
func Collect[V any](seq iter.Seq2[V, error]) ([]V, error) {
	var out []V
	for v, err := range seq {
		if err != nil {
			return out, err
		}
		out = append(out, v)
	}
	return out, nil
}
```

### The runnable demo

The demo uses `fstest.MapFS`, the in-memory filesystem from the standard library, so it needs no real files. It reads a present file line by line, then ranges a missing path to show the error arriving as the second loop variable and being classified with `errors.Is`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"testing/fstest"

	"example.com/stream"
)

func main() {
	fsys := fstest.MapFS{
		"names.txt": {Data: []byte("alice\nbob\ncharlie\n")},
	}

	fmt.Println("Lines of names.txt:")
	for line, err := range stream.Lines(fsys, "names.txt") {
		if err != nil {
			fmt.Println("  error:", err)
			break
		}
		fmt.Println("  ", line)
	}

	fmt.Println("Reading a missing file:")
	for _, err := range stream.Lines(fsys, "missing.txt") {
		fmt.Println("  got error:", err)
		fmt.Println("  is fs.ErrNotExist:", errors.Is(err, fs.ErrNotExist))
		break
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Lines of names.txt:
   alice
   bob
   charlie
Reading a missing file:
  got error: open missing.txt: open missing.txt: file does not exist
  is fs.ErrNotExist: true
```

### Tests

The tests cover the success path, both failure paths, and the eager sink. `TestLinesReadsAll` reads a multi-line file and checks every line arrives with a nil error. `TestLinesErrors` is a table that asserts a nil filesystem wraps `ErrNilFS` and a missing file wraps `fs.ErrNotExist`, both detected with `errors.Is`. `TestCollectStopsAtError` drains a missing file through `Collect` and asserts it returns a non-nil error and no spurious values.

Create `stream_test.go`:

```go
package stream

import (
	"errors"
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"
)

func TestLinesReadsAll(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{"f.txt": {Data: []byte("a\nb\nc\n")}}
	var got []string
	for line, err := range Lines(fsys, "f.txt") {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got = append(got, line)
	}
	if !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("lines = %v, want [a b c]", got)
	}
}

func TestLinesErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		fsys fs.FS
		path string
		want error
	}{
		{name: "nil fs", fsys: nil, path: "any.txt", want: ErrNilFS},
		{name: "missing file", fsys: fstest.MapFS{}, path: "missing.txt", want: fs.ErrNotExist},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var got error
			for _, err := range Lines(tc.fsys, tc.path) {
				got = err
				break
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("error = %v, want errors.Is(_, %v)", got, tc.want)
			}
		})
	}
}

func TestCollectStopsAtError(t *testing.T) {
	t.Parallel()

	values, err := Collect(Lines(fstest.MapFS{}, "missing.txt"))
	if err == nil {
		t.Fatal("Collect: expected error for missing file, got nil")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Collect error = %v, want errors.Is(_, fs.ErrNotExist)", err)
	}
	if len(values) != 0 {
		t.Fatalf("Collect returned %v, want no values before the error", values)
	}
}
```

## Review

The pattern is correct when an error is terminal, classifiable, and never accompanied by a usable value. Confirm `Lines` `return`s immediately after yielding an error, so the missing-file and nil-FS paths produce exactly one pair and stop, and that both errors are wrapped with `%w` so the tests' `errors.Is` checks pass — replace a `%w` with `%v` and the sentinel detection breaks. Confirm `Collect` checks the error before appending, so the zero value paired with an error never enters the result slice; the `len(values) != 0` assertion is what catches that slip. The single-use lifetime is real: each range of `Lines` opens the file afresh, which the doc comment promises and which a caller must respect.

The common mistakes are swallowing the error, forgetting the terminal `return`, and dropping the `%w` wrap. Logging the open failure and ending the sequence quietly makes a missing file look like an empty one. Yielding the error but continuing the loop lets a value follow an error, so a caller that breaks on the first error still consumed a bad pair. Wrapping with `%v` instead of `%w` severs the chain `errors.Is` walks, forcing brittle string matching at every call site. Appending in `Collect` before the error check lets a zero value contaminate the result.

## Resources

- [`iter` package: Single-Use Iterators](https://pkg.go.dev/iter#hdr-Single_Use_Iterators) — why an iterator over a consumable resource must document that it can be walked once.
- [`testing/fstest.MapFS`](https://pkg.go.dev/testing/fstest#MapFS) — the in-memory filesystem the demo and tests read from, no real files needed.
- [`errors.Is` and `fmt.Errorf` `%w`](https://pkg.go.dev/errors#Is) — wrapping a sentinel with `%w` so callers classify a streamed error without string matching.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — the line splitter whose `Scan`/`Err` protocol the iterator drives.

---

Back to [02-ordered-map-iterators.md](02-ordered-map-iterators.md) | Next: [04-domain-event-store.md](04-domain-event-store.md)

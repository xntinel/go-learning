# Exercise 18: Compacting a Write-Ahead Log Into a Map Without Aliasing the Scanner

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Bitcask, the storage engine behind Riak, keeps its data as an append-only
write-ahead log of `SET` and delete records and periodically compacts it:
replay the whole log, keep only each key's most recent surviving state, and
write that out as a smaller file. Every earlier exercise in this lesson about
`bufio.Scanner.Bytes()` warned about the same hazard for a *slice* -- retain
a token past the next `Scan` call and it silently becomes whatever the next
line was. A WAL compactor hits the identical hazard one level up, in a *map*.
The index a compactor builds is `map[string][]byte`, one entry per key, and
if the value half of that entry is stored as the scanner's raw token instead
of a copy, the bug does not corrupt one slice -- it corrupts the entire
index, because every map entry that was ever assigned that way ends up
pointing at whatever bytes the scanner's buffer holds by the time compaction
finishes.

The reason this specific bug is easy to miss in review is that it does
nothing wrong for a small enough log. `bufio.Scanner` keeps one internal
buffer and only shifts or refills it when it runs out of unread space, so a
WAL short enough to fit in that buffer in one read never triggers the reuse
at all -- the bug is invisible in exactly the input sizes a developer tests
by hand, and appears only once the log is large enough to force the scanner
to refill, which in production means "once the log has been running for a
while." That is precisely the shape of bug this lesson keeps returning to: a
view into memory that is only valid until the next call, retained past that
point.

This exercise builds `walcompact`, a command that replays a WAL from stdin
and writes the compacted result to stdout, cloning every value it keeps.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
walcompact/                module example.com/walcompact
  go.mod                   go 1.24
  compact.go                package main — Compact(io.Reader, io.Writer) error; ErrMalformedLine
  compact_test.go          package main — SET/DEL table, malformed lines, the aliased-index contrast, run() end to end
  main.go                  package main — no flags, stdin to stdout, exit codes
```

- Files: `compact.go`, `compact_test.go`, `main.go`.
- Implement: `Compact(r io.Reader, w io.Writer) error` replaying `SET key value` and `DEL key` lines into an in-memory index -- cloning every stored value with `bytes.Clone` -- and writing one `SET key value` line per surviving key to `w`, in first-seen key order; sentinel error `ErrMalformedLine` wrapping the offending line number.
- Tool: `walcompact` takes no arguments and reads WAL lines from stdin, writing the compacted log to stdout. Exit 0 on success, exit 2 for an unexpected argument or a malformed WAL line, exit 1 for a read or write failure against the underlying stream.
- Test: ordinary SET/DEL sequences including a later SET overwriting an earlier one, a DEL removing a key from the output entirely, a DEL of a never-SET key, a SET reviving a deleted key at its original first-seen position, and an empty log; every malformed-line shape rejected with `ErrMalformedLine`; the `compactAliased` contrast proving an uncloned index collapses; `run` end to end over stdin, a bad line, and an unexpected argument.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/06-copy-and-full-slice-expression/18-wal-compaction-scanner-token-in-map
cd go-solutions/06-collections-arrays-slices-and-maps/06-copy-and-full-slice-expression/18-wal-compaction-scanner-token-in-map
go mod edit -go=1.24
```

### The scanner-token rule applies to map values exactly as it applies to slices

`bufio.Scanner.Bytes()` documents its own hazard plainly: "the underlying
array may point to data that will be overwritten by a subsequent call to
Scan." Nothing about that warning is specific to slices held in a `[]T`; a
`map[string][]byte` is just as exposed, because the value half of a map
entry is a slice header like any other, pointing at the same shared buffer
if nothing copies it. The naive compactor looks entirely reasonable:

```go
values[key] = fields[2]   // fields[2] is a window into scanner.Bytes()
```

Nothing here is wrong syntactically, and for a short WAL that fits in the
scanner's buffer in a single read, nothing is wrong observably either -- the
buffer never gets reused, so every stored slice happens to stay valid by
accident. Feed it a WAL long enough to force even one buffer refill, though,
and the picture changes: the scanner shifts unread bytes to the front of its
buffer to make room, overwriting the memory a dozen already-stored map
entries still point at, and every one of those entries now reads back
whatever the buffer holds after the shift instead of what it held when it
was assigned. The fix is the same one-line clone used throughout this
lesson, applied at the point of insertion into the map:

```go
values[key] = bytes.Clone(fields[2])
```

The key itself needs no such treatment here: converting a `[]byte` to a
`string` with `string(fields[1])` always allocates a fresh, immutable copy,
so a map keyed by that conversion is safe by construction. It is specifically
the `[]byte` *value* that must be cloned explicitly.

Create `compact.go`:

```go
// Command walcompact compacts a Bitcask-style write-ahead log into its
// latest surviving state per key, in first-seen key order.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

// ErrMalformedLine means a WAL line was not a well-formed "SET key value" or
// "DEL key" record. The error wraps the 1-based line number and the
// offending text.
var ErrMalformedLine = errors.New("walcompact: malformed line")

// Compact replays WAL lines read from r -- each either "SET key value" or
// "DEL key" -- into an in-memory index, then writes one "SET key value"
// line per surviving key to w, in the order each key was first mentioned in
// the log. A key whose most recent operation was DEL does not appear in the
// output at all, even if an earlier SET gave it a value.
//
// Compact holds no state beyond a single call: it is not safe to share one
// r or w between concurrent calls, but independent calls using different
// readers and writers may run concurrently. Every value Compact stores is
// cloned with bytes.Clone before insertion into its index, so it retains no
// reference into the underlying bufio.Scanner's buffer once Compact
// returns.
func Compact(r io.Reader, w io.Writer) error {
	var order []string
	firstSeen := make(map[string]bool)
	values := make(map[string][]byte)
	deleted := make(map[string]bool)

	scanner := bufio.NewScanner(r)
	line := 0
	for scanner.Scan() {
		line++
		// scanner.Bytes() is a view into the scanner's internal buffer,
		// valid only until the next Scan call. bytes.Fields splits it into
		// sub-slices of that same view; they are fine to inspect right
		// here, but anything kept past this iteration must be copied.
		fields := bytes.Fields(scanner.Bytes())
		if len(fields) < 2 {
			return fmt.Errorf("%w: line %d: %q", ErrMalformedLine, line, scanner.Text())
		}

		key := string(fields[1]) // string() copies; the key is always safe to retain
		switch string(fields[0]) {
		case "SET":
			if len(fields) != 3 {
				return fmt.Errorf("%w: line %d: %q", ErrMalformedLine, line, scanner.Text())
			}
			// The value is the field that must be cloned: it is a []byte
			// slice of the scanner's buffer, and that buffer is overwritten
			// on the very next Scan call.
			values[key] = bytes.Clone(fields[2])
			deleted[key] = false
		case "DEL":
			if len(fields) != 2 {
				return fmt.Errorf("%w: line %d: %q", ErrMalformedLine, line, scanner.Text())
			}
			deleted[key] = true
		default:
			return fmt.Errorf("%w: line %d: %q", ErrMalformedLine, line, scanner.Text())
		}

		if !firstSeen[key] {
			firstSeen[key] = true
			order = append(order, key)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("walcompact: reading WAL: %w", err)
	}

	for _, key := range order {
		if deleted[key] {
			continue
		}
		if _, err := fmt.Fprintf(w, "SET %s %s\n", key, values[key]); err != nil {
			return fmt.Errorf("walcompact: writing compacted log: %w", err)
		}
	}
	return nil
}
```

### The tool

`walcompact` is a pure filter -- stdin in, stdout out, no flags -- so `run`
takes only the argument slice, an `io.Reader`, and an `io.Writer`, and
rejects any argument at all rather than silently ignoring it. `Compact`
streams line by line rather than buffering the whole log into memory first,
which matters for the exact reason this exercise exists: a WAL worth
compacting is, by definition, one that has grown too large to keep appending
to forever, and loading all of it into a `[]byte` before processing would
defeat the point. A malformed line is something the caller fixes by
correcting the input, so `run` re-wraps `ErrMalformedLine` under `errUsage`
for exit code 2; a failure reading from stdin or writing to stdout is a
genuine runtime failure and exits 1 instead.

Create `main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// errUsage marks a failure the caller can fix by changing the invocation or
// the input: an unexpected argument, or a WAL line that does not parse.
// main maps it to exit code 2. A read or write failure against the
// underlying stream is a runtime failure instead and maps to exit code 1.
var errUsage = errors.New("usage")

// run compacts a WAL read from stdin and writes the result to stdout. It
// takes no arguments; walcompact is a pure stdin-to-stdout filter. run
// never touches os.Exit, so it can be driven end to end in a test with a
// strings.Reader and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("%w: walcompact takes no arguments, got %v", errUsage, args)
	}
	if err := Compact(stdin, stdout); err != nil {
		if errors.Is(err, ErrMalformedLine) {
			return fmt.Errorf("%w: %v", errUsage, err)
		}
		return err
	}
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "walcompact:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'SET user:1 alice\nSET user:2 bob\nDEL user:1\nSET user:1 alice2\nSET user:3 carol\n' | go run .
printf 'SET user:1 alice\nPUT user:2 bob\n' | go run .
```

Expected output:

```text
SET user:1 alice2
SET user:2 bob
SET user:3 carol
walcompact: usage: walcompact: malformed line: line 2: "PUT user:2 bob"
```

The first command's log sets, deletes, and resets `user:1`, sets `user:2`
once, and sets `user:3` last; the compacted output carries each key's final
value exactly once, in the order it was first mentioned -- `user:1` still
occupies the first line even though its surviving value came from the fourth
line of the log. The second command's second line uses an unrecognized
command; `Compact` reports the malformed line and number, `run` wraps it
under `errUsage`, and the process exits 2.

### Tests

`TestCompact` is the replay table: ordinary sequential `SET`s, a later `SET`
overwriting an earlier one without moving its position, a `DEL` that removes
a key from the output entirely, a `DEL` of a key that was never `SET`, a
`SET` that revives a deleted key at its original first-seen position, and an
empty log. `TestCompactRejectsMalformedLines` sweeps every shape of bad line
-- an unknown command, a `SET` missing its value, extra fields on either
command, a bare key with no command at all -- against `ErrMalformedLine`.

`TestCompactAliasedCollapsesEveryKeyToTheLastLine` is the heart of the
module. `compactAliased` is unexported and unreachable from the tool; it
builds the same kind of index as `Compact` but stores the scanner's token
directly, and configures a deliberately small scanner buffer so the refill
this bug depends on happens on nearly every line -- exactly what a
multi-kilobyte production WAL would do without any special configuration.
Fed three `SET` lines for three distinct keys, `Compact`'s output keeps all
three values distinct while `compactAliased`'s map collapses every one of
them to the log's last line. `TestRun` drives the command end to end: a
compaction on stdin, a malformed line, and an unexpected argument.

Create `compact_test.go`:

```go
package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestCompact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		wal  string
		want string
	}{
		{
			name: "ordinary SETs in first-seen order",
			wal:  "SET a 1\nSET b 2\nSET c 3\n",
			want: "SET a 1\nSET b 2\nSET c 3\n",
		},
		{
			name: "later SET wins, first-seen position kept",
			wal:  "SET a 1\nSET b 2\nSET a 9\n",
			want: "SET a 9\nSET b 2\n",
		},
		{
			name: "DEL removes the key from the output entirely",
			wal:  "SET a 1\nSET b 2\nDEL a\n",
			want: "SET b 2\n",
		},
		{
			name: "DEL of a key never SET produces no output for it",
			wal:  "DEL ghost\nSET a 1\n",
			want: "SET a 1\n",
		},
		{
			name: "SET after DEL revives the key at its first-seen position",
			wal:  "SET a 1\nSET b 2\nDEL a\nSET a 7\n",
			want: "SET a 7\nSET b 2\n",
		},
		{
			name: "empty input produces empty output",
			wal:  "",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			if err := Compact(strings.NewReader(tc.wal), &out); err != nil {
				t.Fatalf("Compact: %v", err)
			}
			if out.String() != tc.want {
				t.Fatalf("Compact output = %q, want %q", out.String(), tc.want)
			}
		})
	}
}

func TestCompactRejectsMalformedLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		wal  string
	}{
		{name: "unknown command", wal: "PUT a 1\n"},
		{name: "SET missing value", wal: "SET a\n"},
		{name: "SET with extra field", wal: "SET a 1 extra\n"},
		{name: "DEL with extra field", wal: "DEL a extra\n"},
		{name: "bare key with no command", wal: "a\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			err := Compact(strings.NewReader(tc.wal), &out)
			if !errors.Is(err, ErrMalformedLine) {
				t.Fatalf("Compact(%q) err = %v, want ErrMalformedLine", tc.wal, err)
			}
		})
	}
}

// compactAliased is the version of compaction that a first pass at this
// tool tends to ship: it stores the scanner's own token directly instead of
// cloning it. It is unexported and unreachable from the tool; it exists
// only so the tests can observe what that aliasing does once scanning
// finishes.
func compactAliased(r io.Reader) (map[string][]byte, error) {
	values := make(map[string][]byte)
	scanner := bufio.NewScanner(r)
	// A small internal buffer forces the scanner to refill and compact
	// often, which is exactly the condition -- routine on any WAL longer
	// than a few kilobytes -- that overwrites earlier tokens still held by
	// an uncloned reference.
	scanner.Buffer(make([]byte, 8), 64)
	for scanner.Scan() {
		fields := bytes.Fields(scanner.Bytes())
		if len(fields) != 3 || string(fields[0]) != "SET" {
			continue
		}
		key := string(fields[1])
		values[key] = fields[2] // BUG: aliases the scanner's internal buffer
	}
	return values, scanner.Err()
}

// TestCompactAliasedCollapsesEveryKeyToTheLastLine is the heart of this
// module. Fed three SET lines for three distinct keys, Compact's cloned
// values stay distinct; compactAliased's map ends up with every key
// pointing at the same overwritten buffer, so every value reads back as the
// log's last line once scanning has finished.
func TestCompactAliasedCollapsesEveryKeyToTheLastLine(t *testing.T) {
	t.Parallel()

	wal := "SET a 1\nSET b 2\nSET c 3\n"

	var out bytes.Buffer
	if err := Compact(strings.NewReader(wal), &out); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	want := "SET a 1\nSET b 2\nSET c 3\n"
	if out.String() != want {
		t.Fatalf("Compact output = %q, want %q", out.String(), want)
	}

	aliased, err := compactAliased(strings.NewReader(wal))
	if err != nil {
		t.Fatalf("compactAliased: %v", err)
	}
	if len(aliased) != 3 {
		t.Fatalf("compactAliased returned %d keys, want 3", len(aliased))
	}
	for _, key := range []string{"a", "b", "c"} {
		if string(aliased[key]) != "3" {
			t.Errorf("compactAliased[%q] = %q, want %q (the last line's value, aliased into every key)", key, aliased[key], "3")
		}
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	t.Run("compacts stdin to stdout", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		wal := "SET a 1\nDEL a\nSET a 2\nSET b 3\n"
		if err := run(nil, strings.NewReader(wal), &out); err != nil {
			t.Fatalf("run: %v", err)
		}
		want := "SET a 2\nSET b 3\n"
		if out.String() != want {
			t.Fatalf("run stdout = %q, want %q", out.String(), want)
		}
	})

	t.Run("malformed line is a usage error", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		err := run(nil, strings.NewReader("GARBAGE\n"), &out)
		if !errors.Is(err, errUsage) {
			t.Fatalf("run err = %v, want it to wrap errUsage", err)
		}
	})

	t.Run("unexpected argument is a usage error", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		err := run([]string{"extra"}, strings.NewReader(""), &out)
		if !errors.Is(err, errUsage) {
			t.Fatalf("run err = %v, want it to wrap errUsage", err)
		}
	})
}
```

## Review

`Compact` is correct when the compacted output reflects each key's most
recent operation and nothing else -- `TestCompact`'s table pins overwrite,
delete, revive, and the never-set-then-deleted edge case, all in first-seen
order. `TestCompactAliasedCollapsesEveryKeyToTheLastLine` is the module's
core lesson: storing `fields[2]` directly as a map value is syntactically
identical to storing a cloned copy and produces identical results for any
input small enough to never force a scanner refill, which is exactly why
this bug reaches production. The single `bytes.Clone` at the point of
insertion is what makes `Compact` immune to it regardless of log size.
Malformed input -- an unknown command, a missing field, an extra one -- is
reported with the line number via `ErrMalformedLine`, checkable with
`errors.Is`, and `run` maps that to exit code 2 while reserving exit code 1
for an actual stream failure. Run `go test -count=1 -race ./...` to confirm
the replay table, the malformed-line sweep, the aliasing contrast, and
`run`'s behavior end to end.

## Resources

- [`bufio.Scanner.Bytes`](https://pkg.go.dev/bufio#Scanner.Bytes) — documents the exact hazard this module builds around: the returned slice may be overwritten by a subsequent `Scan`.
- [`bytes.Clone`](https://pkg.go.dev/bytes#Clone) — the copy this module inserts at the map-write boundary.
- [`bufio.Scanner.Buffer`](https://pkg.go.dev/bufio#Scanner.Buffer) — used in the test to force the buffer reuse that triggers the bug deterministically.
- [Bitcask: A Log-Structured Hash Table](https://riak.com/assets/bitcask-intro.pdf) — the storage engine whose compaction this exercise models.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-loadbalancer-rotate-in-place.md](17-loadbalancer-rotate-in-place.md) | Next: [19-leaderboard-topn-clone-before-sort.md](19-leaderboard-topn-clone-before-sort.md)

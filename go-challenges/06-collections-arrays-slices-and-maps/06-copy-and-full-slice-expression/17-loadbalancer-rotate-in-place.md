# Exercise 17: Rotating a Round-Robin Server List In Place

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

HAProxy and Envoy both keep a round-robin upstream list and rotate it after
every pick, so the next request starts from the next server instead of the
one that was just used. Rotating that list is a textbook `copy` exercise: you
can do it in place, with one temporary buffer sized to the amount you are
rotating by, instead of allocating a whole second copy of the list. Earlier
exercises in this lesson taught that `copy` is safe even when its source and
destination overlap -- it behaves like `memmove`, not `memcpy` -- and that
fact is exactly what makes the shift step of an in-place rotation correct.
This exercise is about the step on either side of that shift, where the same
overlap safety does not save you if you get the *order* of operations wrong.

An in-place left rotation by `k` needs three moves: save the `k`-byte prefix
that is about to be overwritten, shift the remaining `len(buf)-k` bytes left
by `k` (this is the step `copy`'s overlap safety covers), then write the
saved prefix into the tail the shift just freed. Writing the prefix back
*before* the shift, instead of after, looks like a harmless reordering of two
independent steps -- and it is not. The tail write clobbers part of the exact
region the shift is about to read as its source, so the shift's `copy` call
faithfully copies data that has already been partially destroyed. `copy`
itself never misbehaves; the bug is entirely in what the two calls are told
to read and write, and in which order.

This exercise builds `lbrotate`, a command that reads a whitespace-separated
server list from stdin and writes it rotated left by `k` positions.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
lbrotate/                 module example.com/lbrotate
  go.mod                  go 1.24
  rotate.go               package main — rotateLeft(buf []byte, k int) error; ErrInvalidRotation
  rotate_test.go          package main — rotation table, out-of-range k, the buggy-order contrast, run() end to end
  main.go                 package main — -k flag, exit codes
```

- Files: `rotate.go`, `rotate_test.go`, `main.go`.
- Implement: `rotateLeft(buf []byte, k int) error` rotating `buf` left by `k` in place using a single `k`-byte temporary buffer, returning `ErrInvalidRotation` for a negative `k` or a `k` greater than `len(buf)`.
- Tool: `lbrotate -k int` (default `1`) reads whitespace-separated server names from stdin, reduces `k` modulo the list length so any integer including a negative one is accepted, and writes the rotated list to stdout as one space-separated line. Exit 0 on success, exit 2 for an unparseable `-k` or a server list longer than the tool's 256-entry limit; no runtime failure path exists once the flags and input validate.
- Test: an ordinary rotation, `k == 0`, `k == len(buf)`, a single element, an empty buffer; `ErrInvalidRotation` for a negative `k`, a `k` past the end, and `k` on an empty buffer; the `rotateLeftBuggy` contrast proving the wrong operation order destroys three bytes outright; `run` end to end over `-k` values including negative and oversized ones, an empty list, and a bad flag.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/06-copy-and-full-slice-expression/17-loadbalancer-rotate-in-place
cd go-solutions/06-collections-arrays-slices-and-maps/06-copy-and-full-slice-expression/17-loadbalancer-rotate-in-place
go mod edit -go=1.24
```

### `copy`'s overlap safety covers one step, not the whole algorithm

`copy(dst, src)` is defined to behave correctly no matter how `dst` and `src`
overlap, which is why `copy(buf, buf[k:])` -- shifting the tail of a slice
down over its own head -- is a completely safe, idiomatic way to shift data
left in place. That guarantee is scoped to a single `copy` call, though. It
says nothing about what state `buf` needs to be in *before* that call runs,
and a rotation needs the region `buf[k:]` to still hold its original data
when the shift reads it. The buggy order violates exactly that:

```go
prefix := make([]byte, k)
copy(prefix, buf[:k])          // save prefix -- fine
copy(buf[len(buf)-k:], prefix) // BUG: write the tail *before* shifting
copy(buf, buf[k:])             // shift now reads a tail it just overwrote
```

Trace `rotateLeftBuggy([]byte("ABCDEFGH"), 3)` by hand: the prefix `"ABC"` is
saved, then written into `buf[5:8]`, turning the buffer into
`"ABCDEA" + "BC"` -- the original `"FGH"` in that tail is gone, replaced by
the very prefix that is about to be shifted into a completely different
position. The final shift then copies from that already-corrupted tail, and
`F`, `G`, and `H` never appear anywhere in the result. Writing the prefix
back has to happen *after* the shift, once `buf[len(buf)-k:]` is no longer
needed as a source -- only then is overwriting it safe.

Create `rotate.go`:

```go
// Command lbrotate rotates a round-robin server list left by k positions,
// the operation a load balancer performs on its upstream list after every
// pick so the next request starts from the next server.
package main

import (
	"errors"
	"fmt"
)

// ErrInvalidRotation means k was negative or greater than len(buf).
// rotateLeft never wraps k modulo len(buf) itself; callers that receive an
// arbitrary k must normalize it first.
var ErrInvalidRotation = errors.New("lbrotate: rotation out of range")

// rotateLeft rotates buf left by k positions in place, using exactly one
// temporary buffer of length k.
//
// It is not safe to call concurrently with another operation on the same
// buf: it mutates buf's contents directly and holds no lock of its own. It
// returns ErrInvalidRotation if k is negative or exceeds len(buf); an empty
// buf or k == 0 is a valid no-op.
//
// The algorithm depends on doing three steps in exactly this order: save the
// prefix that the shift is about to overwrite, perform the shift, then
// write the saved prefix into the space the shift freed up. copy is safe to
// use for the shift step even though its source and destination overlap --
// copy is defined to behave correctly for any overlap, like C's memmove --
// but that guarantee only covers the shift itself. Writing the saved prefix
// back before the shift runs is a different mistake: it overwrites part of
// the very region the shift is about to read from, and the shift then
// copies corrupted data. See rotateLeftBuggy in the tests for exactly that
// failure, pinned against this correct ordering.
func rotateLeft(buf []byte, k int) error {
	if k < 0 || k > len(buf) {
		return fmt.Errorf("%w: k=%d, len=%d", ErrInvalidRotation, k, len(buf))
	}
	if k == 0 || len(buf) == 0 {
		return nil
	}

	prefix := make([]byte, k)
	copy(prefix, buf[:k])          // save the region the shift is about to overwrite
	copy(buf, buf[k:])             // shift the remainder left; overlap-safe
	copy(buf[len(buf)-k:], prefix) // write the saved prefix into the freed tail
	return nil
}
```

### The tool

`lbrotate` has one integer flag and reads its only other input from stdin, so
`run` takes the raw argument slice plus an `io.Reader` for stdin and an
`io.Writer` for stdout -- the same shape used throughout this lesson's tools
-- and never touches `os.Exit`. A real round-robin cursor is naturally
represented as `k = 1` per pick, but `run` accepts any integer `k`, including
negative ones for a right rotation, by reducing it modulo the list length
before it ever reaches `rotateLeft`; `rotateLeft`'s own out-of-range checks
exist for direct callers of the package function, not because `run` expects
to trigger them. The server list itself is turned into a byte-sized
permutation -- one byte per position -- so `rotateLeft` can rotate the
*order* rather than the variable-length server names directly; a 256-entry
cap on the list enforces that a position always fits in one byte, comfortably
above any real backend list. A bad `-k` value or an oversized list are both
invocation mistakes and map to exit code 2; there is no separate runtime
failure path, because once the input parses, the rotation cannot fail.

Create `main.go`:

```go
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// errUsage marks a failure the caller can fix by changing the invocation: a
// bad -k value, or a server list too long for the single-byte position
// index this tool uses internally. main maps it to exit code 2.
var errUsage = errors.New("usage")

// maxServers is the largest list rotateLeft's byte-indexed permutation can
// represent: one byte per position. No real HAProxy or Envoy backend list
// comes close to this, so the limit is never a practical concern.
const maxServers = 256

// run reads whitespace-separated server names from stdin, rotates them left
// by k positions (negative k rotates right; k is reduced modulo the list
// length so any integer is accepted), and writes the rotated list to
// stdout as a single space-separated line. It never touches os.Exit, so it
// can be driven end to end in a test with a strings.Reader and a
// bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("lbrotate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	k := fs.Int("k", 1, "positions to rotate left (negative rotates right)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	data, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("reading server list: %w", err)
	}
	servers := strings.Fields(string(data))
	n := len(servers)
	if n > maxServers {
		return fmt.Errorf("%w: %d servers exceeds the %d-server limit", errUsage, n, maxServers)
	}
	if n == 0 {
		fmt.Fprintln(stdout)
		return nil
	}

	// Represent the rotation as a permutation of single-byte positions,
	// rotate that permutation, then reorder the actual server names by it.
	positions := make([]byte, n)
	for i := range positions {
		positions[i] = byte(i)
	}

	effectiveK := ((*k % n) + n) % n // reduce an arbitrary k into [0, n)
	if err := rotateLeft(positions, effectiveK); err != nil {
		return fmt.Errorf("rotating server list: %w", err)
	}

	rotated := make([]string, n)
	for i, pos := range positions {
		rotated[i] = servers[pos]
	}
	fmt.Fprintln(stdout, strings.Join(rotated, " "))
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "lbrotate:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'web1 web2 web3 web4 web5' | go run . -k 2
printf 'web1 web2 web3' | go run . -k -1
printf 'a b' | go run . -k bogus
```

Expected output:

```text
web3 web4 web5 web1 web2
web3 web1 web2
lbrotate: usage: invalid value "bogus" for flag -k: parse error
```

The first command rotates a five-server list left by two: `web1` and `web2`
move to the end, everything else shifts down, exit 0. The second command
passes `-k -1`, which the modulo reduction turns into a right rotation --
`web3` moves from the end to the front -- confirming negative `k` is
accepted rather than rejected. The third command's `-k bogus` fails flag
parsing before `run` ever reads stdin; the error wraps `errUsage` and the
process exits 2.

### Tests

`TestRotateLeft` is the rotation table: an ordinary rotation, `k == 0`,
`k == len(buf)` (a full rotation is the identity), a single element, and an
empty buffer with `k == 0`. `TestRotateLeftRejectsOutOfRangeK` checks
`ErrInvalidRotation` for a negative `k`, a `k` past the end, and any nonzero
`k` on an empty buffer. `TestRotateLeftBuggyCorruptsTheRotation` is the
module's core lesson: `rotateLeftBuggy` performs the identical three `copy`
calls as `rotateLeft` with the tail write moved earlier, and the test proves
that ordering does not just reorder the result -- it destroys three bytes
that a correct rotation must preserve, and none of the three ever reappear
in the buggy output. `TestRun` and `TestRunRejectsTooManyServers` drive the
command end to end across ordinary, negative, oversized, default, and empty
`-k`/list combinations, plus the unparseable-flag usage error.

Create `rotate_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestRotateLeft(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		buf  string
		k    int
		want string
	}{
		{name: "rotate by three of eight", buf: "ABCDEFGH", k: 3, want: "DEFGHABC"},
		{name: "rotate by one", buf: "ABCD", k: 1, want: "BCDA"},
		{name: "k equals zero is a no-op", buf: "ABCD", k: 0, want: "ABCD"},
		{name: "k equals len is identity", buf: "ABCD", k: 4, want: "ABCD"},
		{name: "single element", buf: "A", k: 1, want: "A"},
		{name: "empty buffer with k zero", buf: "", k: 0, want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			buf := []byte(tc.buf)
			if err := rotateLeft(buf, tc.k); err != nil {
				t.Fatalf("rotateLeft: %v", err)
			}
			if string(buf) != tc.want {
				t.Fatalf("rotateLeft(%q, %d) = %q, want %q", tc.buf, tc.k, buf, tc.want)
			}
		})
	}
}

func TestRotateLeftRejectsOutOfRangeK(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		buf  string
		k    int
	}{
		{name: "negative k", buf: "ABCD", k: -1},
		{name: "k greater than len", buf: "ABCD", k: 5},
		{name: "k on empty buffer", buf: "", k: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			buf := []byte(tc.buf)
			if err := rotateLeft(buf, tc.k); !errors.Is(err, ErrInvalidRotation) {
				t.Fatalf("rotateLeft(%q, %d) err = %v, want ErrInvalidRotation", tc.buf, tc.k, err)
			}
		})
	}
}

// rotateLeftBuggy performs the same three copies as rotateLeft but in the
// wrong order: it writes the saved prefix into the tail before shifting the
// remainder, instead of after. It is unexported and unreachable from the
// tool; it exists only so the tests can pin exactly how that ordering
// corrupts the rotation.
func rotateLeftBuggy(buf []byte, k int) error {
	if k < 0 || k > len(buf) {
		return ErrInvalidRotation
	}
	if k == 0 || len(buf) == 0 {
		return nil
	}
	prefix := make([]byte, k)
	copy(prefix, buf[:k])
	copy(buf[len(buf)-k:], prefix) // BUG: written before the shift below runs
	copy(buf, buf[k:])             // shift now reads from a tail it just corrupted
	return nil
}

// TestRotateLeftBuggyCorruptsTheRotation is the heart of this module. Both
// functions save the same k-byte prefix and perform the same two copy
// calls; only the order differs. rotateLeftBuggy writes the prefix into the
// tail first, so by the time the shift reads buf[k:], part of what it reads
// is the prefix it just wrote there instead of the original data -- and the
// bytes that region held are gone, not merely reordered.
func TestRotateLeftBuggyCorruptsTheRotation(t *testing.T) {
	t.Parallel()

	correct := []byte("ABCDEFGH")
	if err := rotateLeft(correct, 3); err != nil {
		t.Fatalf("rotateLeft: %v", err)
	}
	if want := "DEFGHABC"; string(correct) != want {
		t.Fatalf("rotateLeft = %q, want %q", correct, want)
	}

	buggy := []byte("ABCDEFGH")
	if err := rotateLeftBuggy(buggy, 3); err != nil {
		t.Fatalf("rotateLeftBuggy: %v", err)
	}
	if string(buggy) == string(correct) {
		t.Fatalf("rotateLeftBuggy produced the correct rotation %q; expected it to corrupt the buffer", buggy)
	}
	// F, G, and H existed exactly once in the source. The buggy ordering
	// overwrites the tail with the saved prefix before the shift ever reads
	// that region, so these three bytes are destroyed, not merely
	// reordered -- they do not appear anywhere in the buggy result.
	for _, lost := range []byte("FGH") {
		if bytes.ContainsRune(buggy, rune(lost)) {
			t.Errorf("rotateLeftBuggy result %q unexpectedly still contains %q; expected the ordering bug to have destroyed it", buggy, lost)
		}
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		stdin   string
		want    string
		wantErr bool
		usage   bool
	}{
		{
			name:  "rotate a five-server list by two",
			args:  []string{"-k", "2"},
			stdin: "web1 web2 web3 web4 web5",
			want:  "web3 web4 web5 web1 web2\n",
		},
		{
			name:  "negative k rotates right",
			args:  []string{"-k", "-1"},
			stdin: "web1 web2 web3",
			want:  "web3 web1 web2\n",
		},
		{
			name:  "k larger than the list wraps modulo the length",
			args:  []string{"-k", "7"},
			stdin: "a b c",
			want:  "b c a\n",
		},
		{
			name:  "default k is one",
			args:  nil,
			stdin: "a b c",
			want:  "b c a\n",
		},
		{
			name:  "empty server list",
			args:  []string{"-k", "3"},
			stdin: "   \n  ",
			want:  "\n",
		},
		{
			name:    "unparseable k is a usage error",
			args:    []string{"-k", "not-a-number"},
			stdin:   "a b",
			wantErr: true,
			usage:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.stdin), &stdout)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("run(%v): want error, got nil", tc.args)
				}
				if tc.usage && !errors.Is(err, errUsage) {
					t.Fatalf("run(%v) err = %v, want it to wrap errUsage", tc.args, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("run(%v): %v", tc.args, err)
			}
			if stdout.String() != tc.want {
				t.Fatalf("run(%v) stdout = %q, want %q", tc.args, stdout.String(), tc.want)
			}
		})
	}
}

func TestRunRejectsTooManyServers(t *testing.T) {
	t.Parallel()

	tokens := make([]string, maxServers+1)
	for i := range tokens {
		tokens[i] = "s"
	}
	var stdout bytes.Buffer
	err := run(nil, strings.NewReader(strings.Join(tokens, " ")), &stdout)
	if !errors.Is(err, errUsage) {
		t.Fatalf("run err = %v, want it to wrap errUsage", err)
	}
}
```

## Review

`rotateLeft` is correct when the three `copy` calls run in exactly this
order: save the prefix, shift the remainder, write the prefix into the freed
tail. `TestRotateLeftBuggyCorruptsTheRotation` shows what moving the third
step before the second one does -- it is not a cosmetic difference, it
destroys data, because the shift ends up reading part of what it was
supposed to preserve after that region has already been overwritten. `copy`'s
overlap safety, which earlier exercises relied on for `s[lo:hi]`-style
in-place deletes, is real and still holds for the shift step alone; it just
does not extend to guaranteeing the order of unrelated `copy` calls around
it. `rotateLeft` itself rejects a negative or out-of-range `k` with
`ErrInvalidRotation`, checkable with `errors.Is`, while `run` normalizes any
integer `k` modulo the list length before it ever reaches that check, and
reserves exit code 2 for the two invocation mistakes -- a bad flag or an
oversized list -- it can actually produce. Run
`go test -count=1 -race ./...` to confirm the rotation table, the
out-of-range cases, the buggy-order contrast, and `run`'s behavior end to
end.

## Resources

- [`copy`](https://pkg.go.dev/builtin#copy) — the built-in this module relies on, including its overlap-safe, `memmove`-like behavior.
- [Go Specification: Appending to and copying slices](https://go.dev/ref/spec#Appending_and_copying_slices) — the formal definition of what `copy` guarantees.
- [HAProxy: Load Balancing Algorithms (roundrobin)](https://docs.haproxy.org/2.8/configuration.html#4-balance) — the real rotation this exercise's server list models.
- [`flag.NewFlagSet`](https://pkg.go.dev/flag#NewFlagSet) — `flag.ContinueOnError`, used so `run` can return a parse error instead of exiting.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-fanout-log-shipper-clone-per-sink.md](16-fanout-log-shipper-clone-per-sink.md) | Next: [18-wal-compaction-scanner-token-in-map.md](18-wal-compaction-scanner-token-in-map.md)

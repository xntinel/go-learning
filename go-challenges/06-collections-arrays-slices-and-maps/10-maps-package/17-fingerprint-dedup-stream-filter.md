# Exercise 17: Certificate Fingerprint Dedup Stream Filter

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A Certificate Transparency log monitor tailing a mirror sees the same
certificate fingerprint repeatedly: CT logs merge submissions from many
clients, mirrors replay their backlog on reconnect, and a monitor watching
for a domain's certificates cannot assume the feed is already deduplicated
upstream. It has to dedup itself, in a single streaming pass, without
buffering the whole feed in memory first. `00-concepts.md` names the exact
idiom for this: when the only question is membership, `map[K]struct{}` is
the canonical Go set — the zero-width value stores nothing, and the
comma-ok test `_, ok := set[k]` is the O(1) membership check, independent
of how many fingerprints have already gone by.

The trap is reaching for a `[]string` instead, because a slice is the more
familiar collection and "just scan it" reads as simpler than "build a set."
It is simpler to write and identical in output — until the input gets long,
at which point every single line costs more to check than the one before
it, because a linear scan over an ever-growing slice inspects every prior
entry it can before concluding a fingerprint is new. This module builds the
map-backed streaming filter and pins that growth with a property test:
not a measured duration, which is not portable across machines and CI
runners, but a literal count of comparisons the naive version performs.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
fpdedup/                       module example.com/fpdedup
  go.mod                       go 1.24
  fpdedup.go                   package main — ValidateFingerprint, Dedup, New, Seen, Duplicates
  fpdedup_test.go              package main — validation table, Dedup table,
                               the seenSlice comparison-growth property, run() end to end
  main.go                      package main — no flags, stdin only, exit codes
```

- Files: `fpdedup.go`, `fpdedup_test.go`, `main.go`.
- Implement: `ValidateFingerprint(fp string) error` rejecting a non-hex or wrong-length string with `ErrMalformedFingerprint`; `Dedup` holding a `map[string]struct{}` of fingerprints seen so far; `New() *Dedup`; `(*Dedup) Seen(fp string) (firstSeen bool, err error)`; `(*Dedup) Duplicates() int`.
- Tool: `fpdedup` reads one fingerprint per line from stdin, streams each first-seen fingerprint to stdout as it arrives, and prints the duplicate count to stderr once the stream ends. It takes no flags or arguments. Exit 0 on success, exit 2 on a malformed fingerprint line or an unexpected argument, exit 1 is reserved for a runtime failure reading stdin.
- Test: fingerprint validation including too-short, too-long, and non-hex; `Dedup.Seen` across a first sighting, a repeat, an unrelated new fingerprint, and a malformed line; the `seenSlice` property test pinning that its per-call comparison count grows with the input; `run` end to end over a `strings.Reader` and two `bytes.Buffer`s for stdout and stderr.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/fpdedup
cd ~/go-exercises/fpdedup
go mod init example.com/fpdedup
go mod edit -go=1.24
```

### A set costs the same on line one and line one million

`Dedup.Seen` does one thing per call: look up `fp` in a `map[string]struct{}`,
record it if absent, and report which case happened. The cost of that
lookup does not depend on how many fingerprints came before it — that is
the whole point of a hash-based set, and it is exactly the property a
streaming filter over an unbounded feed needs, because the feed does not
come with a promise about how long it runs.

The version this module contrasts against keeps the same fingerprints in a
`[]string` and checks membership by scanning:

```go
func contains(seen []string, fp string) bool {
	for _, s := range seen {
		if s == fp {
			return true
		}
	}
	return false
}
```

This reads as an obviously correct, obviously simple piece of code, and it
is — for correctness. What it hides is that every call which turns out to
be a *new* fingerprint (the common case in a healthy feed) has to walk the
entire slice built so far before it can conclude "not present," so the
per-line cost grows with every line already processed. Over a long-running
monitor process, that turns a linear-time job into a quadratic one, and
nothing about the function's signature gives any hint that it happened.

Create `fpdedup.go`:

```go
// Command fpdedup streams certificate fingerprints from stdin and passes
// through only the first occurrence of each one, the way a Certificate
// Transparency log monitor deduplicates a mirror feed in a single pass.
package main

import (
	"encoding/hex"
	"errors"
	"fmt"
)

// FingerprintLen is the required length, in hex characters, of a
// certificate fingerprint this filter accepts: a SHA-256 digest, 32 bytes,
// hex-encoded.
const FingerprintLen = 64

// ErrMalformedFingerprint is returned when a line is not exactly
// FingerprintLen hex characters.
var ErrMalformedFingerprint = errors.New("fpdedup: malformed fingerprint")

// ValidateFingerprint reports whether fp is exactly FingerprintLen hex
// characters, returning an error wrapping ErrMalformedFingerprint otherwise.
func ValidateFingerprint(fp string) error {
	if len(fp) != FingerprintLen {
		return fmt.Errorf("%w: %q has length %d, want %d", ErrMalformedFingerprint, fp, len(fp), FingerprintLen)
	}
	if _, err := hex.DecodeString(fp); err != nil {
		return fmt.Errorf("%w: %q is not hex: %v", ErrMalformedFingerprint, fp, err)
	}
	return nil
}

// Dedup is a streaming membership filter over certificate fingerprints. It
// holds the fingerprints seen so far in a map[string]struct{} -- zero-width
// values, O(1) membership -- so a Seen call costs the same whether it is
// the first fingerprint of the stream or the millionth.
//
// Dedup is not safe for concurrent use; the caller must synchronize access
// from more than one goroutine.
type Dedup struct {
	seen       map[string]struct{}
	duplicates int
}

// New returns an empty Dedup ready to filter a stream of fingerprints.
func New() *Dedup {
	return &Dedup{seen: make(map[string]struct{})}
}

// Seen validates fp and reports whether this is the first time it has been
// passed to this Dedup. A malformed fingerprint returns
// ErrMalformedFingerprint and counts as neither a first sighting nor a
// duplicate. Every repeat sighting increments the count Duplicates reports.
func (d *Dedup) Seen(fp string) (firstSeen bool, err error) {
	if err := ValidateFingerprint(fp); err != nil {
		return false, err
	}
	if _, ok := d.seen[fp]; ok {
		d.duplicates++
		return false, nil
	}
	d.seen[fp] = struct{}{}
	return true, nil
}

// Duplicates reports how many times Seen has been called with a
// fingerprint it had already recorded.
func (d *Dedup) Duplicates() int {
	return d.duplicates
}
```

### The tool

`fpdedup` streams: it never reads the whole input into memory, and it
writes each first-seen fingerprint the moment `Seen` reports it, rather than
collecting a slice of survivors to print at the end. That is what a monitor
process tailing a live feed needs, and it is also what makes `run`
testable in constant memory regardless of how large the test's input
string is. `run` takes separate `stdout` and `stderr` writers, because the
tool's two outputs are genuinely different streams with different
audiences — the deduplicated fingerprints downstream tooling consumes, and
a duplicate count an operator reads off stderr — and keeping them apart in
the signature is what lets a test assert on each independently instead of
parsing one interleaved buffer.

Create `main.go`:

```go
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
)

// errUsage marks a failure the caller can fix by changing the input: a
// malformed fingerprint line, or an unexpected argument. main maps it to
// exit code 2; every other error maps to exit code 1.
var errUsage = errors.New("usage")

// run streams fingerprints from stdin, writing each first-seen one to
// stdout as it arrives, and reports the duplicate count to stderr once the
// stream ends. It takes no arguments. run never touches os.Exit, so it can
// be exercised in a test with a strings.Reader and two bytes.Buffers.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("%w: fpdedup takes no arguments; it reads stdin", errUsage)
	}

	d := New()
	sc := bufio.NewScanner(stdin)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if line == "" {
			continue
		}
		firstSeen, err := d.Seen(line)
		if err != nil {
			return fmt.Errorf("%w: line %d: %v", errUsage, lineNo, err)
		}
		if firstSeen {
			fmt.Fprintln(stdout, line)
		}
	}
	if err := sc.Err(); err != nil {
		return err // runtime failure reading stdin
	}

	fmt.Fprintln(stderr, "duplicates:", d.Duplicates())
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "fpdedup:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1\nb2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2\na1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1\n' | go run .
printf 'not-a-fingerprint\n' | go run .
```

Expected output:

```text
a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1
b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2
duplicates: 1
fpdedup: usage: line 1: fpdedup: malformed fingerprint: "not-a-fingerprint" has length 17, want 64
```

The first run streams two distinct fingerprints to stdout the moment each
first arrives, then reports one duplicate on stderr once the third line (a
repeat of the first) has been counted rather than printed again. The
second run shows the exit-2 usage path: `"not-a-fingerprint"` is 17
characters, not the required 64, so `ValidateFingerprint` fails before
`Seen` ever touches the map.

### Tests

`TestValidateFingerprint` is the boundary table: exactly right, one
character short, one character long, a non-hex character, and empty.
`TestDedupSeen` walks a small sequence by hand — a first sighting, the same
fingerprint again, an unrelated new one, then a malformed line — checking
both the return values and that `Duplicates` only advances on an actual
repeat.

`TestSeenSliceComparisonsGrowWithInput` is the module's reason to exist.
`seenSlice` is unexported and unreachable from any exported function; its
`contains` method returns not just a boolean but the number of element
comparisons the linear scan needed, which is what lets the test state the
defect as a number instead of a stopwatch reading. Feeding it 500 distinct
fingerprints and checking the *last* one's comparison count is what proves
the scan cost grew with the input: by fingerprint 500, the scan had (up to)
499 prior entries to walk before it could conclude "not present." The
map-backed `Dedup`, run over the identical 500 distinct fingerprints in the
same test, needs no such count — `map[string]struct{}` access does not
degrade as the map grows, which is the property `00-concepts.md` names as
the reason to prefer it over a slice for a set.

Create `fpdedup_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func fingerprint(i int) string {
	return fmt.Sprintf("%064x", i)
}

func TestValidateFingerprint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		fp      string
		wantErr bool
	}{
		{name: "valid", fp: fingerprint(1)},
		{name: "too short", fp: strings.Repeat("a", 63), wantErr: true},
		{name: "too long", fp: strings.Repeat("a", 65), wantErr: true},
		{name: "non-hex character", fp: strings.Repeat("g", 64), wantErr: true},
		{name: "empty", fp: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateFingerprint(tc.fp)
			if tc.wantErr != (err != nil) {
				t.Fatalf("ValidateFingerprint(%q) err = %v, wantErr %v", tc.fp, err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, ErrMalformedFingerprint) {
				t.Fatalf("ValidateFingerprint(%q) err = %v, want ErrMalformedFingerprint", tc.fp, err)
			}
		})
	}
}

func TestDedupSeen(t *testing.T) {
	t.Parallel()

	d := New()

	first, err := d.Seen(fingerprint(1))
	if err != nil || !first {
		t.Fatalf("Seen(first) = %v, %v, want true, nil", first, err)
	}
	repeat, err := d.Seen(fingerprint(1))
	if err != nil || repeat {
		t.Fatalf("Seen(repeat) = %v, %v, want false, nil", repeat, err)
	}
	if d.Duplicates() != 1 {
		t.Fatalf("Duplicates() = %d, want 1", d.Duplicates())
	}

	if _, err := d.Seen(fingerprint(2)); err != nil {
		t.Fatalf("Seen(new): %v", err)
	}
	if d.Duplicates() != 1 {
		t.Fatalf("Duplicates() after a new fingerprint = %d, want 1 (unchanged)", d.Duplicates())
	}

	if _, err := d.Seen("not-hex"); !errors.Is(err, ErrMalformedFingerprint) {
		t.Fatalf("Seen(malformed) err = %v, want ErrMalformedFingerprint", err)
	}
	if d.Duplicates() != 1 {
		t.Fatalf("Duplicates() after a malformed line = %d, want 1 (unchanged)", d.Duplicates())
	}
}

// seenSlice is the fingerprint filter as it is often first written: a
// []string with a linear "have we seen this" scan, instead of the
// map[string]struct{} idiom. It is never exported and exists only so the
// property test below can show what its linear scan costs as the input
// grows, in contrast to Dedup.Seen, which performs a single map lookup no
// matter how many fingerprints have been recorded before it.
type seenSlice struct {
	items []string
}

// contains reports whether fp is present and how many element comparisons
// the linear scan needed to decide -- the cost seenSlice pays on every call.
func (s *seenSlice) contains(fp string) (found bool, comparisons int) {
	for _, item := range s.items {
		comparisons++
		if item == fp {
			return true, comparisons
		}
	}
	return false, comparisons
}

// TestSeenSliceComparisonsGrowWithInput is the property this module exists
// to demonstrate: seenSlice's per-call cost is not constant. Checking the
// 500th distinct fingerprint against a slice already holding 499 others
// requires scanning (up to) all of them -- the comparison count is a
// property of how much of the stream came before, not of the fingerprint
// itself. A map-backed Seen call has no such scan: the language guarantees
// map access does not degrade as the map grows, which is exactly why
// Dedup uses one.
func TestSeenSliceComparisonsGrowWithInput(t *testing.T) {
	t.Parallel()

	var s seenSlice
	var lastComparisons int
	const n = 500
	for i := range n {
		fp := fingerprint(i) // every fingerprint is distinct: always a miss
		_, comparisons := s.contains(fp)
		s.items = append(s.items, fp)
		lastComparisons = comparisons
	}
	if lastComparisons < n-1 {
		t.Fatalf("seenSlice scanned %d elements checking the %dth distinct fingerprint, want it to have scanned all %d prior entries", lastComparisons, n, n-1)
	}

	d := New()
	for i := range n {
		if _, err := d.Seen(fingerprint(i)); err != nil {
			t.Fatalf("Seen: %v", err)
		}
	}
	if d.Duplicates() != 0 {
		t.Fatalf("Duplicates() = %d, want 0: every fingerprint in this run was distinct", d.Duplicates())
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		args       []string
		input      string
		wantStdout string
		wantStderr string
		wantErr    bool
	}{
		{
			name:       "duplicates filtered, first sighting streamed",
			input:      fingerprint(1) + "\n" + fingerprint(2) + "\n" + fingerprint(1) + "\n",
			wantStdout: fingerprint(1) + "\n" + fingerprint(2) + "\n",
			wantStderr: "duplicates: 1\n",
		},
		{
			name:       "no duplicates",
			input:      fingerprint(1) + "\n",
			wantStdout: fingerprint(1) + "\n",
			wantStderr: "duplicates: 0\n",
		},
		{
			name:       "empty input",
			input:      "",
			wantStdout: "",
			wantStderr: "duplicates: 0\n",
		},
		{
			name:    "malformed fingerprint is a usage error",
			input:   "not-hex\n",
			wantErr: true,
		},
		{
			name:    "unexpected argument is a usage error",
			args:    []string{"extra"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.input), &stdout, &stderr)
			if tc.wantErr {
				if !errors.Is(err, errUsage) {
					t.Fatalf("run() err = %v, want it to wrap errUsage", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("run(): %v", err)
			}
			if stdout.String() != tc.wantStdout {
				t.Fatalf("run() stdout = %q, want %q", stdout.String(), tc.wantStdout)
			}
			if stderr.String() != tc.wantStderr {
				t.Fatalf("run() stderr = %q, want %q", stderr.String(), tc.wantStderr)
			}
		})
	}
}
```

## Review

The filter is correct when every first occurrence of a fingerprint reaches
stdout exactly once, in arrival order, and every later repeat is counted
but not reprinted — which `Dedup.Seen`'s comma-ok check against a
`map[string]struct{}` gives directly, with no scan of anything. The
property this module isolates is that a `[]string` plus a linear `contains`
would produce byte-identical output while quietly costing more on every
line than the one before it; `TestSeenSliceComparisonsGrowWithInput` states
that as a comparison count rather than a timing, which is what keeps the
test deterministic and portable across machines. `run` streams rather than
buffers, mapping a malformed fingerprint or a stray argument to exit code 2
and reserving exit code 1 for a stdin read failure. Run
`go test -count=1 -race ./...` to confirm the validation table, the
`Dedup` table, the `seenSlice` property test, and `run` end to end.

## Resources

- [Go: the empty struct](https://dave.cheney.net/2014/03/25/the-empty-struct) — why `struct{}` is the zero-width set value used by `Dedup`.
- [Certificate Transparency (RFC 6962)](https://www.rfc-editor.org/rfc/rfc6962) — the log-monitoring use case this module models.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — the line-oriented, non-buffering reader `run` uses to stream the input.
- [`encoding/hex`](https://pkg.go.dev/encoding/hex#DecodeString) — used by `ValidateFingerprint` to reject a non-hex line.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-protected-key-config-overlay.md](16-protected-key-config-overlay.md) | Next: [18-feature-flag-changeset-clone-and-swap.md](18-feature-flag-changeset-clone-and-swap.md)

# Exercise 17: Verify SSH Host-Key Pins With slices.Contains

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

GitHub and GitLab do not publish one SSH host-key fingerprint, they publish
several at once, precisely so a rotation can roll out gradually: for a window
of days or weeks, either the old key or the new one is a legitimate answer
from the real host. Any client, proxy, or bastion that pins host keys instead
of trusting them blindly needs a membership check against that small, bounded
set of acceptable fingerprints -- not an equality check against a single
expected value. `slices.Contains` is exactly this operation: a linear scan
over a small comparable-element slice, which is the right tool precisely
because the set is small and unordered, so there is nothing for a binary
search or a map to buy you.

The trap is not in reaching for `Contains` -- it is in what people write
before they know it exists. A membership loop and an equality loop look
identical at the call site, and it is easy to write the second while meaning
the first: a loop that returns as soon as it finds *any* pin the presented key
does not match, instead of returning only when *none* of them match. That bug
is invisible with one pin, because "equals every pin" and "equals some pin"
are the same statement when there is only one pin to check against. It only
surfaces the day the rotation window opens and a second, equally valid key
appears -- exactly the day the pinning check exists to get right.

This module builds `Pinset`, a small immutable set of trusted SHA-256
host-key fingerprints, and `pinverify`, the tool that classifies connection
log lines against it using `slices.Contains` over a comparable `[32]byte`
array type.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
pinverify/                    module example.com/pinverify
  go.mod                      go 1.24
  pinverify.go                package main — Fingerprint [32]byte; ParseFingerprint; Pinset, NewPinset, Trusted, Fingerprints
  pinverify_test.go           package main — pinset table, trust/isolation, the naive-loop contrast, run() end to end
  main.go                     package main — -pins flag, stdin classification, exit codes
```

- Files: `pinverify.go`, `pinverify_test.go`, `main.go`.
- Implement: `ParseFingerprint(s string) (Fingerprint, error)` decoding 64 hex characters into a `[32]byte`, `ErrInvalidFingerprint` otherwise; `NewPinset(lines []string) (*Pinset, error)` parsing one fingerprint per line, skipping blanks and `#` comments, collapsing duplicates, returning `ErrInvalidFingerprint` (wrapped with the line number) or `ErrEmptyPinset`; `(*Pinset).Trusted(fp Fingerprint) bool` via `slices.Contains`; `(*Pinset).Fingerprints() []Fingerprint` returning a defensive copy.
- Tool: `pinverify -pins FILE` reads `hostname fingerprint` lines from stdin and writes `OK hostname` or `UNTRUSTED hostname` per line to stdout. A malformed pin file is a usage error (exit 2); a malformed stdin line is reported to stderr and the run continues, but the process exits 1 once stdin is exhausted, so one garbled line in a long connection log cannot silently swallow the rest.
- Test: pinset construction (single pin, comment/blank handling, duplicate collapse, empty, nil, malformed fingerprint); trust checks for a pinned and an unpinned fingerprint plus the `Fingerprints` aliasing contract; a `trustedNaive` contrast pinning the exact rotation-window failure; `run` end to end over `strings.Reader` and `bytes.Buffer` for mixed trust, a malformed line, a missing flag, and an unreadable pin file.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Membership over a small set is a linear scan, and the loop is easy to get backwards

A `Fingerprint` here is `[32]byte`, not `[]byte`. That choice is what makes
`slices.Contains` applicable at all: `Contains` requires a `comparable`
element type, and a fixed-size array of bytes is comparable with `==` while a
slice is not. Converting the decoded digest into an array up front means every
later comparison -- in `Trusted`, in a test, anywhere -- is a plain value
comparison, with no `bytes.Equal` helper and no risk of comparing two slices
that happen to share a backing array.

`slices.Contains(fingerprints, fp)` walks the slice and reports whether any
element equals `fp`. That is deliberately a full scan with no early
optimization: a pinset holds a handful of rotation candidates, so there is no
ordering to exploit with `slices.BinarySearch` and no volume that would justify
building a `map[Fingerprint]struct{}` first. The scan is also where the bug
this module is about hides. Someone asked to "check whether fp is in pins"
reaches for a familiar template -- iterate, and return early on the first
element that fails a check -- and applies it here without noticing the check
has flipped from "must equal" to "must equal at least one of":

```go
func trustedNaive(fp Fingerprint, pins []Fingerprint) bool {
    for _, p := range pins {
        if p != fp {
            return false   // bails out on the FIRST mismatch
        }
    }
    return true
}
```

`trustedNaive` actually answers "does `fp` equal *every* pin", not "does `fp`
equal *some* pin". With one pin in the set the two questions have the same
answer, so the function passes every test written against a single-key
deployment. The moment a rotation adds a second pin, `trustedNaive` rejects a
perfectly valid presented key as soon as it is compared against the *other*
pin in the set -- a false rejection, not a false acceptance, but still an
outage: legitimate connections start failing host-key verification during the
exact window the second pin exists to keep them working. `Pinset.Trusted`
never has this shape because `slices.Contains` encodes "equal to at least one"
directly; there is no loop for the AND/OR swap to hide in.

Create `pinverify.go`:

```go
// Package main implements pinverify, a linear membership check against a
// small set of pinned SSH host-key fingerprints -- the way GitHub and
// GitLab publish several valid SHA-256 host-key fingerprints at once during
// a rotation window, so a client pinned to any one of them keeps
// connecting while the fleet migrates to the new key.
package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// FingerprintSize is the length in bytes of a SHA-256 host-key fingerprint.
const FingerprintSize = 32

// Fingerprint is a SHA-256 host-key fingerprint. It is a comparable array,
// not a slice, so it can be compared with == and used directly with
// slices.Contains: no byte-slice comparison helper is needed anywhere in
// this package.
type Fingerprint [FingerprintSize]byte

// Sentinel errors returned while building or consulting a Pinset. Callers
// should test for them with errors.Is.
var (
	// ErrInvalidFingerprint means a line was not exactly 64 hex characters.
	ErrInvalidFingerprint = errors.New("pinverify: fingerprint must be 64 hex characters")
	// ErrEmptyPinset means the pin file had no fingerprint lines left after
	// stripping comments and blank lines.
	ErrEmptyPinset = errors.New("pinverify: pinset must not be empty")
)

// ParseFingerprint decodes a 64-character hex string into a Fingerprint. It
// returns ErrInvalidFingerprint if s is the wrong length or is not valid
// hex.
func ParseFingerprint(s string) (Fingerprint, error) {
	var fp Fingerprint
	if len(s) != FingerprintSize*2 {
		return fp, fmt.Errorf("%w: got %d characters", ErrInvalidFingerprint, len(s))
	}
	decoded, err := hex.DecodeString(s)
	if err != nil {
		return fp, fmt.Errorf("%w: %v", ErrInvalidFingerprint, err)
	}
	copy(fp[:], decoded)
	return fp, nil
}

// Pinset is a small set of trusted host-key fingerprints, such as the
// handful a provider publishes during a key-rotation window.
//
// A Pinset is immutable after construction and is safe for concurrent use
// by multiple goroutines: Trusted and Fingerprints only read the
// underlying slice, which NewPinset never exposes for mutation.
type Pinset struct {
	fingerprints []Fingerprint
}

// NewPinset parses one fingerprint per line from lines, skipping blank
// lines and lines starting with '#'. Duplicate fingerprints are collapsed.
// It returns ErrInvalidFingerprint wrapped with the offending line number,
// or ErrEmptyPinset if no fingerprint lines remain.
func NewPinset(lines []string) (*Pinset, error) {
	var fps []Fingerprint
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fp, err := ParseFingerprint(line)
		if err != nil {
			return nil, fmt.Errorf("pinverify: line %d: %w", i+1, err)
		}
		if !slices.Contains(fps, fp) {
			fps = append(fps, fp)
		}
	}
	if len(fps) == 0 {
		return nil, ErrEmptyPinset
	}
	return &Pinset{fingerprints: fps}, nil
}

// Trusted reports whether fp is one of the pinned fingerprints.
//
// It is a linear scan on purpose: pinsets are small by construction (a
// handful of rotation candidates), so slices.Contains is the right tool.
// There is no ordering to exploit with a binary search, and building a map
// for a set this size would cost more to construct than the scan ever
// costs to run.
func (p *Pinset) Trusted(fp Fingerprint) bool {
	return slices.Contains(p.fingerprints, fp)
}

// Fingerprints returns the pinned fingerprints. The returned slice is a
// copy and does not alias the Pinset's internal storage; the caller may
// mutate or retain it freely.
func (p *Pinset) Fingerprints() []Fingerprint {
	return slices.Clone(p.fingerprints)
}
```

### The tool

`pinverify` separates two failure classes on purpose. Everything about *how*
the tool is configured -- a missing `-pins` flag, a pin file that cannot be
opened, a pin file with a malformed fingerprint -- is a usage error the
operator fixes by changing the command line or the file, and `run` returns
those wrapped in `errUsage` before it ever reads a byte of stdin. Everything
about the *data stream* it is asked to classify is different: a single
garbled line in a long connection log is routine, not fatal, so `run` reports
it to stderr with its line number and keeps classifying the rest. Only after
stdin is exhausted does it report that some lines failed, wrapped in
`errDataError`, distinct from `errUsage` so `main` can map the two to
different exit codes. `run` takes the argument slice plus separate stdout and
stderr writers, so a test can assert on each independently without touching
`os.Args`, `os.Stdin`, or `os.Exit`.

Create `main.go`:

```go
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// errUsage marks a failure the caller fixes by changing the command line or
// the pin file: a missing -pins flag, an unreadable pin file, or a pin file
// that fails to parse. main maps it to exit code 2.
var errUsage = errors.New("usage")

// errDataError marks one or more malformed lines encountered on stdin while
// otherwise running correctly. main maps it to exit code 1: the flags and
// the pinset were fine, the input stream was not.
var errDataError = errors.New("data errors")

// run reads a pinset from the file named by -pins, then classifies each
// "hostname fingerprint" line read from stdin as OK or UNTRUSTED, writing
// one result line per input line to stdout. Malformed input lines are
// reported to stderr and skipped rather than aborting the run, so one bad
// line in a long connection log does not hide the verdict on every other
// line.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("pinverify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	pinsPath := fs.String("pins", "", "path to a file of pinned SHA-256 fingerprints, one per line (required)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if *pinsPath == "" {
		return fmt.Errorf("%w: -pins is required", errUsage)
	}

	data, err := os.ReadFile(*pinsPath)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	pinset, err := NewPinset(strings.Split(string(data), "\n"))
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	var badLines int
	scanner := bufio.NewScanner(stdin)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			fmt.Fprintf(stderr, "line %d: want \"hostname fingerprint\", got %q\n", lineNum, line)
			badLines++
			continue
		}
		host, hex := fields[0], fields[1]
		fp, err := ParseFingerprint(hex)
		if err != nil {
			fmt.Fprintf(stderr, "line %d: %v\n", lineNum, err)
			badLines++
			continue
		}
		if pinset.Trusted(fp) {
			fmt.Fprintf(stdout, "OK %s\n", host)
		} else {
			fmt.Fprintf(stdout, "UNTRUSTED %s\n", host)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if badLines > 0 {
		return fmt.Errorf("%w: %d line(s) failed to parse", errDataError, badLines)
	}
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: pinverify -pins FILE < connections.log")
		fmt.Fprintln(os.Stderr, "checks each \"hostname fingerprint\" line on stdin against the pinset.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "pinverify:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n' > pins.txt
printf 'github.com aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nold-mirror ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff\ngitlab.com bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n' | go run . -pins pins.txt
printf 'host1 short\n' | go run . -pins pins.txt
printf '' | go run . -pins missing.txt
```

Expected output:

```text
OK github.com
UNTRUSTED old-mirror
OK gitlab.com
```

```text
line 1: pinverify: fingerprint must be 64 hex characters: got 5 characters
pinverify: data errors: 1 line(s) failed to parse
```

```text
pinverify: usage: open missing.txt: no such file or directory
```

The first run shows the two-pin rotation window working as intended:
`github.com` presents the old key, `gitlab.com` presents the new one, both
come back `OK` because `Trusted` checks membership against the whole set, not
equality against one expected value; `old-mirror` presents a key that is in
neither pin and is correctly flagged `UNTRUSTED`. The second run shows the
usage/data-error split in practice: the malformed line is named by number on
stderr and the process still exits nonzero, but nothing about the pinset or
the flags was wrong. The third shows a configuration problem -- the pin file
itself does not exist -- reported and mapped to exit code 2, before any stdin
line is ever read.

### Tests

`TestNewPinset` is the construction table: a single pin, comments and blank
lines interleaved with real ones, duplicate collapse, an empty result from an
all-comment file, a nil input, and a fingerprint that is the wrong length.
`TestPinsetTrustedAndFingerprintsIsolation` checks that both pinned
fingerprints are trusted and an unrelated one is not, then mutates the slice
`Fingerprints` returns and confirms the `Pinset`'s own membership answer does
not change -- pinning the aliasing contract on the doc comment.
`TestTrustedNaiveBreaksDuringRotation` is the module's center of gravity: it
shows `trustedNaive` correctly accepting the only pin in a single-key set,
then incorrectly rejecting a legitimate key the moment a second pin exists,
with `Pinset.Trusted` accepting the same key in the same two-pin set right
next to it. `TestRun` drives the command end to end over `strings.Reader` and
`bytes.Buffer`: mixed trust results with blank lines skipped, a malformed
stdin line that is reported but does not stop the run, a missing `-pins`
flag, and an unreadable pin file, each checked against the right sentinel and
the right exit-code family.

Create `pinverify_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fpA, fpB, fpC stand in for real SHA-256 sums: repeated hex digits.
var (
	fpA = strings.Repeat("a", 64)
	fpB = strings.Repeat("b", 64)
	fpC = strings.Repeat("c", 64)
)

// trustedNaive bails out on the first pin that does not match, so it
// really checks "fp equals every pin" instead of "fp equals some pin".
// Never exported or reachable from Pinset; it exists so the tests below
// can pin what it gets wrong.
func trustedNaive(fp Fingerprint, pins []Fingerprint) bool {
	for _, p := range pins {
		if p != fp {
			return false
		}
	}
	return true
}

func mustParse(t *testing.T, s string) Fingerprint {
	t.Helper()
	fp, err := ParseFingerprint(s)
	if err != nil {
		t.Fatalf("ParseFingerprint(%q): %v", s, err)
	}
	return fp
}

func TestNewPinset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		lines   []string
		wantLen int
		wantErr error
	}{
		{name: "single pin", lines: []string{fpA}, wantLen: 1},
		{name: "two pins with comment and blank", lines: []string{"# rotation window", fpA, "", fpB}, wantLen: 2},
		{name: "duplicate pins collapse", lines: []string{fpA, fpA, fpB}, wantLen: 2},
		{name: "only comments is empty", lines: []string{"# nothing here", ""}, wantErr: ErrEmptyPinset},
		{name: "nil lines is empty", lines: nil, wantErr: ErrEmptyPinset},
		{name: "short fingerprint", lines: []string{"deadbeef"}, wantErr: ErrInvalidFingerprint},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p, err := NewPinset(tc.lines)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("NewPinset error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewPinset: unexpected error: %v", err)
			}
			if got := len(p.Fingerprints()); got != tc.wantLen {
				t.Fatalf("len(Fingerprints()) = %d, want %d", got, tc.wantLen)
			}
		})
	}
}

// TestPinsetTrustedAndFingerprintsIsolation checks membership and confirms
// Fingerprints returns a copy: mutating it must not affect Trusted.
func TestPinsetTrustedAndFingerprintsIsolation(t *testing.T) {
	t.Parallel()
	p, err := NewPinset([]string{fpA, fpB})
	if err != nil {
		t.Fatalf("NewPinset: %v", err)
	}
	if !p.Trusted(mustParse(t, fpA)) || !p.Trusted(mustParse(t, fpB)) || p.Trusted(mustParse(t, fpC)) {
		t.Fatal("Trusted gave a wrong verdict for a pinned or unpinned fingerprint")
	}
	p.Fingerprints()[0] = mustParse(t, fpC) // mutate the copy, not the Pinset
	if !p.Trusted(mustParse(t, fpA)) {
		t.Fatal("mutating the returned slice corrupted the Pinset's own pin")
	}
}

// TestTrustedNaiveBreaksDuringRotation is the heart of the module:
// trustedNaive works with one pin and breaks the instant a rotation
// window introduces a second -- exactly when pinning exists to matter.
func TestTrustedNaiveBreaksDuringRotation(t *testing.T) {
	t.Parallel()
	if !trustedNaive(mustParse(t, fpA), []Fingerprint{mustParse(t, fpA)}) {
		t.Fatal("trustedNaive should accept the only pin in a single-pin set")
	}
	rotating := []Fingerprint{mustParse(t, fpA), mustParse(t, fpB)}
	target := mustParse(t, fpB)
	if trustedNaive(target, rotating) {
		t.Fatal("trustedNaive unexpectedly accepted fpB; the bug should have rejected it")
	}
	p, err := NewPinset([]string{fpA, fpB})
	if err != nil {
		t.Fatalf("NewPinset: %v", err)
	}
	if !p.Trusted(target) {
		t.Fatal("Pinset.Trusted must accept fpB during a two-pin rotation window")
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	pinsPath := filepath.Join(t.TempDir(), "pins.txt")
	if err := os.WriteFile(pinsPath, []byte(fpA+"\n"+fpB), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	tests := []struct {
		name       string
		args       []string
		stdin      string
		wantStdout string
		wantStderr string // substring; empty means "don't check"
		wantErr    error
	}{
		{
			name:       "mixed trust, blank lines skipped",
			args:       []string{"-pins", pinsPath},
			stdin:      "\nhost1 " + fpA + "\nhost2 " + fpC + "\n\nhost3 " + fpB + "\n",
			wantStdout: "OK host1\nUNTRUSTED host2\nOK host3\n",
		},
		{
			name:       "malformed line is reported and the rest still run",
			args:       []string{"-pins", pinsPath},
			stdin:      "bad-line\nhost1 " + fpA + "\n",
			wantStdout: "OK host1\n",
			wantStderr: "line 1:",
			wantErr:    errDataError,
		},
		{
			name:    "missing pins flag is a usage error",
			args:    []string{},
			stdin:   "",
			wantErr: errUsage,
		},
		{
			name:    "unreadable pins file is a usage error",
			args:    []string{"-pins", "/nonexistent/pins.txt"},
			stdin:   "",
			wantErr: errUsage,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.stdin), &stdout, &stderr)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("run error = %v, want %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("run: unexpected error: %v", err)
			}
			if stdout.String() != tc.wantStdout {
				t.Fatalf("stdout = %q, want %q", stdout.String(), tc.wantStdout)
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), tc.wantStderr)
			}
		})
	}
}
```

## Review

`Pinset.Trusted` is correct because `slices.Contains` states "equal to at
least one element" directly, with no loop for a search-versus-validate
mix-up to hide in. The mistake the naive loop makes is subtle exactly because
it is syntactically identical to a validation loop: return `false` at the
first mismatch is right when the goal is "every element must match" and wrong
when the goal is "some element must match", and a single-pin deployment
cannot tell the two apart. `NewPinset` rejects a malformed fingerprint with
`ErrInvalidFingerprint` (wrapped with the failing line number) and an
all-comment or empty file with `ErrEmptyPinset`, both checkable with
`errors.Is`; `Fingerprints` returns a copy so a caller can never corrupt the
set's own pins by mutating what it got back. The tool keeps that same
distinction at the process boundary: a bad flag or an unreadable or malformed
pin file is a usage error, exit 2, discovered before any stdin line is read;
a malformed data line is reported and skipped in place, and only changes the
final exit code to 1 once the whole stream has been classified. Run
`go test -count=1 -race ./...` to confirm the pinset table, the trust and
aliasing checks, the naive-loop contrast, and `run`'s end-to-end behavior.

## Resources

- [`slices.Contains`](https://pkg.go.dev/slices#Contains) — the membership check this module builds around, and why it requires `comparable`.
- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — used by `Fingerprints` to hand back a copy that cannot alias the `Pinset`'s storage.
- [Go Specification: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — why `[32]byte` is comparable with `==` and `[]byte` is not.
- [GitHub: About SSH host key fingerprints](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/githubs-ssh-key-fingerprints) — the real multi-key rotation window this module models.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-kafka-segment-prefix-compaction-delete.md](16-kafka-segment-prefix-compaction-delete.md) | Next: [18-prometheus-bucket-sorted-dedup.md](18-prometheus-bucket-sorted-dedup.md)

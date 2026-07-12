# Exercise 20: Deterministic Config Fingerprint for HTTP ETag Generation

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A config-serving endpoint -- the kind that hands out feature flags or
per-tenant settings to every service that starts up -- must give clients an
`ETag` that changes if and only if the underlying config actually changed.
That is the entire caching contract: a client that already has the current
config sends `If-None-Match` and gets a cheap `304`, and it only re-fetches
the body when the content is genuinely different. The moment the ETag can
change for config that has not changed, the contract is broken in the
expensive direction -- every client re-fetches on every poll, cache hit
rates collapse, and the "cheap 304" path the whole design exists for stops
firing.

The natural way to build that ETag is to hash the config: read the
key/value pairs, feed them into `sha256.New()`, hex-encode the sum. The trap
is doing that by ranging the config map directly into the hasher. A `map`'s
`range` order is deliberately randomized per call by the Go runtime -- the
same fact the rest of this lesson spends most of its time on -- so hashing
`for k, v := range cfg` writes the same bytes in a different sequence on
different calls, and SHA-256, like every general-purpose hash, treats
`"a=1;b=2;"` and `"b=2;a=1;"` as two completely unrelated inputs. Two
processes serving byte-identical config can compute two different ETags for
it, and the same process can compute a different ETag for the same config
on its next request. The fix already lives in this lesson's core idiom:
collect the keys, sort them, then iterate the sorted sequence -- except here
the payoff is not a stable log line, it is a caching contract that clients
can actually rely on.

This module builds `fingerprint`, a small command that reads a `key=value`
config from stdin and prints its digest: `Fingerprint` sorts before it
hashes, so the digest is a pure function of the config's content, never of
the order anything happened to be read or stored in. The naive
range-and-hash version never appears in the tool's logic; it lives in the
test file, isolated, as the fingerprint the tests prove is order-dependent.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
fingerprint/                  module example.com/fingerprint
  go.mod                      go 1.24
  fingerprint.go               package main — ParseConfig, Fingerprint; ErrMalformedLine
  fingerprint_test.go          package main — parsing table, malformed-line table,
                               order-invariance, order-dependence contrast, run() end to end
  main.go                      package main — no flags beyond usage, exit codes
```

- Files: `fingerprint.go`, `fingerprint_test.go`, `main.go`.
- Implement: `ParseConfig(r io.Reader) (map[string]string, error)` reading `key=value` lines, skipping blanks and `#` comments, with the last occurrence of a duplicate key winning, returning `ErrMalformedLine` for a line with no `=`; `Fingerprint(cfg map[string]string) string` returning the hex-encoded SHA-256 digest of the sorted key/value sequence.
- Tool: `fingerprint` reads a config from stdin and prints its hex-encoded fingerprint to stdout. Exit 0 on success, exit 2 for a malformed input line, an unknown flag, or an unexpected argument, exit 1 for any other failure reading stdin.
- Test: the parsing table (blanks, comments, duplicate keys, a value containing `=`); the malformed-line table (missing `=`, empty key); read-error propagation; `Fingerprint`'s order-invariance across two maps built in opposite insertion order; a `fingerprintOrdered` contrast proving the naive range-and-hash approach produces two different digests for two orderings of identical content; and `run` end to end over `strings.Reader` + `bytes.Buffer`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/08-map-internals-and-iteration-order/20-config-fingerprint-etag
cd go-solutions/06-collections-arrays-slices-and-maps/08-map-internals-and-iteration-order/20-config-fingerprint-etag
go mod edit -go=1.24
```

### Hashing a map's range order is hashing something that isn't the config

Here is the fingerprint almost everyone writes on the first pass, because it
reads like the most direct translation of "hash the config":

```go
// BUGGY: order of the writes depends on this call's randomized range start.
h := sha256.New()
for k, v := range cfg {
    fmt.Fprintf(h, "%s=%s;", k, v)
}
return hex.EncodeToString(h.Sum(nil))
```

Every individual write is correct -- `k` and `v` really are a key and value
from `cfg`. What is not fixed is the *sequence* those writes happen in, and
SHA-256 (like any hash function built to detect the smallest change in its
input) has no notion of "these bytes represent an unordered collection".
`fmt.Fprintf(h, "a=1;b=2;")` and `fmt.Fprintf(h, "b=2;a=1;")` produce
different digests, full stop, and the language spec's own guarantee is that
`range` over a map does not commit to any particular order between calls.
There is no scenario where this hash function is "usually right" -- it
simply encodes the runtime's current mood alongside the config content, and
an ETag built on it inherits that mood.

The general rule this lesson keeps returning to applies here with a
sharper stake than a flaky golden test: **the moment output must be
deterministic, collect the keys, sort them, then iterate the sorted
sequence.** `slices.Sorted(maps.Keys(cfg))` gives exactly that, and hashing
in that fixed order turns the digest into a pure function of `cfg`'s
content -- unaffected by insertion order, by which goroutine built the map,
or by which of Go's possible range starts the runtime happened to pick this
time.

Create `fingerprint.go`:

```go
// Package main implements fingerprint, a tool that turns a key=value config
// into a stable hex-encoded SHA-256 digest: the same digest for the same
// content every time, regardless of what order the keys were read or
// inserted in.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// ErrMalformedLine means a non-blank, non-comment line of config input had
// no '=' separating a key from a value.
var ErrMalformedLine = errors.New("fingerprint: malformed line: missing '='")

// ParseConfig reads key=value pairs, one per line, from r. Blank lines and
// lines beginning with '#' are skipped. The key is everything before the
// first '=' on the line; the value is everything after it, so a value may
// itself contain '='. Both are trimmed of surrounding whitespace. A
// duplicate key is not an error: the last occurrence in the input wins,
// matching how a layered config file is normally read. ParseConfig returns
// ErrMalformedLine, wrapped with the offending line number and text, for a
// line with no '='; it returns a plain wrapped error if r itself fails.
func ParseConfig(r io.Reader) (map[string]string, error) {
	cfg := make(map[string]string)
	sc := bufio.NewScanner(r)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%w: line %d: %q", ErrMalformedLine, lineNo, line)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("%w: line %d: empty key", ErrMalformedLine, lineNo)
		}
		cfg[key] = strings.TrimSpace(value)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("fingerprint: reading input: %w", err)
	}
	return cfg, nil
}

// Fingerprint returns the hex-encoded SHA-256 digest of cfg's content. It
// collects cfg's keys, sorts them, and hashes the key=value pairs in that
// sorted order, so the digest is a pure function of cfg's content: it never
// depends on cfg's internal layout or on the randomized order a range over
// cfg would produce on any given call. Two maps with identical key/value
// pairs, however they were built, always produce the same digest.
func Fingerprint(cfg map[string]string) string {
	h := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(cfg)) {
		fmt.Fprintf(h, "%s=%s\n", k, cfg[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}
```

### The tool

`fingerprint` takes no required flags -- its only input is stdin -- but
`run` still parses `args` through an empty `flag.FlagSet` so an unrecognized
flag or a stray positional argument is caught as a usage error rather than
silently ignored. Every failure `run` can produce that the caller fixes by
changing the input -- a bad flag, an unexpected argument, a malformed config
line -- wraps the `errUsage` sentinel, and `main` maps that to exit code 2;
any other error (a genuine I/O failure reading stdin) falls through to exit
code 1. `run` never touches `os.Stdin`, `os.Stdout`, or `os.Exit` directly,
so it is driven end to end in tests with a `strings.Reader` and a
`bytes.Buffer`.

Create `main.go`:

```go
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// errUsage marks a failure the caller can fix by changing the input: an
// unknown flag, an unexpected argument, or a malformed config line. main
// maps it to exit code 2; every other error maps to exit code 1.
var errUsage = errors.New("usage")

// run reads a key=value config from stdin and writes its hex-encoded
// SHA-256 fingerprint, followed by a newline, to stdout. It never touches
// os.Stdin, os.Stdout, or os.Exit, so it can be exercised in a test with a
// strings.Reader and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("fingerprint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q", errUsage, fs.Arg(0))
	}

	cfg, err := ParseConfig(stdin)
	if err != nil {
		if errors.Is(err, ErrMalformedLine) {
			return fmt.Errorf("%w: %v", errUsage, err)
		}
		return err
	}

	fmt.Fprintln(stdout, Fingerprint(cfg))
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: fingerprint")
		fmt.Fprintln(os.Stderr, "reads key=value lines from stdin and prints the hex sha256 fingerprint of their sorted content.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "fingerprint:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'route=cache\nstatus=200\n' | go run .
printf 'status=200\nroute=cache\n' | go run .
printf 'route=db\nstatus=200\n' | go run .
printf 'not-a-pair\n' | go run .
```

Expected output:

```text
39b7cef9ae5d16805712c0dd55a2b95e45ac45df0050e6ec735db9b49323de34
39b7cef9ae5d16805712c0dd55a2b95e45ac45df0050e6ec735db9b49323de34
9835ecccecdf9f878b4b992d4c441f24710bdd3cdcd53e49074a6824fd2d6aa0
fingerprint: usage: fingerprint: malformed line: missing '=': line 1: "not-a-pair"
```

The first two lines feed the identical two key/value pairs in opposite line
order and get the identical digest back -- that is the whole point, and it
is true regardless of which order the file happened to list them in, because
`Fingerprint` sorts before it hashes rather than trusting the order it was
handed. The third line changes one value and gets a different digest, which
is the other half of the contract: content that really changed must produce
a different ETag. The fourth line has no `=` at all, so `ParseConfig` fails
with `ErrMalformedLine`, `run` wraps it under `errUsage`, and the process
exits 2.

### Tests

`TestParseConfigSkipsBlankAndCommentLines` and
`TestParseConfigDuplicateKeyLastWins` pin the parsing rules, including the
edge case of a repeated key, which is a legitimate layered-config pattern
and not an error. `TestParseConfigValueMayContainEquals` covers a value
like a query string that itself has `=` in it. `TestParseConfigRejectsMalformedLines`
tables the two ways a line can fail to parse. `TestParseConfigPropagatesReadError`
confirms a genuine I/O failure is a plain wrapped error, distinct from
`ErrMalformedLine`, which is what lets `run` route it to a different exit
code.

`TestFingerprintIsOrderInvariant` builds two maps with the same content in
opposite insertion order and checks `Fingerprint` returns the same digest
for both -- the property the whole module exists to guarantee.
`TestFingerprintOrderedDependsOnOrderButFingerprintDoesNot` is the heart of
the module: `fingerprintOrdered` is unexported and unreachable from the
package API, and it takes an explicit `order` slice standing in for one
particular range order among the many the runtime's randomization could
produce on a given call -- that is what lets the test compare two orderings
deterministically instead of hoping real randomization cooperates on a given
run. Two different orderings of the same three key/value pairs produce two
different digests under `fingerprintOrdered`, exactly the defect that would
defeat an ETag. `TestRun` drives the whole tool end to end: a successful
fingerprint, a malformed line, an unknown flag, an unexpected argument, and
an empty config, which is valid input and still produces a (fixed) digest
rather than an error.

Create `fingerprint_test.go`:

```go
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestParseConfigSkipsBlankAndCommentLines(t *testing.T) {
	t.Parallel()

	src := "route=cache\n\n# a comment\nstatus = 200\n"
	cfg, err := ParseConfig(strings.NewReader(src))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	want := map[string]string{"route": "cache", "status": "200"}
	if len(cfg) != len(want) || cfg["route"] != "cache" || cfg["status"] != "200" {
		t.Fatalf("cfg = %v, want %v", cfg, want)
	}
}

func TestParseConfigDuplicateKeyLastWins(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfig(strings.NewReader("flag=off\nflag=on\n"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg["flag"] != "on" {
		t.Fatalf(`cfg["flag"] = %q, want "on" (last occurrence wins)`, cfg["flag"])
	}
}

func TestParseConfigValueMayContainEquals(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfig(strings.NewReader("filter=a=1&b=2\n"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg["filter"] != "a=1&b=2" {
		t.Fatalf("cfg[filter] = %q, want %q", cfg["filter"], "a=1&b=2")
	}
}

func TestParseConfigRejectsMalformedLines(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		src  string
	}{
		{"no equals sign", "route cache\n"},
		{"empty key", "=cache\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseConfig(strings.NewReader(tc.src)); !errors.Is(err, ErrMalformedLine) {
				t.Fatalf("ParseConfig(%q) error = %v, want ErrMalformedLine", tc.src, err)
			}
		})
	}
}

// errReader always fails, standing in for a broken stdin stream.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestParseConfigPropagatesReadError(t *testing.T) {
	t.Parallel()

	_, err := ParseConfig(errReader{})
	if err == nil || errors.Is(err, ErrMalformedLine) {
		t.Fatalf("ParseConfig error = %v, want a plain read error, not ErrMalformedLine", err)
	}
}

func TestFingerprintIsOrderInvariant(t *testing.T) {
	t.Parallel()

	// Two maps holding identical content, built by inserting the keys in
	// opposite order. Insertion order has no bearing on Fingerprint's
	// result because it sorts the keys itself before hashing.
	first := map[string]string{}
	for _, k := range []string{"a", "b", "c"} {
		first[k] = k + "-value"
	}
	second := map[string]string{}
	for _, k := range []string{"c", "b", "a"} {
		second[k] = k + "-value"
	}

	if Fingerprint(first) != Fingerprint(second) {
		t.Fatal("Fingerprint differs for identical content built in different insertion order")
	}
}

func TestFingerprintChangesWithContent(t *testing.T) {
	t.Parallel()

	a := Fingerprint(map[string]string{"route": "cache"})
	b := Fingerprint(map[string]string{"route": "db"})
	if a == b {
		t.Fatal("Fingerprint must differ when content differs")
	}
}

// fingerprintOrdered is the fingerprint almost everyone writes first: hash
// the key/value pairs in whatever order they are handed, standing in for
// `for k, v := range cfg` -- Go's runtime picks that order at random on
// every call. It is never exported and never reached from the package API;
// order is an explicit stand-in for one particular range order among the
// many the runtime could produce, which is what lets the test below compare
// two orderings deterministically instead of hoping randomization cooperates.
func fingerprintOrdered(cfg map[string]string, order []string) string {
	h := sha256.New()
	for _, k := range order {
		h.Write([]byte(k))
		h.Write([]byte{'='})
		h.Write([]byte(cfg[k]))
		h.Write([]byte{';'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestFingerprintOrderedDependsOnOrderButFingerprintDoesNot is the whole
// point of the module. The same config content, hashed in two different
// orders, produces two different digests under the naive approach -- an
// ETag computed this way would change even though nothing about the config
// changed, defeating the ETag's purpose. Fingerprint, hashing the sorted key
// sequence, is invariant to the order it is handed the same content in.
func TestFingerprintOrderedDependsOnOrderButFingerprintDoesNot(t *testing.T) {
	t.Parallel()

	cfg := map[string]string{"a": "1", "b": "2", "c": "3"}
	orderX := []string{"a", "b", "c"}
	orderY := []string{"c", "b", "a"}

	digestX := fingerprintOrdered(cfg, orderX)
	digestY := fingerprintOrdered(cfg, orderY)
	if digestX == digestY {
		t.Fatal("setup: two different orderings must produce different digests to demonstrate the defect")
	}

	if got := Fingerprint(cfg); got != Fingerprint(cfg) {
		t.Fatalf("Fingerprint is not even self-consistent: %q vs %q", got, Fingerprint(cfg))
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	t.Run("valid config prints a fingerprint", func(t *testing.T) {
		t.Parallel()
		var stdout, stderr bytes.Buffer
		err := run(nil, strings.NewReader("route=cache\nstatus=200\n"), &stdout, &stderr)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		want := Fingerprint(map[string]string{"route": "cache", "status": "200"}) + "\n"
		if stdout.String() != want {
			t.Fatalf("stdout = %q, want %q", stdout.String(), want)
		}
	})

	t.Run("malformed line is a usage error", func(t *testing.T) {
		t.Parallel()
		var stdout, stderr bytes.Buffer
		err := run(nil, strings.NewReader("not-a-pair\n"), &stdout, &stderr)
		if !errors.Is(err, errUsage) {
			t.Fatalf("run error = %v, want errUsage", err)
		}
	})

	t.Run("unknown flag is a usage error", func(t *testing.T) {
		t.Parallel()
		var stdout, stderr bytes.Buffer
		err := run([]string{"-bogus"}, strings.NewReader(""), &stdout, &stderr)
		if !errors.Is(err, errUsage) {
			t.Fatalf("run error = %v, want errUsage", err)
		}
	})

	t.Run("unexpected argument is a usage error", func(t *testing.T) {
		t.Parallel()
		var stdout, stderr bytes.Buffer
		err := run([]string{"extra"}, strings.NewReader(""), &stdout, &stderr)
		if !errors.Is(err, errUsage) {
			t.Fatalf("run error = %v, want errUsage", err)
		}
	})

	t.Run("empty config still fingerprints", func(t *testing.T) {
		t.Parallel()
		var stdout, stderr bytes.Buffer
		if err := run(nil, strings.NewReader(""), &stdout, &stderr); err != nil {
			t.Fatalf("run: %v", err)
		}
		want := Fingerprint(map[string]string{}) + "\n"
		if stdout.String() != want {
			t.Fatalf("stdout = %q, want %q", stdout.String(), want)
		}
	})
}
```

## Review

`fingerprint` is correct when the digest it prints depends only on the
config's key/value content, never on the order those pairs were read, typed,
or stored in -- and sorting the keys before hashing is what turns that from
a hope into a guarantee. The trap this module isolates is the same one the
rest of this lesson spends its time on, applied somewhere the consequence is
sharper than a flaky test: hashing a map's own `range` order bakes Go's
per-call randomization into a value that is supposed to be a pure function
of content, so an ETag built that way can flap even when nothing about the
underlying config changed, and two processes serving identical config can
disagree about it entirely. `ErrMalformedLine` catches the one way a config
line can fail to parse, checkable with `errors.Is` and mapped to exit code 2
alongside a bad flag or a stray argument; any other failure reading stdin
falls through to exit code 1. Run `go test -count=1 -race ./...` to confirm
the parsing table, the malformed-line cases, the order-invariance property,
the order-dependence contrast, and `run`'s end-to-end behavior.

## Resources

- [`maps.Keys`](https://pkg.go.dev/maps#Keys) and [`slices.Sorted`](https://pkg.go.dev/slices#Sorted) — the sort-before-hash idiom this module's correctness depends on.
- [`crypto/sha256`](https://pkg.go.dev/crypto/sha256) — the digest algorithm; any general-purpose hash has the same order-sensitivity.
- [RFC 7232 (HTTP Conditional Requests)](https://www.rfc-editor.org/rfc/rfc7232) — the `ETag` / `If-None-Match` contract this fingerprint feeds.
- [`strings.Cut`](https://pkg.go.dev/strings#Cut) — splitting each config line on its first `=` while allowing `=` in the value.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-composite-key-quota-tracker.md](19-composite-key-quota-tracker.md) | Next: [../09-slices-package/00-concepts.md](../09-slices-package/00-concepts.md)

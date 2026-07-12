# Exercise 16: Snapshot Import Tool With Map Size-Hint Preallocation

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A control-plane restore streams in a bulk snapshot and rebuilds an in-memory
table from it: an etcd bulk-load replaying a keyspace dump, a BGP speaker
loading a RIB checkpoint on restart, an xDS client applying a full-state
config snapshot from its management server. These formats routinely declare
the record count up front — a header line, a length-prefixed frame, a
`total_keys` field — before the records themselves. That declared count is
information the importer can use immediately: it knows the map's final size
before reading a single key, and it can build the map at that size instead of
growing it one insertion at a time.

Ignoring the count and calling `make(map[string]string)` still produces a
correct table. Every key ends up in the right place. What differs is how many
times the runtime has to grow the bucket array and rehash everything already
inserted while filling it — work that scales with the table size and that a
known count makes entirely avoidable. This is the map analogue of the classic
slice-preallocation mistake, and it hides the same way: the buggy and the
fixed version produce byte-identical output, so nothing in a functional test
catches it. Only an allocation count does.

This module builds `ribload`, a tool that reads a `COUNT <n>` header followed
by `n` `KEY VALUE` lines and imports them into a map sized once from that
header. Its test pins the allocation saving as a property — sized fewer than
unsized — never as an exact count, because the runtime's own growth curve is
not part of the contract.

This module is fully self-contained: its own `go mod init`, an executable
tool, and its tests. Nothing here imports another exercise.

## What you'll build

```text
ribload/                  module example.com/ribload
  go.mod                  go 1.24
  ribload.go              package main — LoadSnapshot, parseHeader; three sentinel errors
  ribload_test.go         package main — decode table, contrast, allocation property, run() end to end
  main.go                 package main — -strict/-format flags, exit codes
```

- Files: `ribload.go`, `ribload_test.go`, `main.go`.
- Implement: `LoadSnapshot(r io.Reader, strict bool) (map[string]string, error)` reading a `COUNT <n>` header then `n` `KEY VALUE` lines and building the result with `make(map[string]string, n)`; sentinel errors `ErrMalformedHeader`, `ErrMalformedRecord`, `ErrDuplicateKey`.
- Tool: `ribload` reads a snapshot from stdin or a file argument. `-strict` rejects a repeated key instead of letting it overwrite the earlier value; `-format=text|tsv` (default `text`) selects the summary line's shape. It prints the final unique record count to stdout. Exit 0 on success, 2 on a malformed header, a malformed record, an unknown `-format`, or a strict duplicate, 1 on an I/O failure opening or reading the file.
- Test: the decode table (ordinary table, zero records, non-strict overwrite, strict rejection, malformed header text and count, a short record, fewer records than declared, empty input); a `loadUnsized` contrast proving both builds decode identically; the allocation property `sized < unsized`; `run` end to end over `strings.Reader` and `bytes.Buffer`, including the file-argument path and a missing-file I/O error.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### A declared count is a size hint, not just a loop bound

`make(map[K]V, n)` takes a second argument most callers never pass: a hint
telling the runtime roughly how many entries the map will hold, so it can
size the initial bucket array for that load up front. It is a hint, not a
cap — the map still grows past it if more keys arrive than promised — but
when the hint is accurate it eliminates the incremental grow-and-rehash
churn that filling an unsized map pays as it crosses each load-factor
threshold. The naive importer treats the header purely as a loop bound and
throws the size information away the moment the loop starts:

```go
n, _ := parseHeader(header)     // n is right there
table := make(map[string]string) // and then ignored
for i := 0; i < n; i++ {
    table[key] = value           // every insertion risks a grow+rehash
}
```

The fix costs nothing beyond reading the number that is already parsed:
`make(map[string]string, n)`. Both versions decode to the same map — the
same keys, the same values, the same final length — so no functional
assertion tells them apart. What differs, and what this module's test
measures, is how many times the runtime allocates while filling the map:
strictly fewer with the hint than without it, never an exact count, since
the growth curve itself is a runtime implementation detail and has changed
across Go releases.

The rest of `LoadSnapshot` is what makes the importer safe to point at
untrusted input: a header that is not exactly `COUNT <n>` is rejected before
any allocation happens, a record line missing its space-separated value is
rejected with the line number that failed, and running out of input before
`n` records arrive is a malformed-input error rather than a silent short
read. `-strict` exists because two different sources landing the same key
twice is usually a sign the snapshot itself is corrupt, and a control plane
would rather fail loudly at import time than silently apply the second
value over the first.

Create `ribload.go`:

```go
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Sentinel errors returned by LoadSnapshot. Callers should test for them with
// errors.Is rather than by comparing error strings.
var (
	// ErrMalformedHeader means the input's first line was not "COUNT <n>"
	// with a non-negative integer n.
	ErrMalformedHeader = errors.New("ribload: malformed header")
	// ErrMalformedRecord means a record line was not "KEY VALUE", or fewer
	// records were present than the header declared.
	ErrMalformedRecord = errors.New("ribload: malformed record")
	// ErrDuplicateKey means -strict rejected a key already present in the
	// snapshot.
	ErrDuplicateKey = errors.New("ribload: duplicate key")
)

// LoadSnapshot reads a table snapshot from r: a header line "COUNT <n>"
// followed by exactly n "KEY VALUE" lines, and returns the decoded map.
//
// The map is preallocated with make(map[string]string, n): the header
// declares the exact final size before a single record is read, so the
// bucket array is sized once and filling it pays no grow/rehash churn. See
// ribload_test.go for the allocation-count property this buys over building
// with a zero size hint.
//
// If strict is true, a repeated key is reported as ErrDuplicateKey;
// otherwise a repeated key silently overwrites the earlier value, matching
// ordinary map assignment semantics.
func LoadSnapshot(r io.Reader, strict bool) (map[string]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("ribload: reading header: %w", err)
		}
		return nil, fmt.Errorf("%w: empty input", ErrMalformedHeader)
	}
	n, err := parseHeader(sc.Text())
	if err != nil {
		return nil, err
	}

	table := make(map[string]string, n) // exact hint from the declared count
	line := 1
	for i := 0; i < n; i++ {
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return nil, fmt.Errorf("ribload: reading record %d: %w", i+1, err)
			}
			return nil, fmt.Errorf("%w: expected %d records, got %d", ErrMalformedRecord, n, i)
		}
		line++
		key, value, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			return nil, fmt.Errorf("%w: line %d: %q", ErrMalformedRecord, line, sc.Text())
		}
		if strict {
			if _, exists := table[key]; exists {
				return nil, fmt.Errorf("%w: line %d: key %q", ErrDuplicateKey, line, key)
			}
		}
		table[key] = value
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("ribload: %w", err)
	}
	return table, nil
}

// parseHeader parses a "COUNT <n>" line and returns n. It rejects a negative
// or unparseable count.
func parseHeader(line string) (int, error) {
	fields := strings.Fields(line)
	if len(fields) != 2 || fields[0] != "COUNT" {
		return 0, fmt.Errorf("%w: %q", ErrMalformedHeader, line)
	}
	n, err := strconv.Atoi(fields[1])
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%w: invalid count %q", ErrMalformedHeader, fields[1])
	}
	return n, nil
}
```

### The tool

`run` takes the argument slice plus an `io.Reader` for stdin and an
`io.Writer` for stdout, so a test drives it entirely over `strings.Reader`
and `bytes.Buffer` without touching a real file descriptor. Reading is
streamed line by line through `bufio.Scanner` rather than buffered whole,
which matters once a real RIB or ConfigMap dump reaches millions of records:
the tool never needs more memory than the destination map itself. Every
error `LoadSnapshot` can produce — a malformed header, a short record, a
strict duplicate — is something the caller fixes by correcting the input or
dropping `-strict`, so `run` wraps all three in `errUsage` and `main` maps
that to exit code 2, alongside an unknown `-format` value caught before
`LoadSnapshot` ever runs. Opening the file argument is the one place a
genuine I/O failure can occur, and that error is deliberately left
unwrapped so it falls through to exit code 1 instead.

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

// errUsage marks a failure the caller can fix by changing the command line
// or the input: a bad flag, an unknown -format, a malformed header or
// record, or a strict duplicate. main maps it to exit code 2; every other
// error (an I/O failure opening or reading the file) maps to exit code 1.
var errUsage = errors.New("usage")

// run parses args, loads the snapshot from stdin or a file argument, and
// writes a one-line summary to stdout. It never touches os.Exit, so it can
// be exercised in a test with a strings.Reader and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("ribload", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	strict := fs.Bool("strict", false, "reject duplicate keys instead of overwriting")
	format := fs.String("format", "text", "output format: text or tsv")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if *format != "text" && *format != "tsv" {
		return fmt.Errorf("%w: unknown -format %q", errUsage, *format)
	}

	r := stdin
	if fs.NArg() > 0 {
		f, err := os.Open(fs.Arg(0))
		if err != nil {
			return fmt.Errorf("ribload: %w", err)
		}
		defer f.Close()
		r = f
	}

	table, err := LoadSnapshot(r, *strict)
	if err != nil {
		if errors.Is(err, ErrMalformedHeader) || errors.Is(err, ErrMalformedRecord) || errors.Is(err, ErrDuplicateKey) {
			return fmt.Errorf("%w: %v", errUsage, err)
		}
		return err
	}

	if *format == "tsv" {
		fmt.Fprintf(stdout, "records\t%d\n", len(table))
	} else {
		fmt.Fprintf(stdout, "records: %d\n", len(table))
	}
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: ribload [-strict] [-format=text|tsv] [file]")
		fmt.Fprintln(os.Stderr, "reads a COUNT header and that many KEY VALUE lines from stdin or file.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "ribload:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'COUNT 3\na 1\nb 2\nc 3\n' | go run .
printf 'COUNT 3\na 1\nb 2\nc 3\n' | go run . -format=tsv
printf 'COUNT 2\na 1\na 2\n' | go run . -strict
```

Expected output:

```text
records: 3
records	3
ribload: usage: ribload: duplicate key: line 3: key "a"
```

The first line shows the default text summary; the second, the same table
under `-format=tsv`, a tab before the count instead of a colon and space.
The third shows a strict duplicate: `a` appears twice in a two-record
snapshot, `-strict` refuses to let the second silently overwrite the first,
and the tool exits 2 rather than importing a table one entry short of what
the header promised.

### Tests

`TestLoadSnapshot` is the decode table: an ordinary table, a zero-record
snapshot, a duplicate key both accepted (non-strict) and rejected (strict),
two shapes of malformed header, a record missing its value, fewer records
than the header declared, and empty input — each malformed case checked
against its sentinel with `errors.Is`. `loadUnsized` is the antipattern from
the concepts section, reproduced as an unexported test helper: it decodes
the identical wire format but builds the map with a zero size hint instead
of the header's count. It is unreachable from `ribload`'s API by design.

`TestSizedPreallocAllocatesLess` is where that helper earns its place: it
runs `LoadSnapshot` and `loadUnsized` over the same five-thousand-record
snapshot through `testing.AllocsPerRun` and asserts `sized < unsized`, never
an exact allocation count, because the runtime's grow curve is not part of
either function's contract. This test does not call `t.Parallel`, because
`testing.AllocsPerRun` panics if invoked from a parallel test — a
concurrent goroutine allocating in the background would corrupt the
measurement.

`TestRun` drives the command end to end over its default text format, the
tsv format, a strict duplicate, an unknown `-format`, a malformed header,
and an unknown flag, checking that every rejection wraps `errUsage`.
`TestRunFileArgument` covers the file-path branch specifically, including
that a missing file produces a plain error wrapping `os.ErrNotExist` rather
than `errUsage`, so `main` exits 1 and not 2 for that case.

Create `ribload_test.go`:

```go
package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func buildSnapshot(n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "COUNT %d\n", n)
	for i := range n {
		fmt.Fprintf(&b, "key-%d value-%d\n", i, i)
	}
	return b.String()
}

func TestLoadSnapshot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		strict  bool
		wantLen int
		wantErr error
	}{
		{name: "ordinary table", input: "COUNT 3\na 1\nb 2\nc 3\n", wantLen: 3},
		{name: "zero records", input: "COUNT 0\n", wantLen: 0},
		{name: "duplicate key non-strict overwrites", input: "COUNT 2\na 1\na 2\n", wantLen: 1},
		{name: "duplicate key strict rejected", input: "COUNT 2\na 1\na 2\n", strict: true, wantErr: ErrDuplicateKey},
		{name: "malformed header text", input: "NOPE\na 1\n", wantErr: ErrMalformedHeader},
		{name: "malformed header count", input: "COUNT abc\n", wantErr: ErrMalformedHeader},
		{name: "malformed record missing value", input: "COUNT 1\nsolo\n", wantErr: ErrMalformedRecord},
		{name: "fewer records than declared", input: "COUNT 3\na 1\nb 2\n", wantErr: ErrMalformedRecord},
		{name: "empty input", input: "", wantErr: ErrMalformedHeader},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			table, err := LoadSnapshot(strings.NewReader(tc.input), tc.strict)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("LoadSnapshot error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadSnapshot: unexpected error: %v", err)
			}
			if len(table) != tc.wantLen {
				t.Fatalf("len(table) = %d, want %d", len(table), tc.wantLen)
			}
		})
	}
}

// loadUnsized parses the same wire format as LoadSnapshot but ignores the
// declared count and builds the map with make(map[string]string) -- the
// antipattern this module exists to avoid. It is unreachable from ribload's
// API; it exists only so TestSizedPreallocAllocatesLess can pin the
// allocation property the size hint buys.
func loadUnsized(r io.Reader) (map[string]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	if !sc.Scan() {
		return nil, ErrMalformedHeader
	}
	n, err := parseHeader(sc.Text())
	if err != nil {
		return nil, err
	}
	table := make(map[string]string) // zero hint: the bug this module avoids
	for i := 0; i < n; i++ {
		if !sc.Scan() {
			return nil, ErrMalformedRecord
		}
		key, value, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			return nil, ErrMalformedRecord
		}
		table[key] = value
	}
	return table, nil
}

// TestSizedPreallocAllocatesLess shows the cost the header's declared count
// lets LoadSnapshot avoid. The exact number of reallocations is a runtime
// detail and is not asserted; that the sized build needs strictly fewer
// allocations than the unsized one is the property that holds across
// toolchains.
//
// This test deliberately does not call t.Parallel: testing.AllocsPerRun
// panics when run from a parallel test, because a concurrent goroutine
// allocating in the background would corrupt its measurement.
func TestSizedPreallocAllocatesLess(t *testing.T) {
	input := buildSnapshot(5000)

	sized := testing.AllocsPerRun(20, func() {
		if _, err := LoadSnapshot(strings.NewReader(input), false); err != nil {
			t.Fatalf("LoadSnapshot: %v", err)
		}
	})
	unsized := testing.AllocsPerRun(20, func() {
		if _, err := loadUnsized(strings.NewReader(input)); err != nil {
			t.Fatalf("loadUnsized: %v", err)
		}
	})
	if !(sized < unsized) {
		t.Fatalf("allocations: sized = %v, unsized = %v; want sized < unsized", sized, unsized)
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
			name:  "text format default",
			args:  nil,
			stdin: "COUNT 3\na 1\nb 2\nc 3\n",
			want:  "records: 3\n",
		},
		{
			name:  "tsv format",
			args:  []string{"-format=tsv"},
			stdin: "COUNT 2\nx 1\ny 2\n",
			want:  "records\t2\n",
		},
		{
			name:    "strict duplicate is a usage error",
			args:    []string{"-strict"},
			stdin:   "COUNT 2\na 1\na 2\n",
			wantErr: true,
			usage:   true,
		},
		{
			name:    "unknown format is a usage error",
			args:    []string{"-format=csv"},
			stdin:   "COUNT 0\n",
			wantErr: true,
			usage:   true,
		},
		{
			name:    "malformed header is a usage error",
			args:    nil,
			stdin:   "nope\n",
			wantErr: true,
			usage:   true,
		},
		{
			name:    "unknown flag is a usage error",
			args:    []string{"-bogus"},
			stdin:   "COUNT 0\n",
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
					t.Fatalf("run(%v) error = %v, want it to wrap errUsage", tc.args, err)
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

// TestRunFileArgument checks the file-argument path end to end, and that a
// missing file is a plain I/O error -- not one wrapping errUsage -- so main
// maps it to exit code 1 rather than 2.
func TestRunFileArgument(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.txt")
	if err := os.WriteFile(path, []byte("COUNT 2\na 1\nb 2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var stdout bytes.Buffer
	if err := run([]string{path}, strings.NewReader(""), &stdout); err != nil {
		t.Fatalf("run: %v", err)
	}
	if stdout.String() != "records: 2\n" {
		t.Fatalf("stdout = %q, want %q", stdout.String(), "records: 2\n")
	}

	missing := filepath.Join(dir, "does-not-exist.txt")
	err := run([]string{missing}, strings.NewReader(""), &stdout)
	if errors.Is(err, errUsage) {
		t.Fatalf("run error = %v, want a plain I/O error, not errUsage", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run error = %v, want it to wrap os.ErrNotExist", err)
	}
}
```

## Review

`ribload` is correct when a snapshot round-trips to a map whose length and
contents match the header, regardless of whether the size hint was honored
— that alone is why the bug is invisible to a functional test. The property
that catches it is allocation count: `make(map[string]string, n)` sizes the
bucket array once from the header's declared count, while `loadUnsized`'s
`make(map[string]string)` grows and rehashes repeatedly as the same
`n` keys arrive, and `TestSizedPreallocAllocatesLess` pins `sized < unsized`
rather than any specific number, because the runtime's growth curve is not
a contract either function can rely on. Malformed input — a bad header, a
short record, a strict duplicate — maps to exit code 2 through `errUsage`;
an I/O failure opening the file argument maps to exit code 1 by staying
unwrapped. Run `go test -count=1 -race ./...` to confirm the decode table,
the allocation property, and `run`'s end-to-end behavior.

## Resources

- [`make`](https://go.dev/ref/spec#Making_slices_maps_and_channels) — the spec paragraph describing the map size hint argument.
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun) — the allocation probe, and its restriction against parallel tests.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — streaming line-oriented input without buffering the whole snapshot in memory.
- [Go Wiki: Maps and Memory Leaks](https://go.dev/wiki/CodeReviewComments) — general guidance on map growth and preallocation in the standard library review conventions.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-error-ratio-histogram-nan-guard.md](15-error-ratio-histogram-nan-guard.md) | Next: [17-readonly-snapshot-syncmap.md](17-readonly-snapshot-syncmap.md)

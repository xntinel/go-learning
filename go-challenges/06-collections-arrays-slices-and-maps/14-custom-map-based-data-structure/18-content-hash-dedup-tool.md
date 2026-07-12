# Exercise 18: Content-Hash Chunk Dedup Tool

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A backup tool in the restic or borgbackup mold splits every file it stores
into content-defined chunks and content-addresses each one: the chunk's
identity in the store is a hash of its own bytes, not a sequentially
assigned number. Two files that happen to share a chunk — the same header
block, the same vendored dependency, the same log preamble — hash to the
same digest and are stored once, no matter which file the tool backed up
first or how many separate runs it took. That deduplication only works if
the digest is *reproducible*: a later run must be able to hash a chunk it
has already seen and get the identical value the earlier run recorded in
the manifest, or it can never recognize a chunk it already has.

That reproducibility requirement quietly rules out an entire family of
otherwise excellent hash functions. `hash/maphash`, the hash Go's own
runtime uses internally to resist hash-flooding attacks against the builtin
map, is fast and well-distributed precisely because it seeds itself from a
random value drawn once per process. That randomness is the whole point for
an in-memory lookup table: an attacker who cannot predict your seed cannot
craft keys that collide into one bucket. It is exactly the wrong property
for a persisted identifier — the same bytes hashed in two different
processes produce two different digests, because they used two different
seeds, and a manifest built from `maphash` output can never be compared
against a manifest from a different run.

This module builds `chunkdedup`, a tool that content-addresses
newline-delimited chunks with a genuinely deterministic hash — `hash/fnv`
or `crypto/sha256` — and prints a manifest plus a total/unique/duplicates
summary. Its test isolates the `maphash` mistake as an unexported helper
computing one chunk's ID twice under two independently seeded
`maphash.Seed` values, and pins that the two disagree, unlike the tool's
real digest.

This module is fully self-contained: its own `go mod init`, an executable
tool, and its tests. Nothing here imports another exercise.

## What you'll build

```text
chunkdedup/               module example.com/chunkdedup
  go.mod                  go 1.24
  chunkdedup.go           package main — Deduper, Entry, Summary; NewDeduper, Dedup
  chunkdedup_test.go       package main — dedup table, cross-invocation determinism,
                          the maphash contrast, run() end to end
  main.go                 package main — -algo flag, exit codes
```

- Files: `chunkdedup.go`, `chunkdedup_test.go`, `main.go`.
- Implement: `Deduper` with `NewDeduper(algo string) (*Deduper, error)` rejecting anything but `"fnv"` or `"sha256"` with `ErrUnknownAlgo`; `(*Deduper).Dedup(r io.Reader) ([]Entry, Summary, error)` reading newline-delimited chunks and returning one `Entry{Digest, FirstSeenIndex}` per unique chunk in first-seen order plus a `Summary{Total, Unique, Duplicates}`.
- Tool: `chunkdedup` reads chunks from stdin or a file argument. `-algo=fnv|sha256` (default `sha256`) selects the digest. It prints one manifest line per unique chunk (`<hex-digest> <first-seen-index>`) followed by a summary line. Exit 0 on success, 2 on an unknown `-algo` or a bad flag, 1 on an I/O failure opening or reading the file.
- Test: the dedup table (no duplicates, all duplicates, mixed, both algorithms, empty input, a single chunk); `NewDeduper` rejecting an unknown algorithm; cross-invocation determinism for both algorithms; a `maphashDigest` contrast proving two independently seeded `maphash` computations disagree on the identical bytes; `run` end to end over `strings.Reader` and `bytes.Buffer`, including the file-argument path and a missing-file I/O error.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### A persisted identifier needs a deterministic hash, not just a fast one

`hash/fnv` and `crypto/sha256` share the property that matters here: given
the same input bytes, they produce the same output, in this process, in the
next process, on a different machine, forever. That is what "deterministic"
means for a hash function, and it is the only property a content-addressed
store can build on — the digest *is* the identity, so two computations of
the same chunk must agree or the store cannot recognize a repeat. `fnv` is
faster and its 64-bit output is enough for a dedup manifest at moderate
scale; `sha256` costs more CPU but its 256-bit output makes an accidental
collision between two different chunks astronomically unlikely, which
matters more as the chunk count grows.

`hash/maphash` fails that requirement by design, not by accident. Its whole
value is a *process-random* seed:

```go
var h maphash.Hash
h.SetSeed(maphash.MakeSeed()) // a new seed every process
h.Write(chunk)
id := h.Sum64()                // depends on this run's seed
```

Run that in two separate processes over the identical `chunk` bytes and
`id` comes out different both times, because `maphash.MakeSeed()` drew a
different seed each time. For the builtin map's internal use, that
unpredictability is a defense against an attacker crafting keys that all
hash into one bucket. For a chunk ID that gets written to a manifest and
compared against a *different* run's manifest, it is fatal: nothing about
this chunk's identity survived the process boundary. The rule is the one
this chapter's concepts page already states — `hash/maphash` for an
in-process structure that must resist hash-flooding, `hash/fnv` or
`crypto/sha256` the moment the output is persisted or compared across
process runs — and a content-addressed dedup manifest is exactly the
persisted case.

Create `chunkdedup.go`:

```go
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
)

// ErrUnknownAlgo is returned by NewDeduper when algo is neither "fnv" nor
// "sha256".
var ErrUnknownAlgo = errors.New("chunkdedup: unknown algorithm")

// Entry is one unique chunk in a manifest, identified by its content digest
// and the 1-based position in the chunk stream at which it was first seen.
type Entry struct {
	Digest         string
	FirstSeenIndex int
}

// Summary totals a Dedup run: how many chunks were read, how many distinct
// digests they reduced to, and how many repeated an earlier chunk.
type Summary struct {
	Total      int
	Unique     int
	Duplicates int
}

// Deduper content-addresses newline-delimited chunks with a fixed,
// deterministic hash algorithm, so the same chunk produces the same digest
// across separate invocations -- the property a persisted chunk manifest
// depends on to recognize a chunk a later run already stored.
//
// A Deduper is immutable after construction and is safe for concurrent use
// by multiple goroutines; Dedup itself reads its argument Reader
// sequentially and is not meant to be called concurrently on the same
// Reader.
type Deduper struct {
	digest func([]byte) string
}

// NewDeduper returns a Deduper using algo, which must be "fnv" or "sha256".
// It returns ErrUnknownAlgo for any other value.
func NewDeduper(algo string) (*Deduper, error) {
	switch algo {
	case "sha256":
		return &Deduper{digest: sha256Digest}, nil
	case "fnv":
		return &Deduper{digest: fnvDigest}, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownAlgo, algo)
	}
}

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func fnvDigest(b []byte) string {
	h := fnv.New64a()
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// Dedup reads newline-delimited chunks from r and returns one Entry per
// unique chunk, in the order each was first seen, plus a Summary of totals.
//
// Two chunks are the same chunk exactly when their digests match, computed
// deterministically with the algorithm the Deduper was constructed with, so
// a second invocation over the same bytes -- even from a different process
// -- reproduces identical digests. That determinism is the entire point: a
// persisted manifest is only useful if a later run can compare its own
// digests against it and agree.
func (d *Deduper) Dedup(r io.Reader) ([]Entry, Summary, error) {
	seen := make(map[string]struct{})
	var entries []Entry
	var total int

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		total++
		digest := d.digest(sc.Bytes())
		if _, dup := seen[digest]; dup {
			continue
		}
		seen[digest] = struct{}{}
		entries = append(entries, Entry{Digest: digest, FirstSeenIndex: total})
	}
	if err := sc.Err(); err != nil {
		return nil, Summary{}, fmt.Errorf("chunkdedup: %w", err)
	}
	return entries, Summary{Total: total, Unique: len(entries), Duplicates: total - len(entries)}, nil
}
```

### The tool

`run` takes the argument slice plus an `io.Reader` for stdin and an
`io.Writer` for stdout, so a test drives it entirely over `strings.Reader`
and `bytes.Buffer`. Chunks are streamed through `bufio.Scanner` rather than
read into one buffer, so the tool's memory footprint tracks the number of
*unique* digests seen, not the total size of everything it has read — the
property a real dedup tool needs when the input is a multi-gigabyte backup
stream. An unknown `-algo` is caught by `NewDeduper` before any input is
read, and wrapped in `errUsage` alongside a bad flag so `main` exits 2 for
either; opening the file argument is the one place a genuine I/O failure
can occur, and it is left unwrapped so it falls through to exit 1.

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

// errUsage marks a failure the caller can fix by changing the command line:
// a bad flag or an unknown -algo. main maps it to exit code 2; every other
// error (an I/O failure opening or reading the file) maps to exit code 1.
var errUsage = errors.New("usage")

// run parses args, builds a Deduper, deduplicates the chunk stream from
// stdin or a file argument, and writes the manifest and summary to stdout.
// It never touches os.Exit, so it can be exercised in a test with a
// strings.Reader and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("chunkdedup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	algo := fs.String("algo", "sha256", "hash algorithm: fnv or sha256")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	d, err := NewDeduper(*algo)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	r := stdin
	if fs.NArg() > 0 {
		f, err := os.Open(fs.Arg(0))
		if err != nil {
			return fmt.Errorf("chunkdedup: %w", err)
		}
		defer f.Close()
		r = f
	}

	entries, summary, err := d.Dedup(r)
	if err != nil {
		return err
	}

	for _, e := range entries {
		fmt.Fprintf(stdout, "%s %d\n", e.Digest, e.FirstSeenIndex)
	}
	fmt.Fprintf(stdout, "total=%d unique=%d duplicates=%d\n", summary.Total, summary.Unique, summary.Duplicates)
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: chunkdedup [-algo=fnv|sha256] [file]")
		fmt.Fprintln(os.Stderr, "reads newline-delimited chunks from stdin or file, prints a dedup manifest.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "chunkdedup:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'alpha\nbeta\nalpha\ngamma\nbeta\n' | go run .
printf 'alpha\nbeta\nalpha\n' | go run . -algo=fnv
printf 'alpha\n' | go run . -algo=md5
```

Expected output:

```text
8ed3f6ad685b959ead7022518e1af76cd816f8e8ec7ccdda1ed4018e8f2223f8 1
f44e64e75f3948e9f73f8dfa94721c4ce8cbb4f265c4790c702b2d41cfbf2753 2
be9d587defa1f0c09ef49eb17e206983a5f8f8289e4281860bd0ee5a19592c67 4
total=5 unique=3 duplicates=2
8ac625bb85ed202b 1
7627619b954620a7 2
total=3 unique=2 duplicates=1
chunkdedup: usage: chunkdedup: unknown algorithm: "md5"
```

The first command's five chunks reduce to three unique entries: `alpha` and
`beta` each appear twice, and their manifest line carries the *first*
position each was seen at (1 and 2), while `gamma`, seen once at position 3,
does not appear again — the summary's `duplicates=2` accounts for the two
repeats. The second command shows the same input under `-algo=fnv`,
producing a shorter, differently valued digest through an entirely
different hash. The third shows the exit-2 usage error for an unsupported
algorithm.

### Tests

`TestDedup` is the table: no duplicates, all duplicates, a mixed run, both
algorithms, empty input, and a single chunk, each checked against the exact
total/unique/duplicates triple. `TestDigestsAreDeterministicAcrossInvocations`
builds two separate `Deduper`s — standing in for two separate process runs
— and hashes the same chunk through each, asserting the digests agree for
both algorithms; this is the property the module exists to guarantee.

`maphashDigest` is the antipattern from the concepts section, reproduced as
an unexported test helper exactly as the exercise brief describes it: it
computes a chunk's ID with `hash/maphash` under an explicit `maphash.Seed`.
`TestMaphashSeedDisagreesAcrossProcesses` calls it twice with two
independently drawn seeds over the identical bytes and asserts the two IDs
differ — a probabilistic assertion, but one with a false-failure chance of
roughly one in 2^64, and it is never reachable from `Deduper`'s own API.
`TestRun` drives the command end to end, including the exact manifest and
summary lines and an unknown-algo usage error; `TestRunFileArgument` covers
the file-path branch and a missing file's plain I/O error.

Create `chunkdedup_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"hash/maphash"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDedup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		input          string
		algo           string
		wantUnique     int
		wantTotal      int
		wantDuplicates int
	}{
		{name: "no duplicates", input: "alpha\nbeta\ngamma\n", algo: "sha256", wantTotal: 3, wantUnique: 3, wantDuplicates: 0},
		{name: "all duplicates", input: "same\nsame\nsame\n", algo: "sha256", wantTotal: 3, wantUnique: 1, wantDuplicates: 2},
		{name: "mixed", input: "a\nb\na\nc\nb\n", algo: "sha256", wantTotal: 5, wantUnique: 3, wantDuplicates: 2},
		{name: "fnv algo", input: "a\nb\na\n", algo: "fnv", wantTotal: 3, wantUnique: 2, wantDuplicates: 1},
		{name: "empty input", input: "", algo: "sha256", wantTotal: 0, wantUnique: 0, wantDuplicates: 0},
		{name: "single chunk", input: "solo\n", algo: "sha256", wantTotal: 1, wantUnique: 1, wantDuplicates: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d, err := NewDeduper(tc.algo)
			if err != nil {
				t.Fatalf("NewDeduper: %v", err)
			}
			entries, summary, err := d.Dedup(strings.NewReader(tc.input))
			if err != nil {
				t.Fatalf("Dedup: %v", err)
			}
			if summary.Total != tc.wantTotal || summary.Unique != tc.wantUnique || summary.Duplicates != tc.wantDuplicates {
				t.Fatalf("summary = %+v, want total=%d unique=%d duplicates=%d",
					summary, tc.wantTotal, tc.wantUnique, tc.wantDuplicates)
			}
			if len(entries) != tc.wantUnique {
				t.Fatalf("len(entries) = %d, want %d", len(entries), tc.wantUnique)
			}
		})
	}
}

func TestNewDeduperRejectsUnknownAlgo(t *testing.T) {
	t.Parallel()

	if _, err := NewDeduper("md5"); !errors.Is(err, ErrUnknownAlgo) {
		t.Fatalf("NewDeduper(md5) error = %v, want ErrUnknownAlgo", err)
	}
}

// TestDigestsAreDeterministicAcrossInvocations pins the property a
// persisted manifest depends on: hashing the identical chunk through two
// separate Deduper instances -- standing in for two separate process runs
// -- produces the identical digest, for both supported algorithms.
func TestDigestsAreDeterministicAcrossInvocations(t *testing.T) {
	t.Parallel()

	for _, algo := range []string{"sha256", "fnv"} {
		d1, err := NewDeduper(algo)
		if err != nil {
			t.Fatalf("NewDeduper(%s): %v", algo, err)
		}
		d2, err := NewDeduper(algo)
		if err != nil {
			t.Fatalf("NewDeduper(%s): %v", algo, err)
		}
		e1, _, err := d1.Dedup(strings.NewReader("payload\n"))
		if err != nil {
			t.Fatalf("Dedup: %v", err)
		}
		e2, _, err := d2.Dedup(strings.NewReader("payload\n"))
		if err != nil {
			t.Fatalf("Dedup: %v", err)
		}
		if e1[0].Digest != e2[0].Digest {
			t.Fatalf("algo %s: digests disagree across invocations: %s != %s", algo, e1[0].Digest, e2[0].Digest)
		}
	}
}

// maphashDigest is the chunk-ID computation this module must never use for
// a persisted manifest: hash/maphash seeds itself with a value drawn once
// per process (maphash.MakeSeed), so the identical chunk hashes differently
// under two independently seeded computations -- exactly what happens when
// a later run of a real dedup tool is a different process than the one that
// wrote the manifest it is comparing against. maphashDigest is unreachable
// from Deduper's API and exists only so the tests can pin the disagreement.
func maphashDigest(seed maphash.Seed, data []byte) string {
	var h maphash.Hash
	h.SetSeed(seed)
	h.Write(data)
	return strconv.FormatUint(h.Sum64(), 16)
}

// TestMaphashSeedDisagreesAcrossProcesses contrasts the antipattern against
// Deduper's real behavior. Two independently seeded maphash computations
// over the identical bytes disagree (the seed is drawn from per-process
// randomness, so agreement would need a 1-in-2^64 coincidence), while two
// separate sha256-backed Dedupers agree, exactly as
// TestDigestsAreDeterministicAcrossInvocations already showed.
func TestMaphashSeedDisagreesAcrossProcesses(t *testing.T) {
	t.Parallel()

	data := []byte("chunk contents a real backup tool would hash")
	id1 := maphashDigest(maphash.MakeSeed(), data)
	id2 := maphashDigest(maphash.MakeSeed(), data)
	if id1 == id2 {
		t.Fatalf("two independently seeded maphash digests agreed by chance: %s == %s", id1, id2)
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
			name:  "sha256 default",
			stdin: "a\nb\na\n",
			want: "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb 1\n" +
				"3e23e8160039594a33894f6564e1b1348bbd7a0088d42c4acb73eeaed59c009d 2\n" +
				"total=3 unique=2 duplicates=1\n",
		},
		{
			name:    "unknown algo is a usage error",
			args:    []string{"-algo=md5"},
			stdin:   "a\n",
			wantErr: true,
			usage:   true,
		},
		{
			name:    "unknown flag is a usage error",
			args:    []string{"-bogus"},
			stdin:   "a\n",
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
	path := filepath.Join(dir, "chunks.txt")
	if err := os.WriteFile(path, []byte("x\ny\nx\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var stdout bytes.Buffer
	if err := run([]string{path}, strings.NewReader(""), &stdout); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.HasSuffix(stdout.String(), "total=3 unique=2 duplicates=1\n") {
		t.Fatalf("stdout = %q, want it to end with the summary line", stdout.String())
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

`chunkdedup` is correct when the identical chunk hashes to the identical
digest no matter how many times, or in how many separate processes, it is
hashed — that reproducibility is the entire reason a content-addressed
store works. `sha256Digest` and `fnvDigest` both have that property;
`TestDigestsAreDeterministicAcrossInvocations` pins it directly by hashing
the same chunk through two independent `Deduper`s and requiring agreement.
`maphashDigest` is the mistake this module isolates: a chunk ID computed
with `hash/maphash`'s process-random seed disagrees with itself across two
independently seeded computations, which `TestMaphashSeedDisagreesAcrossProcesses`
demonstrates directly rather than merely asserting by citation — and that
helper is unreachable from `Deduper`'s own API, exactly as it would need to
be kept out of a real tool. An unknown `-algo` and a bad flag both map to
exit code 2 through `errUsage`; a file-open failure maps to exit code 1 by
staying unwrapped. Run `go test -count=1 -race ./...` to confirm the dedup
table, the determinism property, the `maphash` contrast, and `run`'s
end-to-end behavior.

## Resources

- [`hash/maphash`](https://pkg.go.dev/hash/maphash) — `MakeSeed`, and the per-process-random seeding this module's antipattern relies on.
- [`crypto/sha256`](https://pkg.go.dev/crypto/sha256) — the default, collision-resistant digest for the persisted manifest.
- [`hash/fnv`](https://pkg.go.dev/hash/fnv) — the faster, non-cryptographic alternative selectable via `-algo=fnv`.
- [restic design documentation](https://restic.readthedocs.io/en/stable/100_references.html) — content-defined chunking and content-addressed storage in a real backup tool.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-readonly-snapshot-syncmap.md](17-readonly-snapshot-syncmap.md) | Next: [19-composite-key-token-bucket.md](19-composite-key-token-bucket.md)

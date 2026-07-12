# Exercise 20: KV Snapshot Diff Tool

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A config-drift detector compares two point-in-time snapshots of the same
key/value space — a Kubernetes `ConfigMap` before and after a rollout, a
Consul KV tree before and after a change, a rendered Terraform state before
and after `plan` — and reports exactly what moved. This is the check that
gates a deploy: an operator wants to see every addition, removal, and value
change before it goes live, not just the ones that happen to be easy to
spot. Missing even one kind of change defeats the entire point of running a
diff in the first place.

The natural first draft loops over the new snapshot and compares each value
against the old one:

```go
for k, v := range newSnapshot {
    if oldSnapshot[k] != v {
        report(k)
    }
}
```

This finds every addition (a key with no old value) and every value change
correctly. It can never find a deletion, because a key that was removed
entirely does not exist in `newSnapshot` — the loop that ranges over
`newSnapshot` simply never visits it. The bug is invisible in the common
case, where most deploys only add or change keys, and it is exactly the
case that matters most in an incident: a key silently dropped from a
`ConfigMap` is the kind of change a config-drift detector exists to catch,
and this version of the tool cannot.

This module builds `kvdiff`, a tool that reads two `KEY=VALUE` snapshots and
diffs them by checking membership with comma-ok in *both* directions, which
is the only way to see all three kinds of change, deletions included.

This module is fully self-contained: its own `go mod init`, an executable
tool, and its tests. Nothing here imports another exercise.

## What you'll build

```text
kvdiff/                   module example.com/kvdiff
  go.mod                  go 1.24
  kvdiff.go               package main — ParseKV, Diff, Change, ChangeKind; ErrMalformedLine
  kvdiff_test.go          package main — parse table, diff table, the one-direction
                          contrast, run() end to end
  main.go                 package main — positional args, "-" for stdin, exit codes
```

- Files: `kvdiff.go`, `kvdiff_test.go`, `main.go`.
- Implement: `ParseKV(r io.Reader) (map[string]string, error)` reading `KEY=VALUE` lines (blank lines skipped, first `=` splits key from value) and returning `ErrMalformedLine` for anything else; `Diff(before, after map[string]string) []Change` returning every `Added`, `Removed`, and `Changed` difference, sorted by key.
- Tool: `kvdiff <old-file> <new-file>`, either path may be `-` for stdin (not both). Each file is `KEY=VALUE` per line. It prints one difference line per change: `+key=value` added, `-key=value` removed, `~key=old->new` changed, sorted by key. Exit 0 when both files parse, 2 on a malformed `KEY=VALUE` line, a bad argument count, or both paths being `-`, 1 on a file-open failure.
- Test: the parse table (ordinary, blank lines, a value containing `=`, empty input, a missing `=`, an empty key); the diff table across added, removed, changed, and unchanged keys, plus empty and nil-map edge cases; a `diffNaive` contrast proving a one-direction loop never reports a deletion; `run` end to end over file arguments and the `-` stdin path, including a malformed line, a missing file, and both bad-argument cases.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Comma-ok in both directions is the only way to see every kind of change

`v, ok := m[k]` is Go's only way to distinguish "key absent" from "key
present with the zero value", and a correct diff needs that distinction
twice: once to check whether a key from one snapshot exists in the other.
Iterating only the new snapshot answers "does this key's value differ from
before" for every key *that still exists* — it can report an addition (the
old side's comma-ok says absent) and a change (both present, values
differ), but a deletion never enters that loop at all, because the loop's
range expression is the map that no longer has the key.

The fix is to union the key spaces first, then classify each key by
checking presence on both sides:

```go
oldVal, oldOK := before[k]
newVal, newOK := after[k]
switch {
case !oldOK && newOK:
    // added
case oldOK && !newOK:
    // removed -- only reachable by checking the old side too
case oldOK && newOK && oldVal != newVal:
    // changed
}
```

Building the union of keys — everything from `before` plus everything from
`after` — costs one extra pass over each map, and it is what makes the
`Removed` branch reachable at all. There is no way to shortcut this into a
single-map iteration and still catch every deletion; the union is not an
optimization detail, it is the fix.

Create `kvdiff.go`:

```go
package main

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// ErrMalformedLine is returned by ParseKV when a non-blank line is not of
// the form KEY=VALUE with a non-empty KEY.
var ErrMalformedLine = errors.New("kvdiff: malformed KEY=VALUE line")

// ParseKV reads a KEY=VALUE snapshot from r, one entry per line, and
// returns it as a map. Blank lines are skipped. The VALUE half may itself
// contain "=" -- ParseKV splits on the first "=" only, via strings.Cut, so
// a value like "url=https://host/a=b" is preserved whole.
func ParseKV(r io.Reader) (map[string]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	out := make(map[string]string)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if text == "" {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("%w: line %d: %q", ErrMalformedLine, line, text)
		}
		out[key] = value
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("kvdiff: %w", err)
	}
	return out, nil
}

// ChangeKind classifies one entry in a Diff result.
type ChangeKind int

const (
	// Added means the key exists in the new snapshot only.
	Added ChangeKind = iota
	// Removed means the key exists in the old snapshot only.
	Removed
	// Changed means the key exists in both snapshots with different values.
	Changed
)

// Change is one difference between two key/value snapshots.
type Change struct {
	Key      string
	Kind     ChangeKind
	OldValue string
	NewValue string
}

// Diff compares two point-in-time key/value snapshots and returns every
// difference between them, sorted by key.
//
// Diff checks every key that appears in either snapshot -- present in
// before but not after (Removed), present in after but not before (Added),
// or present in both with different values (Changed) -- by testing
// membership with comma-ok against both maps in both directions. Checking
// only one direction can report an addition or a change, but it can never
// report a deletion: a key entirely absent from after is never visited by
// a loop that only ranges over after. See kvdiff_test.go for that failure
// pinned directly.
func Diff(before, after map[string]string) []Change {
	keys := make(map[string]struct{}, len(before)+len(after))
	for k := range before {
		keys[k] = struct{}{}
	}
	for k := range after {
		keys[k] = struct{}{}
	}

	changes := make([]Change, 0, len(keys))
	for k := range keys {
		oldVal, oldOK := before[k]
		newVal, newOK := after[k]
		switch {
		case !oldOK && newOK:
			changes = append(changes, Change{Key: k, Kind: Added, NewValue: newVal})
		case oldOK && !newOK:
			changes = append(changes, Change{Key: k, Kind: Removed, OldValue: oldVal})
		case oldOK && newOK && oldVal != newVal:
			changes = append(changes, Change{Key: k, Kind: Changed, OldValue: oldVal, NewValue: newVal})
		}
	}
	slices.SortFunc(changes, func(a, b Change) int { return cmp.Compare(a.Key, b.Key) })
	return changes
}
```

### The tool

`run` takes the argument slice plus an `io.Reader` for stdin and an
`io.Writer` for stdout, so a test drives every code path — two files, one
file plus stdin, and every error case — without touching a real file
descriptor for the stdin branch. Either positional argument may be `-`, but
not both at once: `os.Stdin` can only be consumed once, so reading both
snapshots from it would silently starve the second `ParseKV` call, and
`run` rejects that combination up front instead of producing a confusing
empty diff. Both `ParseKV` errors and a bad argument count wrap `errUsage`
and map to exit code 2; the one place a real I/O failure can occur —
opening a named file that does not exist — is left unwrapped and falls
through to exit code 1.

Create `main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// errUsage marks a failure the caller can fix by changing the command line
// or the input: the wrong number of arguments, both paths set to "-", or a
// malformed KEY=VALUE line. main maps it to exit code 2; every other error
// (an I/O failure opening a file) maps to exit code 1.
var errUsage = errors.New("usage")

// openInput opens path for reading, or wraps stdin if path is "-".
func openInput(path string, stdin io.Reader) (io.ReadCloser, error) {
	if path == "-" {
		return io.NopCloser(stdin), nil
	}
	return os.Open(path)
}

// run parses its two file arguments, diffs the resulting snapshots, and
// writes one line per difference to stdout. It never touches os.Exit, so it
// can be exercised in a test with a strings.Reader and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) != 2 {
		return fmt.Errorf("%w: want exactly 2 arguments (old-file new-file), got %d", errUsage, len(args))
	}
	oldPath, newPath := args[0], args[1]
	if oldPath == "-" && newPath == "-" {
		return fmt.Errorf("%w: only one file argument may be \"-\" (stdin)", errUsage)
	}

	oldR, err := openInput(oldPath, stdin)
	if err != nil {
		return fmt.Errorf("kvdiff: %w", err)
	}
	defer oldR.Close()
	before, err := ParseKV(oldR)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	newR, err := openInput(newPath, stdin)
	if err != nil {
		return fmt.Errorf("kvdiff: %w", err)
	}
	defer newR.Close()
	after, err := ParseKV(newR)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	for _, c := range Diff(before, after) {
		switch c.Kind {
		case Added:
			fmt.Fprintf(stdout, "+%s=%s\n", c.Key, c.NewValue)
		case Removed:
			fmt.Fprintf(stdout, "-%s=%s\n", c.Key, c.OldValue)
		case Changed:
			fmt.Fprintf(stdout, "~%s=%s->%s\n", c.Key, c.OldValue, c.NewValue)
		}
	}
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "kvdiff:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'a=1\nb=2\nc=3\n' > old.kv
printf 'a=1\nb=20\nd=4\n' > new.kv
go run . old.kv new.kv
printf 'a=1\nb=2\n' | go run . - new.kv
go run . old.kv
```

Expected output:

```text
~b=2->20
-c=3
+d=4
~b=2->20
+d=4
kvdiff: usage: want exactly 2 arguments (old-file new-file), got 1
```

The first command diffs two files directly: `b` changed value, `c` was
removed, `d` was added, and `a` — unchanged — produces no line at all. The
second reads the old snapshot from stdin via `-` and diffs it against
`new.kv`; because stdin here only has `a` and `b`, there is no `c` to
report as removed, and the output correctly reflects that. The third shows
the exit-2 usage error for a missing second argument.

### Tests

`TestParseKV` covers the input grammar: an ordinary file, blank lines
skipped, a value that itself contains `=` (only the first `=` splits),
empty input producing an empty map, a line with no `=`, and a line with an
empty key. `TestDiff` is the case that matters most: one snapshot with a
key unchanged, one changed, one removed, and one added, checked against the
exact sorted `[]Change` Diff should produce. `TestDiffEdgeCases` sweeps
empty maps, nil maps, an all-additions snapshot, and an all-removals
snapshot.

`diffNaive` is the antipattern from the concepts section, reproduced as an
unexported test helper exactly as described: a loop that ranges only over
`after`. `TestNaiveDiffMissesRemovedKeys` removes a key from `before` that
does not appear in `after` at all, shows `diffNaive` reports nothing for
it — the loop never visits a key it does not iterate over — and shows
`Diff` correctly reports it `Removed`. `TestRun` and
`TestRunStdinForOneFile` drive the command end to end over files and the
`-` stdin path; `TestRunErrors` covers a bad argument count, both paths
being `-`, a malformed line, and a missing file, checking that only the
last one falls outside `errUsage`.

Create `kvdiff_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseKV(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    map[string]string
		wantErr error
	}{
		{name: "ordinary", input: "a=1\nb=2\n", want: map[string]string{"a": "1", "b": "2"}},
		{name: "blank lines skipped", input: "a=1\n\nb=2\n\n", want: map[string]string{"a": "1", "b": "2"}},
		{name: "value contains equals", input: "url=https://host/a=b\n", want: map[string]string{"url": "https://host/a=b"}},
		{name: "empty input", input: "", want: map[string]string{}},
		{name: "missing equals", input: "nope\n", wantErr: ErrMalformedLine},
		{name: "empty key", input: "=value\n", wantErr: ErrMalformedLine},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseKV(strings.NewReader(tc.input))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParseKV error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseKV: unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseKV = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("ParseKV[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestDiff(t *testing.T) {
	t.Parallel()

	before := map[string]string{"a": "1", "b": "2", "c": "3"}
	after := map[string]string{"a": "1", "b": "20", "d": "4"}
	// a: unchanged, b: changed, c: removed, d: added

	changes := Diff(before, after)
	want := []Change{
		{Key: "b", Kind: Changed, OldValue: "2", NewValue: "20"},
		{Key: "c", Kind: Removed, OldValue: "3"},
		{Key: "d", Kind: Added, NewValue: "4"},
	}
	if !slices.Equal(changes, want) {
		t.Fatalf("Diff = %+v, want %+v", changes, want)
	}
}

func TestDiffEdgeCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		before map[string]string
		after  map[string]string
		want   int
	}{
		{name: "both empty", before: map[string]string{}, after: map[string]string{}, want: 0},
		{name: "identical", before: map[string]string{"a": "1"}, after: map[string]string{"a": "1"}, want: 0},
		{name: "nil maps", before: nil, after: nil, want: 0},
		{name: "everything added", before: map[string]string{}, after: map[string]string{"a": "1", "b": "2"}, want: 2},
		{name: "everything removed", before: map[string]string{"a": "1", "b": "2"}, after: map[string]string{}, want: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := len(Diff(tc.before, tc.after)); got != tc.want {
				t.Fatalf("len(Diff) = %d, want %d", got, tc.want)
			}
		})
	}
}

// diffNaive reproduces this module's antipattern: it only iterates the new
// snapshot, so a key present in the old snapshot but absent from the new
// one is never visited and therefore never reported as removed. It is
// unreachable from Diff's own API and exists only so
// TestNaiveDiffMissesRemovedKeys can pin the omission.
func diffNaive(before, after map[string]string) []string {
	var changed []string
	for k, v := range after {
		if before[k] != v {
			changed = append(changed, k)
		}
	}
	return changed
}

// TestNaiveDiffMissesRemovedKeys is the heart of this module. Deleting key
// "b" from before is invisible to a loop that only ranges over after: the
// naive diff reports nothing for it, while Diff -- which checks membership
// in both directions with comma-ok -- correctly reports it Removed.
func TestNaiveDiffMissesRemovedKeys(t *testing.T) {
	t.Parallel()

	before := map[string]string{"a": "1", "b": "2", "c": "3"}
	after := map[string]string{"a": "1", "c": "3"} // "b" removed, nothing else changed

	naive := diffNaive(before, after)
	if slices.Contains(naive, "b") {
		t.Fatalf("diffNaive reported removed key %q, but it only ranges over the new snapshot and can never see it", "b")
	}
	if len(naive) != 0 {
		t.Fatalf("diffNaive = %v, want empty: no key present in the new snapshot changed value", naive)
	}

	changes := Diff(before, after)
	if len(changes) != 1 || changes[0].Kind != Removed || changes[0].Key != "b" {
		t.Fatalf("Diff = %+v, want a single Removed change for key %q", changes, "b")
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.kv")
	newPath := filepath.Join(dir, "new.kv")
	if err := os.WriteFile(oldPath, []byte("a=1\nb=2\nc=3\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(newPath, []byte("a=1\nb=20\nd=4\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var stdout bytes.Buffer
	if err := run([]string{oldPath, newPath}, strings.NewReader(""), &stdout); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "~b=2->20\n-c=3\n+d=4\n"
	if stdout.String() != want {
		t.Fatalf("run stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRunStdinForOneFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	newPath := filepath.Join(dir, "new.kv")
	if err := os.WriteFile(newPath, []byte("a=1\nb=99\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var stdout bytes.Buffer
	if err := run([]string{"-", newPath}, strings.NewReader("a=1\nb=2\n"), &stdout); err != nil {
		t.Fatalf("run: %v", err)
	}
	if stdout.String() != "~b=2->99\n" {
		t.Fatalf("run stdout = %q, want %q", stdout.String(), "~b=2->99\n")
	}
}

func TestRunErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	goodPath := filepath.Join(dir, "good.kv")
	if err := os.WriteFile(goodPath, []byte("a=1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	badPath := filepath.Join(dir, "bad.kv")
	if err := os.WriteFile(badPath, []byte("nope\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	missing := filepath.Join(dir, "does-not-exist.kv")

	tests := []struct {
		name  string
		args  []string
		usage bool
	}{
		{name: "wrong argument count", args: []string{goodPath}, usage: true},
		{name: "both stdin", args: []string{"-", "-"}, usage: true},
		{name: "malformed line is a usage error", args: []string{badPath, goodPath}, usage: true},
		{name: "missing file is a plain I/O error", args: []string{missing, goodPath}, usage: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			err := run(tc.args, strings.NewReader(""), &stdout)
			if err == nil {
				t.Fatalf("run(%v): want error, got nil", tc.args)
			}
			if got := errors.Is(err, errUsage); got != tc.usage {
				t.Fatalf("run(%v) errors.Is(err, errUsage) = %v, want %v (err=%v)", tc.args, got, tc.usage, err)
			}
		})
	}
}
```

## Review

`kvdiff` is correct when it reports all three kinds of drift — additions,
removals, and value changes — and the mistake it inoculates against is
specific to the third kind falling silently out of scope: `diffNaive`
ranges only over the new snapshot, so a key removed entirely never enters
its loop and is never reported, which `TestNaiveDiffMissesRemovedKeys`
demonstrates directly against `Diff`'s correct behavior on the identical
input. `Diff` avoids that by unioning both snapshots' keys first and
checking comma-ok membership on both sides for every key in that union —
the only way a deletion becomes visible. A malformed `KEY=VALUE` line, a
bad argument count, and using `-` for both files all map to exit code 2
through `errUsage`; a missing file maps to exit code 1 by staying
unwrapped. Run `go test -count=1 -race ./...` to confirm the parse and diff
tables, the one-direction contrast, and `run`'s end-to-end behavior across
files and stdin.

## Resources

- [Two-value map index expressions](https://go.dev/ref/spec#Index_expressions) — the comma-ok form this module relies on in both directions.
- [`slices.SortFunc`](https://pkg.go.dev/slices#SortFunc) and [`cmp.Compare`](https://pkg.go.dev/cmp#Compare) — sorting the diff output deterministically by key.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — streaming line-oriented input for both snapshot files.
- [Kubernetes ConfigMap](https://kubernetes.io/docs/concepts/configuration/configmap/) — a real key/value snapshot this style of diff gates a rollout against.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-composite-key-token-bucket.md](19-composite-key-token-bucket.md) | Next: [../../07-structs-and-methods/01-struct-declaration-and-initialization/00-concepts.md](../../07-structs-and-methods/01-struct-declaration-and-initialization/00-concepts.md)

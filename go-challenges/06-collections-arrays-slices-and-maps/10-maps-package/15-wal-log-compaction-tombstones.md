# Exercise 15: Write-Ahead Log Compaction with Tombstone Pruning

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Kafka's log-compaction topics and every LSM-tree storage engine (RocksDB,
LevelDB, and the Bitcask-style stores restic and etcd's boltdb draw on) face
the same problem: an append-only log of writes grows forever, but the reader
only cares about the latest value per key, and a delete is itself just
another write — a tombstone — that must eventually cause the key to vanish
rather than linger with a stale value. Compaction collapses the log into
that final state in one pass: replay every event in order, keep only the
latest write per key, and drop any key whose latest write was a tombstone.
That is exactly `maps.DeleteFunc(state, isTombstoned)`, run once after a
full replay, applied to a real production shape instead of a synthetic one.

The trap is doing that same job the way a first draft usually does: treating
compaction as something that has to happen incrementally, on every
tombstone, by rewriting and rescanning the log so far — instead of realizing
the record map already *is* the compacted state as of the last write, and a
single terminal prune is both cheaper and correct. This module builds the
correct replay-then-prune pipeline as a small command, and pins the specific
off-by-one that an incremental rescan strategy invites: a WAL that ends
right after a delete — an entirely ordinary way for a compaction run to be
triggered — is precisely the case where a rescan-per-tombstone strategy
misses its own last tombstone and lets the deleted key reappear.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
walcompact/                    module example.com/walcompact
  go.mod                       go 1.24
  walcompact.go                package main — Op, ParseLine, Replay, Compact
  walcompact_test.go           package main — parsing table, replay/compact table,
                               the compactNaive contrast, run() end to end
  main.go                      package main — no flags, stdin only, exit codes
```

- Files: `walcompact.go`, `walcompact_test.go`, `main.go`.
- Implement: `ParseLine(line string) (key string, op Op, err error)` parsing `key=value` (a set) or `key=` (a tombstone), rejecting a line with no `=` or an empty key with `ErrMalformedLine`; `Replay(r io.Reader) (map[string]Op, error)` folding every line in order into a record map, wrapping a malformed line with its 1-based line number; `Compact(records map[string]Op) map[string]string` pruning tombstoned entries with one `maps.DeleteFunc` pass.
- Tool: `walcompact` reads the WAL from stdin, one `key=value` or `key=` line at a time, and prints the compacted state sorted by key to stdout. It takes no flags or arguments. Exit 0 on success, exit 2 on a malformed line or an unexpected argument, exit 1 is reserved for a runtime failure reading stdin.
- Test: line parsing including the malformed cases; replay-then-compact across overwrite, tombstone, revive-after-tombstone, unrelated-key-survives, blank-line-skip, and empty-input cases; a malformed line's error names its line number; the `compactNaive` contrast pinning the trailing-tombstone resurrection bug; `run` end to end over a `strings.Reader` and a `bytes.Buffer`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why one terminal DeleteFunc pass, and what a per-tombstone rescan gets wrong

`Replay` folds the log into a record map the way any last-write-wins state
machine does: `records[key] = op` on every line, no matter whether `op` is a
set or a tombstone. After replay, `records` already holds, for every key
that ever appeared, its single most recent event — that is what "last write
wins" means. Compaction is then just deciding what to do with the entries
whose most recent event happens to be a tombstone: drop them. There is
nothing incremental about that decision; it is a property of the finished
map, checked once, with `maps.DeleteFunc(records, func(_ string, op Op)
bool { return op.Tombstone })`. One traversal, deletions applied to the
existing backing store, no new map allocated for the survivor set.

The version this module is built to contrast against tries to keep the
state compacted *as it goes*: on every tombstone, rewrite the event log so
far, dropping every prior entry for that key. It looks more "eager" and
therefore safer, but it recomputes the running state by rescanning a
growing log again and again, and it is exactly the kind of code that hides
an off-by-one in its rescan boundary — usually because the line that just
appended the current tombstone is, at the moment of the rescan, mentally
"already handled" even though the loop bound says otherwise:

```go
state = map[string]string{}
for i, o := range log[:len(log)-1] { // the tombstone just appended is excluded
    ...
}
```

If that tombstone happens to be the *last* line of the file, there is no
subsequent rescan to pick up the entry the bound excluded, and the key's
last set value survives compaction under a delete. See `walcompact_test.go`
for the full trace.

Create `walcompact.go`:

```go
// Command walcompact replays a write-ahead log of key sets and tombstones
// and prints the compacted final state: the latest write per key, with any
// key whose latest write was a tombstone dropped entirely.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
)

// ErrMalformedLine is returned when a WAL line is not "key=value" (a set)
// or "key=" (a tombstone: an empty value).
var ErrMalformedLine = errors.New("walcompact: malformed line")

// Op is one write-ahead-log event for a single key: a set (Tombstone is
// false and Value holds the written value) or a tombstone (Tombstone is
// true and Value is always empty).
type Op struct {
	Value     string
	Tombstone bool
}

// ParseLine parses one WAL line of the form "key=value" or "key=". A value
// of "" marks the line as a tombstone: RFC-free, this format has no way to
// set a key to the empty string, and that ambiguity is resolved in favor of
// deletion, matching Kafka log-compaction's own null-value-means-tombstone
// convention. ParseLine returns ErrMalformedLine if the line has no '=' or
// an empty key.
func ParseLine(line string) (key string, op Op, err error) {
	i := strings.IndexByte(line, '=')
	if i <= 0 {
		return "", Op{}, fmt.Errorf("%w: %q", ErrMalformedLine, line)
	}
	value := line[i+1:]
	return line[:i], Op{Value: value, Tombstone: value == ""}, nil
}

// Replay reads WAL lines from r, one per line, and folds them in order into
// a record map: each line overwrites any earlier entry for the same key,
// including a set that follows a tombstone (the key is live again) and a
// tombstone that follows a set (the key is marked for deletion again).
// Blank lines are skipped. The returned map holds the raw per-key record --
// live sets and tombstones alike -- and is not yet compacted; pass it to
// Compact to prune the tombstoned entries.
//
// A malformed line stops the replay and returns an error wrapping
// ErrMalformedLine together with the 1-based line number that failed. An
// error reading r itself is returned unwrapped: it is a runtime failure,
// not a malformed-input error.
func Replay(r io.Reader) (map[string]Op, error) {
	records := make(map[string]Op)
	sc := bufio.NewScanner(r)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if line == "" {
			continue
		}
		key, op, err := ParseLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		records[key] = op
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("walcompact: reading input: %w", err)
	}
	return records, nil
}

// Compact prunes every tombstoned entry from records in a single
// maps.DeleteFunc pass and returns the resulting live key/value state.
//
// Compact mutates records in place -- the tombstoned entries are gone from
// the map the caller passed in -- but the returned map is freshly built
// from the survivors and shares no backing storage with records, so the
// caller may retain and mutate either one independently afterward.
func Compact(records map[string]Op) map[string]string {
	maps.DeleteFunc(records, func(_ string, op Op) bool {
		return op.Tombstone
	})
	state := make(map[string]string, len(records))
	for k, op := range records {
		state[k] = op.Value
	}
	return state
}
```

### The tool

`walcompact` has exactly one job and no configuration: read the WAL from
stdin, compact it, print the result. `run` takes the argument slice and an
`io.Reader`/`io.Writer` pair rather than touching `os.Stdin`/`os.Stdout`
directly, which is what lets the test drive it over a `strings.Reader` and a
`bytes.Buffer`. A malformed line surfaces from `Replay` wrapping
`ErrMalformedLine`; `run` recognizes that specific error and re-wraps it in
the package-level `errUsage` sentinel, which `main` maps to exit code 2 —
the caller's WAL is bad, not the tool. Any other error out of `Replay` (a
genuine I/O failure reading stdin) is returned as-is and maps to exit code
1. Sorting the output by key before printing is the same fix `00-concepts.md`
gives for any output that must be stable: `slices.Sorted(maps.Keys(state))`.

Create `main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
)

// errUsage marks a failure the caller can fix by changing the input: a
// malformed WAL line, or an unexpected argument. main maps it to exit code
// 2; every other error maps to exit code 1.
var errUsage = errors.New("usage")

// run reads a WAL from stdin, replays and compacts it, and writes the final
// state sorted by key to stdout. It takes no arguments; walcompact reads
// only stdin. run never touches os.Exit, so it can be exercised in a test
// with a strings.Reader and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("%w: walcompact takes no arguments; it reads the WAL from stdin", errUsage)
	}

	records, err := Replay(stdin)
	if err != nil {
		if errors.Is(err, ErrMalformedLine) {
			return fmt.Errorf("%w: %v", errUsage, err)
		}
		return err // runtime failure reading stdin
	}

	state := Compact(records)
	for _, k := range slices.Sorted(maps.Keys(state)) {
		fmt.Fprintf(stdout, "%s=%s\n", k, state[k])
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
printf 'user:1=alice\nuser:2=bob\nuser:1=\nuser:3=carol\nuser:2=bobby\n' | go run .
printf 'bad-line-no-equals\n' | go run .
```

Expected output:

```text
user:2=bobby
user:3=carol
walcompact: usage: line 1: walcompact: malformed line: "bad-line-no-equals"
```

The first run replays five events: `user:1` is set then tombstoned, so it is
absent from the compacted state; `user:2` is set twice, and the second value
(`bobby`) is what survives; `user:3` is set once and survives untouched. The
second run shows the exit-2 usage path: a line with no `=` fails
`ParseLine`, `Replay` wraps it with its line number, and `run` re-wraps that
in `errUsage`.

### Tests

`TestParseLine` and `TestReplayAndCompact` are the tables: every event
ordering that matters — overwrite, tombstone, revive-after-tombstone, an
unrelated key surviving a neighbor's tombstone, blank lines, empty input —
each checked by exact key set and value. `TestReplayMalformedLineReportsLineNumber`
pins that the wrapped error names the failing line, which is what makes a
bad WAL debuggable instead of just "somewhere in this file."

`TestCompactNaiveResurrectsTrailingTombstone` is the module's reason to
exist. `compactNaive` is unexported and unreachable from any exported
function; it exists only so the test can demonstrate, with a two-line WAL
that sets a key and then immediately tombstones it, that the rescan-per-tombstone
strategy returns the key with its pre-delete value instead of omitting it —
while `Replay` followed by `Compact` on the identical input correctly omits
it. If a future edit ever reintroduces an incremental rescan into the real
pipeline, this is the shape of input that would catch it.

Create `walcompact_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestParseLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		line    string
		wantKey string
		wantOp  Op
		wantErr bool
	}{
		{name: "set", line: "user:1=alice", wantKey: "user:1", wantOp: Op{Value: "alice", Tombstone: false}},
		{name: "tombstone", line: "user:1=", wantKey: "user:1", wantOp: Op{Value: "", Tombstone: true}},
		{name: "value contains equals", line: "k=a=b", wantKey: "k", wantOp: Op{Value: "a=b", Tombstone: false}},
		{name: "no equals sign", line: "bogus", wantErr: true},
		{name: "empty key", line: "=value", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			key, op, err := ParseLine(tc.line)
			if tc.wantErr {
				if !errors.Is(err, ErrMalformedLine) {
					t.Fatalf("ParseLine(%q) err = %v, want ErrMalformedLine", tc.line, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLine(%q): %v", tc.line, err)
			}
			if key != tc.wantKey || op != tc.wantOp {
				t.Fatalf("ParseLine(%q) = %q, %+v, want %q, %+v", tc.line, key, op, tc.wantKey, tc.wantOp)
			}
		})
	}
}

func TestReplayAndCompact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  map[string]string
	}{
		{
			name:  "later set overwrites earlier set",
			input: "a=1\na=2\n",
			want:  map[string]string{"a": "2"},
		},
		{
			name:  "tombstone drops the key",
			input: "a=1\na=\n",
			want:  map[string]string{},
		},
		{
			name:  "set after tombstone revives the key",
			input: "a=1\na=\na=3\n",
			want:  map[string]string{"a": "3"},
		},
		{
			name:  "unrelated keys survive a tombstone",
			input: "a=1\nb=2\na=\n",
			want:  map[string]string{"b": "2"},
		},
		{
			name:  "blank lines are skipped",
			input: "a=1\n\nb=2\n",
			want:  map[string]string{"a": "1", "b": "2"},
		},
		{
			name:  "empty input yields empty state",
			input: "",
			want:  map[string]string{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			records, err := Replay(strings.NewReader(tc.input))
			if err != nil {
				t.Fatalf("Replay: %v", err)
			}
			state := Compact(records)
			if len(state) != len(tc.want) {
				t.Fatalf("Compact() = %v, want %v", state, tc.want)
			}
			for k, v := range tc.want {
				if state[k] != v {
					t.Errorf("state[%q] = %q, want %q", k, state[k], v)
				}
			}
		})
	}
}

func TestReplayMalformedLineReportsLineNumber(t *testing.T) {
	t.Parallel()

	_, err := Replay(strings.NewReader("a=1\nb=2\nbogus\n"))
	if !errors.Is(err, ErrMalformedLine) {
		t.Fatalf("Replay err = %v, want ErrMalformedLine", err)
	}
	if !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("Replay err = %v, want it to name line 3", err)
	}
}

// compactNaive is the compaction pass as it is often first written: instead
// of replaying the whole log once into a record map and pruning with a
// single maps.DeleteFunc pass, it rescans the growing event log every time
// it sees a tombstone, folding log[:len(log)-1] into a fresh state map. The
// bound is off by one: right after appending the tombstone that triggered
// the rescan, log[:len(log)-1] excludes that very entry, on the assumption
// that a *later* rescan will pick it up. That assumption holds for every
// tombstone except the last line of the file -- there is no later rescan --
// so a key whose final event is a tombstone is never actually applied, and
// its last set value reappears in the final state.
func compactNaive(lines []string) (map[string]string, error) {
	var log []Op
	var keys []string
	state := map[string]string{}
	for _, line := range lines {
		key, op, err := ParseLine(line)
		if err != nil {
			return nil, err
		}
		log = append(log, op)
		keys = append(keys, key)
		if !op.Tombstone {
			continue
		}
		state = map[string]string{}
		for i, o := range log[:len(log)-1] { // BUG: should be log[:len(log)]
			if o.Tombstone {
				delete(state, keys[i])
			} else {
				state[keys[i]] = o.Value
			}
		}
	}
	if n := len(log); n > 0 && !log[n-1].Tombstone {
		state[keys[n-1]] = log[n-1].Value
	}
	return state, nil
}

// TestCompactNaiveResurrectsTrailingTombstone is the heart of the module: a
// WAL that ends immediately after deleting a key -- an entirely ordinary
// compaction trigger -- is exactly the case compactNaive gets wrong.
func TestCompactNaiveResurrectsTrailingTombstone(t *testing.T) {
	t.Parallel()

	lines := []string{"k=v1", "k="}

	naive, err := compactNaive(lines)
	if err != nil {
		t.Fatalf("compactNaive: %v", err)
	}
	if v, present := naive["k"]; !present || v != "v1" {
		t.Fatalf("compactNaive[%q] = %q, present=%v; want the bug to resurrect %q=%q", "k", v, present, "k", "v1")
	}

	records, err := Replay(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	correct := Compact(records)
	if _, present := correct["k"]; present {
		t.Fatalf("Compact left %q present at %v, want it deleted", "k", correct)
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		args    []string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "sorted compacted state",
			input: "user:1=alice\nuser:2=bob\nuser:1=\nuser:3=carol\nuser:2=bobby\n",
			want:  "user:2=bobby\nuser:3=carol\n",
		},
		{
			name:  "empty input yields empty output",
			input: "",
			want:  "",
		},
		{
			name:    "malformed line is a usage error",
			input:   "bogus\n",
			wantErr: true,
		},
		{
			name:    "unexpected argument is a usage error",
			args:    []string{"extra"},
			input:   "",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.input), &stdout)
			if tc.wantErr {
				if !errors.Is(err, errUsage) {
					t.Fatalf("run() err = %v, want it to wrap errUsage", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("run(): %v", err)
			}
			if stdout.String() != tc.want {
				t.Fatalf("run() stdout = %q, want %q", stdout.String(), tc.want)
			}
		})
	}
}
```

## Review

The pipeline is correct when the final state holds exactly the latest write
per key, with tombstoned keys absent entirely — which is what `Replay`
folding every line into a record map, followed by one `Compact` pass of
`maps.DeleteFunc`, produces by construction: there is nothing to get wrong
about "when" to prune, because the prune runs exactly once, after every
write has already landed. The trap this module isolates is the tempting
alternative of pruning incrementally as tombstones arrive, which turns a
one-line traversal into a repeated rescan of a growing log and invites an
off-by-one in its bound — here, one that specifically drops a WAL's own
trailing tombstone. `run` maps a malformed WAL line to exit code 2 and
reserves exit code 1 for an I/O failure reading stdin. Run
`go test -count=1 -race ./...` to confirm the parsing table, the
replay/compact table, the `compactNaive` contrast, and `run` end to end.

## Resources

- [`maps.DeleteFunc`](https://pkg.go.dev/maps#DeleteFunc) — the in-place pruning pass this module's `Compact` relies on.
- [Kafka: Log Compaction](https://kafka.apache.org/documentation/#compaction) — the production feature this module models, including its tombstone-via-null-value convention.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — the line-oriented reader `Replay` uses to walk the WAL.
- [Go spec: for statements with range clause](https://go.dev/ref/spec#For_range) — deleting from a map during a range over that same map is explicitly permitted, which is what makes `DeleteFunc` sound.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-chunk-store-integrity-equalfunc.md](14-chunk-store-integrity-equalfunc.md) | Next: [16-protected-key-config-overlay.md](16-protected-key-config-overlay.md)

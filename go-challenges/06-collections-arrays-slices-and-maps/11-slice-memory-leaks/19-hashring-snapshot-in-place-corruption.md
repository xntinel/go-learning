# Exercise 19: A Consistent-Hashing Ring That Never Mutates a Live Snapshot

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Consistent hashing is how memcached clients shard keys across a pool of
servers and how Envoy's `ring_hash` load balancer picks an upstream: every
node claims several points ("vnodes") on a hash circle, and a key routes to
the node owning the next point clockwise from the key's own hash. Adding or
removing a node only remaps the keys between its neighboring points --
everyone else's assignment is undisturbed, which is the entire reason the
algorithm exists. Administering a ring like this -- rebalancing shards,
running a dry-run before a real deploy, auditing where a key currently
routes -- routinely calls for a stable snapshot of the ring's current
layout, taken once and read repeatedly while other calls continue to
mutate the live ring underneath.

That is where a mutation hazard hides that has nothing to do with hashing
itself. A `Snapshot` method that returns the ring's entries is, structurally,
exactly the pattern this whole lesson is about: a slice header aliasing the
producer's backing array. The ordinary way to remove one node's entries
from the middle of a slice -- filter in place, `out := s[:0]` followed by
appending survivors back into the same array -- is a correct, allocation-
light idiom on its own. It becomes a bug the instant something else is
still holding a `Snapshot` taken before the removal: that snapshot's slice
header never changes, same pointer, same length, but the *bytes* underneath
it have been rewritten, because the removal wrote its surviving entries
into the exact array the snapshot is still reading from.

This module builds `Ring` so that adding or removing a node always builds
a fresh backing array instead of mutating the current one. The version that
filters in place is not part of that API; it lives in the test file,
isolated as the thing the tests prove corrupts an outstanding snapshot.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
hashring/                 module example.com/hashring
  go.mod                  go 1.24
  ring.go                 Entry, Ring; NewRing, AddNode, RemoveNode, Lookup, Snapshot
  ring_test.go             construction/edge-error table, lookup validity,
                           the in-place-vs-fresh-array snapshot contrast, run() end to end
  main.go                 package main — add/remove/lookup commands, exit codes
```

- Files: `ring.go`, `ring_test.go`, `main.go`.
- Implement: `NewRing(maxNodes int) (*Ring, error)` rejecting a non-positive size with `ErrInvalidCapacity`; `(*Ring).AddNode(node string, vnodes int) error` rejecting a non-positive vnode count with `ErrInvalidVnodes` and too many distinct nodes with `ErrTooManyNodes`; `(*Ring).RemoveNode(node string) error` rejecting an absent node with `ErrNodeNotFound`; `(*Ring).Lookup(key string) (string, error)` rejecting an empty ring with `ErrEmptyRing`; `(*Ring).Snapshot() []Entry`. `AddNode` and `RemoveNode` always replace the ring's entries with a freshly built array.
- Tool: `hashring` reads `add NODE VNODES`, `remove NODE`, and `lookup KEY` commands from stdin, one status line per command to stdout. Exit 0 on success, exit 2 for a bad flag or a malformed or rejected command (all usage errors), exit 1 for a stdin read failure.
- Test: construction and edge-error coverage for every sentinel; `Lookup` returning a deterministic, valid node; the contrast between `removeNodeInPlace`, which corrupts an outstanding `Snapshot`, and the real `RemoveNode`, which never does; `run` end to end over its argument slice, a `strings.Reader`, and a `bytes.Buffer`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### A snapshot's header never changes; the array underneath can

`Snapshot() []Entry` returns a value that is, at the machine level, three
words: a pointer, a length, and a capacity. Once returned, that value is
immutable -- nothing can reach into a caller's local variable and change
its pointer or its length. What is *not* immutable is the array the pointer
points at, and Go gives no way to prevent writes to it except never sharing
it in the first place. A `RemoveNode` written the way most engineers first
write "delete matching elements from a slice" filters in place, reusing the
ring's own array to avoid an allocation:

```go
// ring.go -- the bug, if RemoveNode filtered in place.
func (r *Ring) removeNodeInPlace(node string) error {
    out := r.entries[:0]
    for _, e := range r.entries {
        if e.Node != node {
            out = append(out, e)
        }
    }
    r.entries = out
    return nil
}
```

Call `Snapshot` before this runs, then call this, and the snapshot's own
variable is untouched -- same pointer, same length, still perfectly valid
Go. But every element that `append` wrote inside `removeNodeInPlace` landed
in the same array the snapshot's pointer refers to, at the same indices the
snapshot reads from. `snap[0]` used to be entry A; after the removal, it
might be entry C, shifted down to fill the gap A's removal left. The
snapshot did not become stale in the usual sense (pointing at freed or
reused memory) -- it became *wrong*, silently, while remaining perfectly
type-safe and bounds-checked. The fix costs one extra allocation per
mutation: build the surviving entries into a brand-new slice, and only then
replace `r.entries` with it. Every `Snapshot` taken before that swap keeps
reading the array it always read, which nothing will ever write to again.

Create `ring.go`:

```go
// Command hashring implements a consistent-hashing ring (memcached client
// sharding, Envoy's ring_hash): each node owns several points ("vnodes")
// on a hash circle, and a key routes to the node owning the next point
// clockwise from its own hash. AddNode and RemoveNode always build a
// fresh array for the ring's entries rather than mutating the current one
// in place, which is what keeps Snapshot's result stable once taken. See
// the package tests for the corruption an in-place version would cause.
package main

import (
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
)

// Sentinel errors returned by NewRing, AddNode, RemoveNode, and Lookup.
// Test for them with errors.Is, not by comparing error strings.
var (
	ErrInvalidCapacity = errors.New("hashring: max nodes must be positive")
	ErrInvalidVnodes   = errors.New("hashring: vnodes must be positive")
	ErrTooManyNodes    = errors.New("hashring: adding node would exceed max nodes")
	ErrNodeNotFound    = errors.New("hashring: node not on ring")
	ErrEmptyRing       = errors.New("hashring: ring is empty")
)

// Entry is one vnode's position on the ring.
type Entry struct {
	Hash uint32
	Node string
}

// Ring is a consistent-hashing ring of vnode entries, kept sorted by Hash.
//
// Ring is not safe for concurrent use.
type Ring struct {
	entries []Entry
	max     int
}

// NewRing returns an empty Ring that refuses AddNode past maxNodes
// distinct nodes. It returns ErrInvalidCapacity if maxNodes <= 0.
func NewRing(maxNodes int) (*Ring, error) {
	if maxNodes <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidCapacity, maxNodes)
	}
	return &Ring{max: maxNodes}, nil
}

func hashOf(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

func (r *Ring) distinctNodes() map[string]bool {
	nodes := make(map[string]bool)
	for _, e := range r.entries {
		nodes[e.Node] = true
	}
	return nodes
}

// AddNode adds vnodes points for node (or more points for an existing
// node). It returns ErrInvalidVnodes or ErrTooManyNodes, and always
// replaces r.entries with a freshly built array; see the package doc.
func (r *Ring) AddNode(node string, vnodes int) error {
	if vnodes <= 0 {
		return fmt.Errorf("%w: got %d", ErrInvalidVnodes, vnodes)
	}
	nodes := r.distinctNodes()
	if !nodes[node] && len(nodes) >= r.max {
		return fmt.Errorf("%w: max %d", ErrTooManyNodes, r.max)
	}

	next := make([]Entry, 0, len(r.entries)+vnodes)
	next = append(next, r.entries...)
	for i := range vnodes {
		next = append(next, Entry{Hash: hashOf(fmt.Sprintf("%s#%d", node, i)), Node: node})
	}
	sort.Slice(next, func(i, j int) bool { return next[i].Hash < next[j].Hash })
	r.entries = next
	return nil
}

// RemoveNode removes every vnode belonging to node, replacing r.entries
// with a freshly built array. Returns ErrNodeNotFound if node is absent.
func (r *Ring) RemoveNode(node string) error {
	found := false
	next := make([]Entry, 0, len(r.entries))
	for _, e := range r.entries {
		if e.Node == node {
			found = true
			continue
		}
		next = append(next, e)
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrNodeNotFound, node)
	}
	r.entries = next
	return nil
}

// Lookup returns the node owning the next vnode clockwise from key's hash
// (wrapping around), or ErrEmptyRing if the ring has no nodes.
func (r *Ring) Lookup(key string) (string, error) {
	if len(r.entries) == 0 {
		return "", ErrEmptyRing
	}
	h := hashOf(key)
	idx := sort.Search(len(r.entries), func(i int) bool { return r.entries[i].Hash >= h })
	if idx == len(r.entries) {
		idx = 0
	}
	return r.entries[idx].Node, nil
}

// Snapshot returns the ring's entries, sorted by Hash, aliasing the Ring's
// storage as of this call; its content never changes afterward.
func (r *Ring) Snapshot() []Entry { return r.entries }
```

### The tool

`hashring` has no configuration beyond the maximum node count, so `run`
takes the argument slice plus an `io.Reader` for stdin and an `io.Writer`
for stdout -- nothing tied to `os.Stdin`/`os.Stdout` directly, which is
what lets the table test below drive it with a `strings.Reader` and a
`bytes.Buffer`. Input streams line by line through a `bufio.Scanner`. Every
failure `run` can produce -- a bad flag, a malformed command, a rejected
`AddNode`/`RemoveNode`/`Lookup` call -- is something the caller fixes by
changing the input, so all of them wrap `errUsage` via the small `usagef`
helper, and `main` maps that to exit code 2; a stdin read failure is the
one path that maps to exit code 1.

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
	"strconv"
	"strings"
)

// errUsage marks a failure fixable by changing the command line or input.
// main maps it to exit code 2; every other error maps to exit code 1.
var errUsage = errors.New("usage")

func usagef(line int, format string, args ...any) error {
	return fmt.Errorf("%w: line %d: %s", errUsage, line, fmt.Sprintf(format, args...))
}

// run parses args, then streams "add NODE VNODES", "remove NODE", and
// "lookup KEY" commands from stdin, one status line of output each. It
// never touches os.Stdin/os.Stdout/os.Exit, so a test can drive it with a
// strings.Reader and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("hashring", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	maxNodes := fs.Int("max-nodes", 100, "maximum distinct nodes the ring accepts")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	r, err := NewRing(*maxNodes)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	scanner := bufio.NewScanner(stdin)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		fields := strings.Fields(text)
		switch fields[0] {
		case "add":
			if len(fields) != 3 {
				return usagef(line, "add requires NODE VNODES")
			}
			vnodes, err := strconv.Atoi(fields[2])
			if err != nil {
				return usagef(line, "invalid vnode count %q", fields[2])
			}
			if err := r.AddNode(fields[1], vnodes); err != nil {
				return usagef(line, "%v", err)
			}
			fmt.Fprintf(stdout, "added %s (+%d vnodes, ring size=%d)\n", fields[1], vnodes, len(r.Snapshot()))
		case "remove":
			if len(fields) != 2 {
				return usagef(line, "remove requires NODE")
			}
			if err := r.RemoveNode(fields[1]); err != nil {
				return usagef(line, "%v", err)
			}
			fmt.Fprintf(stdout, "removed %s (ring size=%d)\n", fields[1], len(r.Snapshot()))
		case "lookup":
			if len(fields) != 2 {
				return usagef(line, "lookup requires KEY")
			}
			node, err := r.Lookup(fields[1])
			if err != nil {
				return usagef(line, "%v", err)
			}
			fmt.Fprintf(stdout, "%s -> %s\n", fields[1], node)
		default:
			return usagef(line, "unknown command %q", fields[0])
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: hashring [-max-nodes N] < commands")
		fmt.Fprintln(os.Stderr, "reads 'add NODE VNODES', 'remove NODE', and 'lookup KEY' lines.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "hashring:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'add web-1 3\nadd web-2 3\nadd web-3 3\nlookup user:14\nlookup user:42\nremove web-2\nlookup user:14\nlookup user:42\n' | go run .
printf 'lookup k\n' | go run .
```

Expected output:

```text
added web-1 (+3 vnodes, ring size=3)
added web-2 (+3 vnodes, ring size=6)
added web-3 (+3 vnodes, ring size=9)
user:14 -> web-2
user:42 -> web-1
removed web-2 (ring size=6)
user:14 -> web-1
user:42 -> web-1
```

```text
hashring: usage: line 1: hashring: ring is empty
```

The first run is consistent hashing's whole point, visible in eight lines:
`user:14` moves from `web-2` to `web-1` once `web-2` leaves the ring,
because `user:14`'s hash fell in the arc `web-2` used to own -- but
`user:42`, which was never routed to `web-2`, keeps its assignment across
the removal untouched. The second run shows the exit-2 usage error: an
empty ring rejects `Lookup` with `ErrEmptyRing`, wrapped by `errUsage`.

### Tests

`TestConstructionAndEdgeErrors` walks every sentinel this package defines
in one pass: a non-positive `NewRing` capacity, a non-positive vnode count,
adding past the configured node limit, removing an absent node, and
looking up against an empty ring -- each checked with `errors.Is` via the
small `wantErrIs` helper. `TestLookupIsDeterministicAndValid` builds a
three-node ring and asserts that repeated lookups of the same key agree
with each other and always name a node actually on the ring.

`TestRemoveNodeSnapshotContrast` is the module's center of gravity, as two
subtests sharing one setup. `removeNodeInPlace` is unexported and
unreachable from the package API; it is `RemoveNode` with the fresh-array
allocation deleted, filtering `r.entries` in place instead. The first
subtest takes a `Snapshot`, clones it for comparison, runs the in-place
removal, and asserts the snapshot's content is no longer equal to what it
cloned -- proving the corruption. The second subtest runs the identical
sequence through the real `RemoveNode` and asserts the opposite: the
snapshot is byte-for-byte what it was before, because `RemoveNode` never
touched the array the snapshot's header points at. `TestRun` drives the
command end to end: a successful add/remove/lookup session against its
exact expected stdout, and four ways the input can be rejected, each
asserted to wrap `errUsage`.

Create `ring_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func wantErrIs(t *testing.T, err, want error, ctx string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: error = %v, want %v", ctx, err, want)
	}
}

func TestConstructionAndEdgeErrors(t *testing.T) {
	t.Parallel()
	for _, max := range []int{0, -1} {
		_, err := NewRing(max)
		wantErrIs(t, err, ErrInvalidCapacity, "NewRing")
	}

	r, err := NewRing(2)
	must(t, err)
	wantErrIs(t, r.AddNode("a", 0), ErrInvalidVnodes, "AddNode(vnodes=0)")
	must(t, r.AddNode("a", 2))
	must(t, r.AddNode("b", 2))
	wantErrIs(t, r.AddNode("c", 1), ErrTooManyNodes, "AddNode(past max)")
	wantErrIs(t, r.RemoveNode("ghost"), ErrNodeNotFound, "RemoveNode(unknown)")

	empty, err := NewRing(1)
	must(t, err)
	_, err = empty.Lookup("k")
	wantErrIs(t, err, ErrEmptyRing, "Lookup(empty ring)")
}

func TestLookupIsDeterministicAndValid(t *testing.T) {
	t.Parallel()
	r := buildThreeNodeRing(t)
	valid := map[string]bool{"a": true, "b": true, "c": true}

	for _, key := range []string{"k1", "k2", "k3", "user:42"} {
		got, err := r.Lookup(key)
		must(t, err)
		if again, _ := r.Lookup(key); again != got {
			t.Fatalf("Lookup(%q) not deterministic: %q vs %q", key, got, again)
		}
		if !valid[got] {
			t.Fatalf("Lookup(%q) = %q, not a node on the ring", key, got)
		}
	}
}

func buildThreeNodeRing(t *testing.T) *Ring {
	t.Helper()
	r, err := NewRing(10)
	must(t, err)
	must(t, r.AddNode("a", 2))
	must(t, r.AddNode("b", 2))
	must(t, r.AddNode("c", 2))
	return r
}

// removeNodeInPlace is RemoveNode with the fix removed: it filters the
// ring's array in place instead of building a fresh one. Never exported or
// reachable from the API; it exists to pin the corruption it causes.
func removeNodeInPlace(r *Ring, node string) error {
	found := false
	out := r.entries[:0]
	for _, e := range r.entries {
		if e.Node == node {
			found = true
			continue
		}
		out = append(out, e)
	}
	if !found {
		return ErrNodeNotFound
	}
	r.entries = out
	return nil
}

// TestRemoveNodeSnapshotContrast is the heart of the module: an in-place
// removal changes what an outstanding Snapshot reads even though its
// header never changes; the real RemoveNode leaves it untouched.
func TestRemoveNodeSnapshotContrast(t *testing.T) {
	t.Parallel()
	t.Run("in-place removal corrupts it", func(t *testing.T) {
		t.Parallel()
		r := buildThreeNodeRing(t)
		snap := r.Snapshot()
		before := slices.Clone(snap)
		must(t, removeNodeInPlace(r, "b"))
		if slices.Equal(snap, before) {
			t.Fatal("expected the in-place removal to corrupt the snapshot, but it stayed identical")
		}
	})

	t.Run("RemoveNode preserves it", func(t *testing.T) {
		t.Parallel()
		r := buildThreeNodeRing(t)
		snap := r.Snapshot()
		before := slices.Clone(snap)
		must(t, r.RemoveNode("b"))
		if !slices.Equal(snap, before) {
			t.Fatalf("outstanding snapshot changed after RemoveNode: got %v, want %v", snap, before)
		}
	})
}

func TestRun(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		args    []string
		in      string
		want    string
		wantErr bool
	}{
		{name: "add and remove report ring size", in: "add a 2\nadd b 2\nremove a\nlookup k\n", want: "added a (+2 vnodes, ring size=2)\nadded b (+2 vnodes, ring size=4)\nremoved a (ring size=2)\nk -> b\n"},
		{name: "unknown command is a usage error", in: "wat\n", wantErr: true},
		{name: "lookup on empty ring is a usage error", in: "lookup k\n", wantErr: true},
		{name: "non-numeric vnodes is a usage error", in: "add a banana\n", wantErr: true},
		{name: "bad flag is a usage error", args: []string{"-max-nodes=0"}, in: "add a 1\n", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.in), &stdout)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("run(%v): want error, got nil", tc.args)
				}
				if !errors.Is(err, errUsage) {
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
```

## Review

`RemoveNode` and `AddNode` are correct when a `Snapshot` taken before
either call reads exactly what it read at the moment it was taken, no
matter what the `Ring` does afterward -- and that holds precisely because
both methods always replace `r.entries` with a freshly built array rather
than writing into the current one. `NewRing` rejects a non-positive
capacity with `ErrInvalidCapacity`; `AddNode` rejects a non-positive vnode
count with `ErrInvalidVnodes` and too many distinct nodes with
`ErrTooManyNodes`; `RemoveNode` rejects an absent node with
`ErrNodeNotFound`; `Lookup` rejects an empty ring with `ErrEmptyRing` --
all checkable with `errors.Is`. The in-place version that corrupts a
snapshot is confined to the test file as `removeNodeInPlace`, never offered
as a mode on `Ring` -- the only way to reproduce the corruption is to
delete the one allocation that prevents it, which is exactly what the
contrast test does to prove why it matters. The tool's exit codes follow
the same split as every other module in this lesson: a bad flag or a
malformed or rejected command is something the caller fixes by changing
the input, exit 2; a stdin read failure is exit 1. Run `go test -count=1
-race ./...`.

## Resources

- [Consistent hashing (Wikipedia)](https://en.wikipedia.org/wiki/Consistent_hashing) — the algorithm and the minimal-disruption property the "Run it" output demonstrates.
- [Envoy: ring_hash load balancer](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/load_balancing/load_balancers#ring-hash) — a real proxy using this exact vnode-ring shape for upstream selection.
- [Go Spec: Slice expressions](https://go.dev/ref/spec#Slice_expressions) — why a slice header's own fields (pointer, length, capacity) are immutable once returned, even though the array they point at is not.
- [`slices.Clone`](https://pkg.go.dev/slices#Clone) and [`slices.Equal`](https://pkg.go.dev/slices#Equal) — the two functions the contrast tests use to pin the corruption and its absence.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-dispatch-context-cancel-goroutine-leak.md](18-dispatch-context-cancel-goroutine-leak.md) | Next: [../12-sorted-collections-binary-search/00-concepts.md](../12-sorted-collections-binary-search/00-concepts.md)

# Exercise 20: All-Pairs Hop Counts Over a True 2D [N][N]int Adjacency Matrix

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A small service mesh's reachability graph is naturally an adjacency matrix,
and Floyd-Warshall is the standard way to compute every pair's shortest
path (here, fewest hops) in one pass: for each candidate intermediate
service `k`, check whether routing through `k` beats the current best known
route from `i` to `j`. The interesting Go decision is the matrix's type.
`[N][N]int` — an array of arrays — is one contiguous block of `N*N` ints,
copied element-for-element on every assignment, exactly like any other
array. `[][]int` — a slice of slices — is `N` independently allocated rows;
copying the outer slice only copies `N` row pointers, and every "copy" still
shares the same underlying row data. For a small, fixed mesh size known at
compile time, `[N][N]int` gives true value semantics: assigning a matrix
snapshot is a real, independent copy, which matters the moment you want to
run an in-place algorithm on one snapshot without corrupting another.

This exercise builds `hops`, a command that reads directed edges as lines on
stdin, runs Floyd-Warshall over a `[N][N]int` in place, and prints every
reachable pair's hop count. A test proves that assigning the matrix never
aliases.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
hopmatrix/                     module example.com/hopmatrix
  go.mod                       go 1.24
  hopmatrix.go                 package main — Matrix [N][N]int; NewMatrix; SetEdge; FloydWarshall (in place); Reachable; ErrIndexRange
  hopmatrix_test.go             package main — hop-count table, unreachable-pair detection, assignment non-aliasing, diagonal init, run() end to end
  main.go                      package main — reads "from to weight" edges from stdin, prints reachable pairs
```

- Files: `hopmatrix.go`, `hopmatrix_test.go`, `main.go`.
- Implement: `Matrix [N][N]int` (N=6); `NewMatrix() Matrix` initializing the diagonal to 0 and everything else to `Unreachable`; `func (m *Matrix) SetEdge(i, j, weight int) error` returning `ErrIndexRange` for an out-of-range index; `func (m *Matrix) FloydWarshall()` running the classic triple-nested relaxation in place; `func (m *Matrix) Reachable(i, j int) bool`.
- Tool: `hops` streams stdin line by line, parsing each non-blank line as three whitespace-separated integers `from to weight`. It takes no arguments — only stdin. Exit 0 on success, exit 2 for a bad flag, an unexpected argument, a malformed line, or an out-of-range node index (all naming the offending line number), exit 1 for a stdin read failure.
- Test: a table of hop counts for specific `(from, to)` pairs on a hand-built 6-service DAG-shaped mesh, checked against manually worked-out shortest paths; an isolated service is unreachable in both directions from every other service; `SetEdge` rejects an out-of-range index with `ErrIndexRange`; plain assignment of a `Matrix` produces an independent copy, verified by mutating the copy and asserting the original is unaffected, including after running `FloydWarshall` on only one of the two; `NewMatrix`'s diagonal-zero, everything-else-`Unreachable` initialization; `run` end to end over `strings.Reader` and `bytes.Buffer`, including a malformed line and an out-of-range node.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/hopmatrix
cd ~/go-exercises/hopmatrix
go mod init example.com/hopmatrix
go mod edit -go=1.24
```

### Why [N][N]int copies by value and [][]int does not

`Matrix` is defined as `[N][N]int` — an array whose element type is itself
an array. Go arrays nest without any special mechanism: `[N][N]int` is just
`[N]([N]int)`, `N` copies of an `[N]int`, laid out contiguously in memory. The
consequence that matters here is what assignment does: `b := a` for two
`Matrix` values copies every one of the `N*N` ints, because that is what
array assignment always does, recursively, all the way down. There is no
row-pointer indirection anywhere in a `[N][N]int` — there are no pointers in
it at all.

Compare that to `[][]int`, the shape you would reach for if `N` were only
known at runtime. There, the outer value is a slice header (pointer, length,
capacity) over `N` row *slices*, each of which is itself a header over its
own backing array. `b := a` for a `[][]int` copies the outer header — three
words — and both `a` and `b` now point at the exact same `N` row headers.
Mutating `b[2][3]` also changes what `a[2][3]` reports, because `a[2]` and
`b[2]` are two names for the same row slice pointing at the same backing
array. A `[][]int` "copy" is not a copy of the matrix at all; it is a second
handle onto the same one. Getting a truly independent 2D snapshot out of
`[][]int` requires manually allocating and copying every row — exactly the
work `[N][N]int` assignment does for free, and exactly the work
`TestAssignmentDoesNotAlias` in this module checks actually happens.

That non-aliasing property is what makes it safe to take a `snapshot := *m`
before mutating one copy in place — `FloydWarshall` writes directly into the
matrix it is called on (a pointer receiver, needed because `Matrix` is large
enough — 36 ints for N=6, but the same reasoning holds at any realistic mesh
size — that a value receiver would silently discard the in-place updates,
the same trap a 1D fixed array falls into under a value-receiver method).
Running `FloydWarshall` on a snapshot must never perturb the original
matrix still being read elsewhere; with `[N][N]int` that is guaranteed by
the type, not by discipline.

`Unreachable` is set to `1<<30`, far below `math.MaxInt`, specifically so
that summing two `Unreachable`-adjacent distances during relaxation
(`m[i][k] + m[k][j]`) cannot silently wrap into a small, wrongly "reachable"
number — the `if m[i][k] >= Unreachable { continue }` guards make sure that
sum is never even computed when either leg is unknown, but the safety
margin is cheap insurance against the same class of bug in any future edit.

Create `hopmatrix.go`:

```go
// Command hops reads directed edges as lines on stdin ("from to weight"),
// runs Floyd-Warshall over a fixed N x N adjacency matrix, and prints every
// reachable pair's hop count.
package main

import (
	"errors"
	"fmt"
)

// N is the number of services in the mesh. It is fixed at compile time
// because the matrix is a true 2D array, whose dimensions -- like an
// array's length -- must be constants.
const N = 6

// ErrIndexRange is returned by SetEdge when either index falls outside
// [0, N).
var ErrIndexRange = errors.New("hopmatrix: index out of range")

// Unreachable marks the absence of a known path, deliberately far below the
// maximum int so summing two Unreachable-adjacent values can never silently
// overflow into a small, wrongly "reachable" number.
const Unreachable = 1 << 30

// Matrix is an N x N hop-count adjacency matrix: Matrix[i][j] is the number
// of hops from service i to service j, or Unreachable if no path is known.
// Because it is a true [N][N]int, not a slice of slices, plain assignment
// copies all N*N ints: two mesh snapshots are guaranteed independent.
type Matrix [N][N]int

// NewMatrix returns a Matrix with every entry initialized to Unreachable,
// except the diagonal, which is 0 (every service reaches itself in zero
// hops).
func NewMatrix() Matrix {
	var m Matrix
	for i := 0; i < N; i++ {
		for j := 0; j < N; j++ {
			if i == j {
				m[i][j] = 0
			} else {
				m[i][j] = Unreachable
			}
		}
	}
	return m
}

// SetEdge records a direct hop of the given weight from i to j. The
// receiver is a pointer because Matrix is large (N*N ints): a value
// receiver would mutate a full copy and the caller would see no change.
func (m *Matrix) SetEdge(i, j, weight int) error {
	if i < 0 || i >= N || j < 0 || j >= N {
		return fmt.Errorf("%w: (%d,%d) not in [0,%d)", ErrIndexRange, i, j, N)
	}
	m[i][j] = weight
	return nil
}

// FloydWarshall runs the classic all-pairs relaxation in place over m: for
// every candidate intermediate k, it checks whether i -> k -> j beats the
// known i -> j hop count, and updates m[i][j] if so. After it returns,
// m[i][j] holds the minimum hop count from i to j, or stays Unreachable.
func (m *Matrix) FloydWarshall() {
	for k := 0; k < N; k++ {
		for i := 0; i < N; i++ {
			if m[i][k] >= Unreachable {
				continue
			}
			for j := 0; j < N; j++ {
				if m[k][j] >= Unreachable {
					continue
				}
				if d := m[i][k] + m[k][j]; d < m[i][j] {
					m[i][j] = d
				}
			}
		}
	}
}

// Reachable reports whether service i can reach service j after
// FloydWarshall has been run.
func (m *Matrix) Reachable(i, j int) bool {
	return m[i][j] < Unreachable
}
```

### The tool

`hops` streams stdin with `bufio.Scanner`, one edge per line, so a large
edge list never needs to be buffered whole before the matrix can start
filling in — a natural fit given the matrix itself is already a fixed,
bounded `[N][N]int` regardless of how many edge lines arrive. `run` takes
`args`, an `io.Reader` for stdin, and an `io.Writer` for stdout, so tests
drive it with `strings.Reader` and `bytes.Buffer`. Every parse failure — the
wrong field count, a non-integer field, an out-of-range node index from
`SetEdge` — is reported with the 1-based line number and wraps `errUsage`,
mapped to exit code 2; a genuine stdin read failure maps to exit code 1.
Blank lines are skipped so a trailing newline in a file never becomes a
spurious error.

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

// errUsage marks a bad flag, a malformed edge line, or a node index outside
// [0,N). main maps it to exit code 2; a stdin read failure maps to 1.
var errUsage = errors.New("usage")

// run reads one directed edge per stdin line as "from to weight", builds
// the N x N matrix, runs Floyd-Warshall, and writes every reachable pair's
// hop count to stdout. Blank lines are skipped.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("hops", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: hops takes no arguments, only stdin", errUsage)
	}

	m := NewMatrix()
	scanner := bufio.NewScanner(stdin)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 3 {
			return fmt.Errorf("%w: line %d: want \"from to weight\", got %q", errUsage, line, text)
		}
		from, err1 := strconv.Atoi(fields[0])
		to, err2 := strconv.Atoi(fields[1])
		weight, err3 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil || err3 != nil {
			return fmt.Errorf("%w: line %d: %q is not three integers", errUsage, line, text)
		}
		if err := m.SetEdge(from, to, weight); err != nil {
			return fmt.Errorf("%w: line %d: %v", errUsage, line, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}

	m.FloydWarshall()
	for i := 0; i < N; i++ {
		for j := 0; j < N; j++ {
			if i == j || !m.Reachable(i, j) {
				continue
			}
			fmt.Fprintf(stdout, "%d -> %d: %d hop(s)\n", i, j, m[i][j])
		}
	}
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: hops < edges.txt")
		fmt.Fprintln(os.Stderr, "reads \"from to weight\" edges (node indices 0..5) from stdin,")
		fmt.Fprintln(os.Stderr, "prints every reachable pair's shortest hop count.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "hops:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf '0 1 1\n0 2 1\n1 2 1\n1 3 1\n2 4 1\n3 4 1\n' | go run .
```

Expected output:

```text
0 -> 1: 1 hop(s)
0 -> 2: 1 hop(s)
0 -> 3: 2 hop(s)
0 -> 4: 2 hop(s)
1 -> 2: 1 hop(s)
1 -> 3: 1 hop(s)
1 -> 4: 2 hop(s)
2 -> 4: 1 hop(s)
3 -> 4: 1 hop(s)
```

The mesh here is `0=gateway, 1=auth, 2=orders, 3=inventory, 4=payments,
5=legacy-billing`; every edge points strictly forward toward payments, and
service 5 has none at all, so no line involves it and `payments -> gateway`
is correctly absent. Feeding a malformed line instead — `printf '0 1
1\nbogus\n' | go run .` — prints `hops: usage: line 2: want "from to
weight", got "bogus"` to stderr and exits 2, naming the exact line the
parser rejected.

### Tests

`TestFloydWarshallHopCounts` is a table over specific `(from, to)` pairs on
a hand-built 6-service mesh, each expected hop count worked out by hand from
the direct edges, run as parallel subtests. `TestUnreachablePairsDetected`
checks the isolated service in both directions and checks that a one-way
edge does not imply reachability in reverse. `TestSetEdgeRejectsOutOfRange`
asserts `SetEdge` returns `ErrIndexRange`, checkable with `errors.Is`,
instead of panicking on a bad index. `TestAssignmentDoesNotAlias` is the
test this exercise is built around: it takes a plain-assignment snapshot,
mutates only the snapshot (first with `SetEdge`, then by running
`FloydWarshall` on it), and asserts the original matrix is untouched by
either mutation. `TestNewMatrixDiagonalIsZero` pins the base-case
initialization every other test implicitly depends on. `TestRun` drives the
command end to end over the same mesh fed as stdin lines, plus
`TestRunRejectsMalformedLine` and `TestRunRejectsOutOfRangeNode` for the two
usage-error paths.

Create `hopmatrix_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// buildMesh builds a small DAG-shaped mesh: 0=gateway, 1=auth, 2=orders,
// 3=inventory, 4=payments, 5=legacy-billing (deliberately unconnected, to
// exercise unreachable-pair detection).
func buildMesh(t *testing.T) Matrix {
	t.Helper()
	m := NewMatrix()
	edges := [][3]int{
		{0, 1, 1}, // gateway -> auth
		{0, 2, 1}, // gateway -> orders
		{1, 2, 1}, // auth -> orders
		{1, 3, 1}, // auth -> inventory
		{2, 4, 1}, // orders -> payments
		{3, 4, 1}, // inventory -> payments
	}
	for _, e := range edges {
		if err := m.SetEdge(e[0], e[1], e[2]); err != nil {
			t.Fatalf("SetEdge%v: %v", e, err)
		}
	}
	return m
}

// TestFloydWarshallHopCounts is a table over specific (from, to) pairs in
// the mesh built by buildMesh, checked against hop counts worked out by hand
// from the direct edges above.
func TestFloydWarshallHopCounts(t *testing.T) {
	t.Parallel()

	m := buildMesh(t)
	m.FloydWarshall()

	cases := []struct {
		from, to int
		want     int
	}{
		{0, 0, 0},
		{0, 1, 1},
		{0, 2, 1}, // direct edge beats the 2-hop 0->1->2 path
		{0, 3, 2}, // 0 -> 1 -> 3
		{0, 4, 2}, // 0 -> 2 -> 4 beats 0 -> 1 -> 3 -> 4
		{1, 4, 2}, // 1 -> 2 -> 4 (or 1 -> 3 -> 4, same length)
		{2, 4, 1},
		{3, 4, 1},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d-to-%d", tc.from, tc.to), func(t *testing.T) {
			t.Parallel()
			got := m[tc.from][tc.to]
			if got != tc.want {
				t.Errorf("hops[%d][%d] = %d, want %d", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

// TestUnreachablePairsDetected asserts that a service with no outbound or
// inbound edges (5) is reported unreachable from and to every other
// service, and that the reverse direction of a one-way edge is also
// correctly unreachable (this mesh's edges are all one-directional).
func TestUnreachablePairsDetected(t *testing.T) {
	t.Parallel()

	m := buildMesh(t)
	m.FloydWarshall()

	for i := 0; i < N; i++ {
		if i == 5 {
			continue
		}
		if m.Reachable(i, 5) {
			t.Errorf("service %d must not reach isolated service 5", i)
		}
		if m.Reachable(5, i) {
			t.Errorf("isolated service 5 must not reach service %d", i)
		}
	}

	// The mesh has no path back from payments (4) to gateway (0): every
	// edge points forward.
	if m.Reachable(4, 0) {
		t.Error("service 4 must not reach service 0 (all edges point forward)")
	}
	if !m.Reachable(0, 4) {
		t.Error("service 0 must reach service 4")
	}
}

// TestSetEdgeRejectsOutOfRange asserts SetEdge validates both indices
// instead of panicking or silently corrupting an adjacent row.
func TestSetEdgeRejectsOutOfRange(t *testing.T) {
	t.Parallel()

	m := NewMatrix()
	for _, e := range [][2]int{{-1, 0}, {0, N}, {N, N}} {
		if err := m.SetEdge(e[0], e[1], 1); !errors.Is(err, ErrIndexRange) {
			t.Fatalf("SetEdge(%d,%d) err = %v, want ErrIndexRange", e[0], e[1], err)
		}
	}
}

// TestAssignmentDoesNotAlias proves that Matrix, a true [N][N]int array, is
// copied element-for-element on plain assignment. This is the property that
// distinguishes it from [][]int, where assigning the outer slice only
// copies row pointers and every "copy" still shares the same row storage.
func TestAssignmentDoesNotAlias(t *testing.T) {
	t.Parallel()

	original := buildMesh(t)

	// Plain assignment: snapshot is a full, independent copy.
	snapshot := original
	if err := snapshot.SetEdge(4, 0, 1); err != nil { // mutate only the snapshot
		t.Fatalf("SetEdge: %v", err)
	}

	if original.Reachable(4, 0) {
		t.Fatal("mutating snapshot must not affect original: Matrix assignment must deep-copy")
	}
	if !snapshot.Reachable(4, 0) {
		t.Fatal("the snapshot's own mutation must be visible on the snapshot")
	}

	// Running FloydWarshall on one copy must not touch the other.
	snapshot.FloydWarshall()
	if original.Reachable(3, 0) {
		t.Fatal("FloydWarshall on snapshot must not affect original")
	}
}

// TestNewMatrixDiagonalIsZero asserts every service reaches itself in zero
// hops and every off-diagonal entry starts Unreachable.
func TestNewMatrixDiagonalIsZero(t *testing.T) {
	t.Parallel()

	m := NewMatrix()
	for i := 0; i < N; i++ {
		for j := 0; j < N; j++ {
			want := Unreachable
			if i == j {
				want = 0
			}
			if m[i][j] != want {
				t.Fatalf("NewMatrix()[%d][%d] = %d, want %d", i, j, m[i][j], want)
			}
		}
	}
}

// TestRun exercises the command end to end over strings.Reader and
// bytes.Buffer: the same mesh as buildMesh, fed as edge lines, must produce
// the same reachable pairs FloydWarshall computes directly.
func TestRun(t *testing.T) {
	t.Parallel()

	stdin := "0 1 1\n0 2 1\n1 2 1\n1 3 1\n2 4 1\n3 4 1\n\n"

	var stdout bytes.Buffer
	if err := run(nil, strings.NewReader(stdin), &stdout); err != nil {
		t.Fatalf("run: %v", err)
	}

	want := "0 -> 1: 1 hop(s)\n" +
		"0 -> 2: 1 hop(s)\n" +
		"0 -> 3: 2 hop(s)\n" +
		"0 -> 4: 2 hop(s)\n" +
		"1 -> 2: 1 hop(s)\n" +
		"1 -> 3: 1 hop(s)\n" +
		"1 -> 4: 2 hop(s)\n" +
		"2 -> 4: 1 hop(s)\n" +
		"3 -> 4: 1 hop(s)\n"
	if stdout.String() != want {
		t.Fatalf("run stdout =\n%s\nwant\n%s", stdout.String(), want)
	}
}

// TestRunRejectsMalformedLine asserts a line that is not exactly three
// integers is a usage error naming the offending line number.
func TestRunRejectsMalformedLine(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	err := run(nil, strings.NewReader("0 1 1\nbogus\n"), &stdout)
	if err == nil {
		t.Fatal("run: want error for malformed line, got nil")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("run error = %v, want it to name line 2", err)
	}
}

// TestRunRejectsOutOfRangeNode asserts a node index outside [0,N) is a
// usage error, not a panic.
func TestRunRejectsOutOfRangeNode(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	err := run(nil, strings.NewReader("0 99 1\n"), &stdout)
	if err == nil {
		t.Fatal("run: want error for out-of-range node, got nil")
	}
}
```

## Review

`FloydWarshall` is correct when, after it returns, `m[i][j]` holds the
minimum hop count over any path from `i` to `j`, or remains `Unreachable`
when no path exists — and `TestFloydWarshallHopCounts` checks that against
hand-derived expectations rather than trusting the algorithm's own output.
The property unique to this exercise is the aliasing guarantee:
`TestAssignmentDoesNotAlias` is the test that would fail immediately if
`Matrix` were ever redefined as `[][]int` (or `[N][]int`, the half-measure
that still shares row backing) without updating every call site that relies
on `snapshot := original` producing an independent copy — a refactor like
that would compile cleanly and only break at run time, invisibly, wherever
someone assumed the old value semantics still held. `SetEdge` returns
`ErrIndexRange` instead of letting a bad index panic or corrupt a
neighboring row, and `hops` maps every parse failure to exit code 2 with
the offending line number attached, reserving exit 1 for a genuine stdin
failure. Run `go test -count=1 -race ./...` to confirm the hop-count table,
the unreachable-pair detection, the non-aliasing guarantee, the diagonal
initialization, and `run`'s end-to-end behavior.

## Resources

- [Go Specification: Array types](https://go.dev/ref/spec#Array_types) — array-of-array nesting and why `[N][N]T` has no pointer indirection anywhere in it.
- [Floyd-Warshall algorithm (Wikipedia)](https://en.wikipedia.org/wiki/Floyd%E2%80%93Warshall_algorithm) — the all-pairs shortest-path algorithm this module implements in place.
- [Go Specification: Assignability](https://go.dev/ref/spec#Assignability) — why `b := a` for an array type `a` copies the full value, unlike the same assignment for a slice.
- [bufio.Scanner](https://pkg.go.dev/bufio#Scanner) — the line-at-a-time stdin reader `hops` uses to parse edges.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-bitset-array-value-copy-trap.md](19-bitset-array-value-copy-trap.md) | Next: [../02-slices-creation-append-capacity/00-concepts.md](../02-slices-creation-append-capacity/00-concepts.md)

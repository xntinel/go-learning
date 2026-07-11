# Exercise 6: Turn A Production Crash Into A testdata Regression Seed

The operational payoff of fuzzing is not the crash it finds — it is the committed
file that keeps the crash from ever coming back. This module walks the full loop
on a deliberately-buggy path normalizer: fuzz it until it panics, capture the
minimized input under `testdata/fuzz`, fix the bug, and watch *plain* `go test`
replay that committed file as a permanent regression. The lesson is that fuzz
corpus files are version-controlled test assets, not scratch.

This module is fully self-contained: its own `go mod init`, demo, tests, and a
committed corpus file.

## What you'll build

```text
pathnorm/                          independent module: example.com/pathnorm
  go.mod                           module path
  normalize.go                     Normalize(string) string — collapses '//' runs
  cmd/
    demo/
      main.go                      normalize a few messy paths
  normalize_test.go                TestNormalizeTable, FuzzNormalize, Example
  testdata/
    fuzz/
      FuzzNormalize/
        slash_panic                the committed, minimized crasher (replayed by go test)
```

Files: `normalize.go`, `cmd/demo/main.go`, `normalize_test.go`,
`testdata/fuzz/FuzzNormalize/slash_panic`.
Implement: `Normalize(path string) string`, the *fixed* version.
Test: a table test; `FuzzNormalize` asserting no panic and no `//` in the output;
a committed corpus file replayed under plain `go test`.
Verify: `go test -race ./...` (replays the corpus), then
`go test -fuzz=FuzzNormalize -fuzztime=2s`.

Set up the module:

```bash
mkdir -p ~/go-exercises/pathnorm/cmd/demo
mkdir -p ~/go-exercises/pathnorm/testdata/fuzz/FuzzNormalize
cd ~/go-exercises/pathnorm
go mod init example.com/pathnorm
```

### The bug, the crash, and the loop

`Normalize` collapses runs of consecutive `/` into a single `/`, the way a router
tidies a request path before matching. The first, buggy version looks past the
current byte to peek at the next one:

```go
// BUGGY — do not commit this. It reads path[i+1] without a bounds check.
func Normalize(path string) string {
	out := make([]byte, 0, len(path))
	for i := 0; i < len(path); i++ {
		if path[i] == '/' && path[i+1] == '/' { // panics when i is the last index
			continue
		}
		out = append(out, path[i])
	}
	return string(out)
}
```

Every example test the author wrote used a path that did *not* end in `/`, so the
`path[i+1]` access was always in range and the tests were green. Fuzzing finds the
gap immediately. Run `go test -fuzz=FuzzNormalize -fuzztime=10s` against the buggy
version and the engine reports a panic and minimizes the trigger to the smallest
input that still crashes — a single `"/"`. It writes that reproducer to
`testdata/fuzz/FuzzNormalize/<hash>`, a file in the `go test fuzz v1` format:

```text
go test fuzz v1
string("/")
```

You commit that file. Then you fix the bug with a bounds check, and — this is the
whole point — plain `go test` with no `-fuzz` flag now replays the committed file
as an ordinary sub-test on every build. A fixed bug that a committed corpus entry
guards cannot silently return; a future refactor that reintroduces the
out-of-range read fails CI the moment it lands. Below is the *fixed* `Normalize`
and the committed corpus file, named readably here (`slash_panic`) — Go replays
every file in the corpus directory regardless of its name.

Create `normalize.go` (the fixed version):

```go
package pathnorm

// Normalize collapses runs of consecutive '/' in a URL path into a single '/'.
// The bounds check (i+1 < len(path)) is the fix for the original crash on a path
// ending in '/'.
func Normalize(path string) string {
	if path == "" {
		return "/"
	}
	out := make([]byte, 0, len(path))
	for i := 0; i < len(path); i++ {
		if path[i] == '/' && i+1 < len(path) && path[i+1] == '/' {
			continue
		}
		out = append(out, path[i])
	}
	return string(out)
}
```

Create `testdata/fuzz/FuzzNormalize/slash_panic` (the committed regression seed):

```
go test fuzz v1
string("/")
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pathnorm"
)

func main() {
	for _, p := range []string{"/a//b///c", "/", "", "no/leading/slash", "trailing//"} {
		fmt.Printf("%-18q -> %q\n", p, pathnorm.Normalize(p))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"/a//b///c"        -> "/a/b/c"
"/"                -> "/"
""                 -> "/"
"no/leading/slash" -> "no/leading/slash"
"trailing//"       -> "trailing/"
```

### Tests

`FuzzNormalize` asserts a real post-condition — the normalized output never
contains `//` — and, because it runs at all, that no input panics. Under plain
`go test` this function runs every seed from `f.Add` *and* every file under
`testdata/fuzz/FuzzNormalize`, including the committed `slash_panic` crasher.

Create `normalize_test.go`:

```go
package pathnorm

import (
	"fmt"
	"strings"
	"testing"
)

func TestNormalizeTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"/a//b///c", "/a/b/c"},
		{"/", "/"},
		{"", "/"},
		{"trailing//", "trailing/"},
		{"////", "/"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := Normalize(tc.in); got != tc.want {
				t.Fatalf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func FuzzNormalize(f *testing.F) {
	f.Add("/")
	f.Add("/a//b")
	f.Add("no slash")
	f.Add("////")
	f.Fuzz(func(t *testing.T, path string) {
		got := Normalize(path) // the original bug panicked here on a trailing '/'
		if strings.Contains(got, "//") {
			t.Fatalf("Normalize(%q) = %q still contains a double slash", path, got)
		}
	})
}

func Example() {
	fmt.Println(Normalize("/x//y/"))
	// Output: /x/y/
}
```

## Review

The exercise is correct when three things are true at once: `Normalize` no longer
reads out of range (the `i+1 < len(path)` guard), the committed
`testdata/fuzz/FuzzNormalize/slash_panic` file replays green under plain
`go test`, and the fuzz invariant "no `//` in the output" holds for every input.
The durable lesson is the workflow: a fuzzer-found crash becomes a minimized file,
that file is committed as source (never gitignored), and CI's plain `go test`
replays it forever. If you delete the corpus file, you delete the regression test
and the bug can creep back. Run `go test -race ./...` to see the committed corpus
replayed, then `go test -fuzz=FuzzNormalize -fuzztime=2s` to keep hunting for the
next one.

## Resources

- [Go Fuzzing reference: corpus and minimization](https://go.dev/doc/security/fuzz/) — the `testdata/fuzz` layout and the `go test fuzz v1` file format.
- [Go Tutorial: fixing a fuzz-found failure](https://go.dev/doc/tutorial/fuzz) — the find-minimize-fix-commit loop end to end.
- [`go test` flags](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — why plain `go test` replays the seed corpus and `-fuzz` mutates.

---

Back to [05-json-body-decoder-limits.md](05-json-body-decoder-limits.md) | Next: [07-roundtrip-splitter-invariant.md](07-roundtrip-splitter-invariant.md)

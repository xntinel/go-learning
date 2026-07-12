# Exercise 8: Fuzz A Static-File Path Joiner For Traversal Safety

A static file server maps a user-supplied path onto a base directory. Get the
join wrong and a request for `../../etc/passwd` reads a file the server was never
meant to expose — a path-traversal vulnerability, one of the oldest and most
common web bugs. This module builds `SafeJoin` on top of `filepath.IsLocal` and
fuzzes the security invariant directly: for every possible user path, the result
either errors or stays strictly under the base directory.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
safepath/                  independent module: example.com/safepath
  go.mod                   module path
  join.go                  SafeJoin(base, userPath string) (string, error); ErrTraversal
  cmd/
    demo/
      main.go              join a few safe and hostile paths, print the outcome
  join_test.go             TestSafeJoinTable, FuzzSafeJoinContained, Example
```

Files: `join.go`, `cmd/demo/main.go`, `join_test.go`.
Implement: `SafeJoin(base, userPath string) (string, error)` returning
`ErrTraversal` (wrapped) for any path that is not local.
Test: a table test for the known attacks; `FuzzSafeJoinContained` proving every
accepted result stays under the base.
Verify: `go test -race ./...`, then `go test -fuzz=FuzzSafeJoinContained
-fuzztime=2s`.

### Why filepath.IsLocal is the whole safety argument

The temptation is to write the containment check by hand: clean the path, join it,
then verify the result still starts with the base. That check is deceptively hard
to get right across `..` chains, absolute paths, and separator quirks, and a
subtle miss is a live vulnerability. `path/filepath.IsLocal` exists precisely to
make this decision correct once, in the standard library. Its contract is exactly
what a static server needs: `IsLocal(p)` reports whether `p`, when joined to any
directory, is *guaranteed* to stay within that directory. It returns false for an
absolute path, for any path that escapes its starting point via `..`, for the
empty string, and (on Windows) for reserved device names.

So `SafeJoin` is short: reject anything that is not `IsLocal`, and otherwise
`filepath.Join(base, userPath)` — which also `Clean`s the result. Because
`IsLocal` guaranteed the path cannot escape, the join cannot escape either. The
security invariant is a direct corollary of the standard library's contract, not
of hand-written string prefix logic.

The fuzz target is where that argument is *proven* rather than asserted. For every
generated `userPath`, `SafeJoin` either returns an error (a rejected path — fine)
or a result that must be contained under `base`. Containment is checked
independently of the implementation: `filepath.Rel(base, result)` must succeed and
must not begin with `..`, which is the definition of "still under base". If any
input ever produced an accepted result that escaped, the fuzzer would minimize it
to a tiny reproducer — and that reproducer would be a genuine path-traversal
exploit against the server.

Create `join.go`:

```go
package safepath

import (
	"errors"
	"fmt"
	"path/filepath"
)

// ErrTraversal is returned (wrapped) when a user path is not local: absolute, an
// escaping ".." chain, empty, or otherwise able to leave the base directory.
var ErrTraversal = errors.New("path escapes base directory")

// SafeJoin joins userPath onto base, guaranteeing the result stays within base.
// It relies on filepath.IsLocal for the safety decision and Clean-joins the rest.
func SafeJoin(base, userPath string) (string, error) {
	if !filepath.IsLocal(userPath) {
		return "", fmt.Errorf("reject %q: %w", userPath, ErrTraversal)
	}
	return filepath.Join(base, userPath), nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/safepath"
)

func main() {
	const base = "/srv/static"
	for _, p := range []string{"css/app.css", "a/../b.txt", "../../etc/passwd", "/etc/passwd", ""} {
		got, err := safepath.SafeJoin(base, p)
		if err != nil {
			fmt.Printf("%-18q reject\n", p)
			continue
		}
		fmt.Printf("%-18q -> %s\n", p, got)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"css/app.css"      -> /srv/static/css/app.css
"a/../b.txt"       -> /srv/static/b.txt
"../../etc/passwd" reject
"/etc/passwd"      reject
""                 reject
```

### Tests

Create `join_test.go`:

```go
package safepath

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeJoinTable(t *testing.T) {
	t.Parallel()
	const base = "/srv/static"
	cases := []struct {
		userPath string
		want     string
		reject   bool
	}{
		{"index.html", "/srv/static/index.html", false},
		{"a/b/c.js", "/srv/static/a/b/c.js", false},
		{"a/../b.txt", "/srv/static/b.txt", false},
		{"../secret", "", true},
		{"../../etc/passwd", "", true},
		{"/etc/passwd", "", true},
		{"", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.userPath, func(t *testing.T) {
			t.Parallel()
			got, err := SafeJoin(base, tc.userPath)
			if tc.reject {
				if !errors.Is(err, ErrTraversal) {
					t.Fatalf("SafeJoin(%q) err = %v, want ErrTraversal", tc.userPath, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("SafeJoin(%q) unexpected error: %v", tc.userPath, err)
			}
			if got != tc.want {
				t.Fatalf("SafeJoin(%q) = %q, want %q", tc.userPath, got, tc.want)
			}
		})
	}
}

func FuzzSafeJoinContained(f *testing.F) {
	for _, s := range []string{"a/b.txt", "a/../b", "../escape", "../../../../etc/passwd", "/abs", "", ".", "a/./b", "a\x00b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, userPath string) {
		const base = "/srv/static"
		got, err := SafeJoin(base, userPath)
		if err != nil {
			return // rejected paths are safe by definition
		}
		// Accepted: the result must stay strictly under base.
		rel, rerr := filepath.Rel(base, got)
		if rerr != nil {
			t.Fatalf("SafeJoin(%q) = %q not relative to base: %v", userPath, got, rerr)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			t.Fatalf("SafeJoin(%q) = %q escapes base (rel=%q)", userPath, got, rel)
		}
	})
}

func Example() {
	got, err := SafeJoin("/srv/static", "img/logo.png")
	fmt.Println(got, err)
	// Output: /srv/static/img/logo.png <nil>
}
```

## Review

`SafeJoin` is correct when every accepted result is provably contained under the
base and every escaping or absolute path is rejected with `ErrTraversal`. The key
design decision is delegating the safety judgment to `filepath.IsLocal` rather
than reimplementing containment with string prefixes — the standard library's
contract *is* the proof, and the fuzz target verifies your usage of that contract
holds for inputs you never listed. Because a divergence here is an actual
path-traversal exploit, this is the class of target where a time-boxed fuzz job in
CI earns its keep. Run `go test -race ./...`, then
`go test -fuzz=FuzzSafeJoinContained -fuzztime=2s`.

## Resources

- [`path/filepath.IsLocal`](https://pkg.go.dev/path/filepath#IsLocal) — the exact "stays within the directory" contract this module relies on.
- [`path/filepath.Join`](https://pkg.go.dev/path/filepath#Join) and [`Clean`](https://pkg.go.dev/path/filepath#Clean) — the Clean-join that produces the final path.
- [`path/filepath.Rel`](https://pkg.go.dev/path/filepath#Rel) — the independent containment check the fuzz invariant uses.

---

Back to [07-roundtrip-splitter-invariant.md](07-roundtrip-splitter-invariant.md) | Next: [09-stateful-token-bucket.md](09-stateful-token-bucket.md)

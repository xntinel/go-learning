# Exercise 13: Config Tree Walker With an Early-Stop Visitor Callback

**Nivel: Intermedio** — validacion rapida (un test corto).

A parsed YAML or JSON config is a tree of nested maps. Walking it to check
every leaf — hunting for a plaintext secret, say — needs the same early-stop
contract as a Go 1.23 iterator, but over a recursive structure instead of a
flat slice. This module builds that walker with a `Visitor` callback.

## What you'll build

```text
configwalk/                 independent module: example.com/config-tree-walk
  go.mod                     go 1.24
  configwalk.go              type Visitor, func Walk
  configwalk_test.go         table test: full walk vs early stop, visit order
```

Files: `configwalk.go`, `configwalk_test.go`.
Implement: `type Visitor func(path string, value any) bool` and
`func Walk(tree map[string]any, prefix string, visit Visitor) bool`.
Test: a table over a two-branch config tree — one case where the visitor
always continues, one where it stops at a specific path — asserting both the
visited-path sequence and Walk's returned bool.
Verify: `go test -count=1 ./...`

```bash
mkdir -p go-solutions/04-functions/06-function-types-and-callbacks/13-config-tree-visitor-early-stop
cd go-solutions/04-functions/06-function-types-and-callbacks/13-config-tree-visitor-early-stop
go mod edit -go=1.24
```

### Why Walk returns a bool

`Visitor` returning `false` must stop the walk *immediately*, exactly like
the range-over-func `yield` contract: the moment the consumer says "enough,"
the producer must not visit another leaf. A recursive walker only honors that
if every recursive call propagates the stop signal back up, which is why
`Walk` itself returns a `bool` — "did I visit everything" — and every call
site checks it before continuing to the next sibling. Sorting the keys makes
the walk deterministic, which is what makes the test's exact visited-path
sequence assertable at all; ranging over a `map` directly would not be.

Create `configwalk.go`:

```go
package configwalk

import "sort"

// Visitor is called once per leaf value found while walking a config tree.
// path is the dotted key (e.g. "database.password"); value is the leaf's raw
// value. Returning false stops the walk immediately, mirroring the
// range-over-func yield contract: the producer must stop the instant the
// consumer asks it to.
type Visitor func(path string, value any) bool

// Walk descends tree depth-first in deterministic (sorted) key order, calling
// visit for every leaf. A leaf is any value that is not itself a
// map[string]any. Walk reports whether it visited every leaf (true) or
// stopped early because visit returned false (false).
func Walk(tree map[string]any, prefix string, visit Visitor) bool {
	keys := make([]string, 0, len(tree))
	for k := range tree {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		v := tree[k]
		if sub, ok := v.(map[string]any); ok {
			if !Walk(sub, path, visit) {
				return false
			}
			continue
		}
		if !visit(path, v) {
			return false
		}
	}
	return true
}
```

### Tests

The sample tree has two branches, `database` (three leaves, one named
`password`) and `server` (one leaf). Sorted order makes the full visit
sequence `database.host`, `database.password`, `database.port`,
`server.port`. The second case's visitor stops as soon as it sees
`database.password`, so it visits exactly two leaves and `Walk` reports
`false`.

Create `configwalk_test.go`:

```go
package configwalk

import "testing"

func sampleTree() map[string]any {
	return map[string]any{
		"database": map[string]any{
			"host":     "db.internal",
			"password": "hunter2",
			"port":     "5432",
		},
		"server": map[string]any{
			"port": "8080",
		},
	}
}

func TestWalk(t *testing.T) {
	tests := []struct {
		name       string
		visit      func(visited *[]string) Visitor
		wantPaths  []string
		wantResult bool
	}{
		{
			name: "visits every leaf in sorted order when visitor always continues",
			visit: func(visited *[]string) Visitor {
				return func(path string, value any) bool {
					*visited = append(*visited, path)
					return true
				}
			},
			wantPaths:  []string{"database.host", "database.password", "database.port", "server.port"},
			wantResult: true,
		},
		{
			name: "stops the instant the visitor rejects a plaintext secret",
			visit: func(visited *[]string) Visitor {
				return func(path string, value any) bool {
					*visited = append(*visited, path)
					return path != "database.password"
				}
			},
			wantPaths:  []string{"database.host", "database.password"},
			wantResult: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var visited []string
			got := Walk(sampleTree(), "", tc.visit(&visited))

			if got != tc.wantResult {
				t.Errorf("Walk() = %v, want %v", got, tc.wantResult)
			}
			if len(visited) != len(tc.wantPaths) {
				t.Fatalf("visited %v, want %v", visited, tc.wantPaths)
			}
			for i, p := range tc.wantPaths {
				if visited[i] != p {
					t.Errorf("visited[%d] = %q, want %q", i, visited[i], p)
				}
			}
		})
	}
}
```

Run it: `go test -count=1 ./...`

## Review

The recursive call to `Walk` for a nested branch and the direct call to
`visit` for a leaf both feed the same `if !x { return false }` check — that
uniform propagation is what makes early stop work across arbitrary nesting
depth, not just one level. Left out on purpose: slices and other composite
values as leaves, and cycle detection for a tree built from untrusted input
— a hostile or buggy config source could construct a self-referential
structure that this walker would recurse into forever.

## Resources

- [Go Wiki: Range over Function Types](https://go.dev/wiki/RangefuncExperiment)
- [sort.Strings](https://pkg.go.dev/sort#Strings)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-pubsub-topic-subscribers.md](12-pubsub-topic-subscribers.md) | Next: [14-batch-migrator-progress-callback.md](14-batch-migrator-progress-callback.md)

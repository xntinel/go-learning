# Exercise 19: Fail-Fast init() — Validating a Migration Dependency Graph

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Schema migrations declare which other migrations must run before them. This
exercise validates that declared dependency graph at `init()` — rejecting a
reference to an undeclared migration and, the trickier case, a cycle — and
computes a safe topological application order once, so a broken migration
graph is caught the moment the binary starts, long before any `ALTER TABLE`
runs against a real database.

## What you'll build

```text
migrations/                independent module: example.com/migrations
  go.mod                    module example.com/migrations
  migrations.go              Migration, registered list, topoSort (Kahn's algorithm), init()
  cmd/
    demo/
      main.go                prints the computed application order
  migrations_test.go          valid-graph table + cycle + missing-parent + duplicate-name cases
```

Files: `migrations.go`, `cmd/demo/main.go`, `migrations_test.go`.
Implement: `Migration{Name string; Depends []string}`, a `registered` list, `topoSort(migrations []Migration) ([]string, error)` using Kahn's algorithm, and `init()` calling it and panicking on error.
Test: a table of valid graphs (linear chain, diamond, no dependencies) each produce the expected order; a two-node cycle is rejected; a dependency on an undeclared migration is rejected; a duplicate migration name is rejected.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/08-init-functions-and-package-initialization/19-schema-migration-ordering-dag-validator/cmd/demo
cd go-solutions/04-functions/08-init-functions-and-package-initialization/19-schema-migration-ordering-dag-validator
go mod edit -go=1.24
```

### Why a cycle is the failure mode worth testing specifically

A missing-parent check is a simple set-membership test — every `Depends`
entry must appear somewhere in `registered`. A cycle is qualitatively harder
to catch, because nothing about any single migration's declaration looks
wrong; the problem only exists in the *shape* of the whole graph. `topoSort`
uses Kahn's algorithm specifically because it detects a cycle as a natural
side effect of the same pass that computes the order: it repeatedly emits
migrations with zero remaining unmet dependencies and decrements their
dependents' counts, and if the graph has a cycle, at least one migration's
dependency count never reaches zero — so the number of migrations emitted
ends up strictly less than the number that were registered. That single
length check is both the cycle detector and the correctness check, with no
separate cycle-detection pass needed.

Running this at `init()` — with the result cached in the package-level
`Order` — means the ordering computation happens exactly once, at process
start, and every later reader of `Order` gets the same validated slice
without recomputing it.

Create `migrations.go`:

```go
// migrations.go
// Package migrations declares this service's schema migrations and their
// dependencies, validating at init that the dependency graph is a valid DAG
// — no cycles, no reference to an undeclared migration — and computing a
// safe application order once, up front.
package migrations

import "fmt"

// Migration declares a schema migration and the names of migrations that
// must be applied before it.
type Migration struct {
	Name    string
	Depends []string
}

// registered is the fixed set of migrations this service ships. A real
// project would generate this list from migration files on disk; it is
// declared directly here so the validator has something concrete to check.
var registered = []Migration{
	{Name: "001_create_users", Depends: nil},
	{Name: "002_create_posts", Depends: []string{"001_create_users"}},
	{Name: "003_add_user_email_index", Depends: []string{"001_create_users"}},
	{Name: "004_add_post_author_fk", Depends: []string{"002_create_posts", "001_create_users"}},
}

// Order is the topologically sorted application order for registered,
// computed once at init. A migration only ever appears after every
// migration it depends on.
var Order []string

func init() {
	order, err := topoSort(registered)
	if err != nil {
		panic(fmt.Errorf("migrations: invalid migration graph: %w", err))
	}
	Order = order
}

// topoSort computes a valid application order using Kahn's algorithm,
// failing if a migration depends on a name that was never declared, or if
// the graph contains a cycle. Extracted from init so a test can feed it a
// deliberately broken graph.
func topoSort(migrations []Migration) ([]string, error) {
	byName := make(map[string]Migration, len(migrations))
	for _, m := range migrations {
		if _, dup := byName[m.Name]; dup {
			return nil, fmt.Errorf("migration %q declared more than once", m.Name)
		}
		byName[m.Name] = m
	}
	for _, m := range migrations {
		for _, dep := range m.Depends {
			if _, ok := byName[dep]; !ok {
				return nil, fmt.Errorf("migration %q depends on undeclared migration %q", m.Name, dep)
			}
		}
	}

	// indegree[name] counts how many not-yet-emitted dependencies remain.
	indegree := make(map[string]int, len(migrations))
	dependents := make(map[string][]string, len(migrations)) // dep -> migrations that need it
	for _, m := range migrations {
		indegree[m.Name] = len(m.Depends)
		for _, dep := range m.Depends {
			dependents[dep] = append(dependents[dep], m.Name)
		}
	}

	// queue holds names with zero remaining dependencies. Migrations are
	// added to it in declared order, so among several equally-ready
	// migrations the result is deterministic rather than map-order-dependent.
	var queue []string
	for _, m := range migrations {
		if indegree[m.Name] == 0 {
			queue = append(queue, m.Name)
		}
	}

	var order []string
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		order = append(order, name)
		for _, dependent := range dependents[name] {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}

	if len(order) != len(migrations) {
		return nil, fmt.Errorf("migration graph has a cycle: only %d of %d migrations could be ordered", len(order), len(migrations))
	}
	return order, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/migrations"
)

func main() {
	fmt.Println("migration order:", migrations.Order)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
migration order: [001_create_users 002_create_posts 003_add_user_email_index 004_add_post_author_fk]
```

### Tests

Create `migrations_test.go`:

```go
// migrations_test.go
package migrations

import (
	"reflect"
	"testing"
)

// TestPackageLoaded proves init did not panic: if the shipped migration
// graph had a cycle or a missing parent, this test binary would never reach
// a test body.
func TestPackageLoaded(t *testing.T) {
	if len(Order) != len(registered) {
		t.Fatalf("Order has %d entries, want %d", len(Order), len(registered))
	}
}

func TestOrderRespectsDependencies(t *testing.T) {
	position := make(map[string]int, len(Order))
	for i, name := range Order {
		position[name] = i
	}
	for _, m := range registered {
		for _, dep := range m.Depends {
			if position[dep] >= position[m.Name] {
				t.Errorf("%q (pos %d) does not come after its dependency %q (pos %d)",
					m.Name, position[m.Name], dep, position[dep])
			}
		}
	}
}

func TestTopoSortValidGraph(t *testing.T) {
	tests := []struct {
		name       string
		migrations []Migration
		want       []string
	}{
		{
			name:       "linear chain",
			migrations: []Migration{{Name: "a"}, {Name: "b", Depends: []string{"a"}}, {Name: "c", Depends: []string{"b"}}},
			want:       []string{"a", "b", "c"},
		},
		{
			name:       "diamond",
			migrations: []Migration{{Name: "a"}, {Name: "b", Depends: []string{"a"}}, {Name: "c", Depends: []string{"a"}}, {Name: "d", Depends: []string{"b", "c"}}},
			want:       []string{"a", "b", "c", "d"},
		},
		{
			name:       "no dependencies",
			migrations: []Migration{{Name: "a"}, {Name: "b"}},
			want:       []string{"a", "b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := topoSort(tt.migrations)
			if err != nil {
				t.Fatalf("topoSort error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("topoSort = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTopoSortRejectsCycle(t *testing.T) {
	broken := []Migration{
		{Name: "a", Depends: []string{"b"}},
		{Name: "b", Depends: []string{"a"}},
	}
	if _, err := topoSort(broken); err == nil {
		t.Fatal("topoSort did not reject a cyclic graph")
	}
}

func TestTopoSortRejectsMissingParent(t *testing.T) {
	broken := []Migration{
		{Name: "a", Depends: []string{"does-not-exist"}},
	}
	if _, err := topoSort(broken); err == nil {
		t.Fatal("topoSort did not reject a reference to an undeclared migration")
	}
}

func TestTopoSortRejectsDuplicateName(t *testing.T) {
	broken := []Migration{
		{Name: "a"},
		{Name: "a"},
	}
	if _, err := topoSort(broken); err == nil {
		t.Fatal("topoSort did not reject a duplicate migration name")
	}
}
```

## Review

`TestTopoSortValidGraph`'s three shapes — a linear chain, a diamond, and no
dependencies at all — cover the structural cases a real migration set
actually takes; `TestOrderRespectsDependencies` then re-derives the same
property directly from the package's real `registered` list, so it keeps
working even if someone edits that list later. `TestTopoSortRejectsCycle` is
the one to pay closest attention to: it is the case a naive "does every
dependency exist" check would completely miss, since both `a` and `b` in the
broken graph reference migrations that *do* exist — the problem is purely
structural.

The trap to avoid is validating migrations one at a time as they're declared
instead of validating the graph as a whole. A cycle, by definition, cannot be
detected by looking at any single migration in isolation — it only becomes
visible once Kahn's algorithm (or an equivalent whole-graph traversal) is run
across the entire set.

## Resources

- [Wikipedia — Topological sorting (Kahn's algorithm)](https://en.wikipedia.org/wiki/Topological_sorting#Kahn's_algorithm) — the algorithm this exercise implements.
- [golang-migrate/migrate](https://github.com/golang-migrate/migrate) — a real Go migration tool that must solve this same ordering problem in production.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-feature-flag-table-consistency-check.md](18-feature-flag-table-consistency-check.md) | Next: [20-tls-certificates-loader-and-validator.md](20-tls-certificates-loader-and-validator.md)

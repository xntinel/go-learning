# Exercise 29: Database Schema Migration Sequencer with Dependency Graph

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde).

Schema migrations rarely form a straight line — an index migration and a
foreign-key migration might both depend on the table that creates the
column, but not on each other. Running them in file-name order works
until someone adds a migration whose true dependency comes later
alphabetically. `Sequence(migrations ...Migration)` takes the whole batch
as a graph of named dependencies and returns one valid run order — or an
error the moment that graph cannot produce one.

## What you'll build

```text
migseq/                    independent module: example.com/migseq
  go.mod                   go 1.24
  migseq.go                package migseq; type Migration struct{Name string; DependsOn []string}; Sequence(migrations ...Migration) ([]string, error)
  cmd/
    demo/
      main.go              runnable demo: a small table/index/fk graph, then a two-migration cycle
  migseq_test.go            table tests: dependency ordering, determinism across repeated runs, cycle, unknown dependency, duplicate name, empty input
```

- Files: `migseq.go`, `cmd/demo/main.go`, `migseq_test.go`.
- Implement: `type Migration struct{ Name string; DependsOn []string }` and `Sequence(migrations ...Migration) ([]string, error)`, a topological sort with alphabetical tie-breaking for determinism.
- Test: every migration appears after all of its dependencies; running `Sequence` on the same input repeatedly always returns the identical order; a two-migration cycle, a dependency on a name never given, and a duplicate migration name each return an error; zero migrations returns an empty, non-error result.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Kahn's algorithm, and why tie-breaking is not optional

`Sequence` implements Kahn's algorithm: compute, for every migration, how
many of its dependencies have not yet been scheduled (`remaining`); start
with every migration whose count is already zero (`ready`); repeatedly
take one out of `ready`, append it to the output, and decrement the
`remaining` count of everything that depended on it, moving any that hit
zero into `ready`. If the loop runs out of `ready` items before every
migration has been scheduled, some migrations still have unmet
dependencies on each other — a cycle — and that is reported as an error
rather than a truncated, silently-wrong order.

The part that is easy to skip and wrong to skip is tie-breaking. At any
point there can be more than one migration in `ready` — nothing about the
graph says whether "create the index" or "add the trigger" should run
first if both only depend on "create the table." Kahn's algorithm as
commonly described just says "pick any," but "any" run non-deterministically
against a real database means two runs of the same migration set could
apply steps in a different order, which turns an already-tricky-to-debug
migration failure into one that is not even reproducible. `Sequence`
breaks every tie by sorting the `ready` set alphabetically before picking
the next migration, so the exact same input always produces the exact
same output — `TestSequenceIsDeterministic` pins this by running the same
graph ten times and asserting byte-identical results.

Create `migseq.go`:

```go
// migseq.go
package migseq

import (
	"fmt"
	"sort"
)

// Migration is one database schema migration step. DependsOn names other
// migrations (by Name) that must run before this one.
type Migration struct {
	Name      string
	DependsOn []string
}

// Sequence topologically orders migrations so that every migration runs
// after everything it depends on. Among migrations that are equally ready
// to run at a given point, it breaks ties by name, so the result is
// deterministic across runs and across the order migrations were passed
// in. Sequence returns an error for a duplicate migration name, a
// dependency on a migration that was not given, or a dependency cycle.
func Sequence(migrations ...Migration) ([]string, error) {
	byName := make(map[string]Migration, len(migrations))
	for _, m := range migrations {
		if _, dup := byName[m.Name]; dup {
			return nil, fmt.Errorf("migseq: duplicate migration name %q", m.Name)
		}
		byName[m.Name] = m
	}
	for _, m := range migrations {
		for _, dep := range m.DependsOn {
			if _, ok := byName[dep]; !ok {
				return nil, fmt.Errorf("migseq: %q depends on unknown migration %q", m.Name, dep)
			}
		}
	}

	// remaining[name] = number of not-yet-scheduled dependencies.
	remaining := make(map[string]int, len(migrations))
	// dependents[name] = migrations that list name in DependsOn.
	dependents := make(map[string][]string, len(migrations))
	for _, m := range migrations {
		remaining[m.Name] = len(m.DependsOn)
		for _, dep := range m.DependsOn {
			dependents[dep] = append(dependents[dep], m.Name)
		}
	}

	var ready []string
	for name, n := range remaining {
		if n == 0 {
			ready = append(ready, name)
		}
	}
	sort.Strings(ready)

	order := make([]string, 0, len(migrations))
	for len(ready) > 0 {
		sort.Strings(ready)
		next := ready[0]
		ready = ready[1:]
		order = append(order, next)

		for _, dep := range dependents[next] {
			remaining[dep]--
			if remaining[dep] == 0 {
				ready = append(ready, dep)
			}
		}
	}

	if len(order) != len(migrations) {
		return nil, fmt.Errorf("migseq: dependency cycle detected among %d unresolved migration(s)", len(migrations)-len(order))
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

	"example.com/migseq"
)

func main() {
	order, err := migseq.Sequence(
		migseq.Migration{Name: "add_users_table"},
		migseq.Migration{Name: "add_orders_table", DependsOn: []string{"add_users_table"}},
		migseq.Migration{Name: "add_users_email_index", DependsOn: []string{"add_users_table"}},
		migseq.Migration{Name: "add_orders_user_fk", DependsOn: []string{"add_orders_table", "add_users_table"}},
	)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("migration order:")
	for i, name := range order {
		fmt.Printf("  %d. %s\n", i+1, name)
	}

	_, err = migseq.Sequence(
		migseq.Migration{Name: "a", DependsOn: []string{"b"}},
		migseq.Migration{Name: "b", DependsOn: []string{"a"}},
	)
	fmt.Println("cycle error:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
migration order:
  1. add_users_table
  2. add_orders_table
  3. add_orders_user_fk
  4. add_users_email_index
cycle error: migseq: dependency cycle detected among 2 unresolved migration(s)
```

### Tests

`TestSequenceIsDeterministic` is the edge case that separates a merely
correct topological sort from a production-ready one: it runs the same
three-migration graph ten times and asserts every run returns the exact
same order, which only holds because ties are broken by name rather than
left to map iteration order (which Go randomizes on purpose).

Create `migseq_test.go`:

```go
// migseq_test.go
package migseq

import (
	"strings"
	"testing"
)

func TestSequenceOrdersDependenciesFirst(t *testing.T) {
	t.Parallel()

	order, err := Sequence(
		Migration{Name: "add_users_table"},
		Migration{Name: "add_orders_table", DependsOn: []string{"add_users_table"}},
		Migration{Name: "add_users_email_index", DependsOn: []string{"add_users_table"}},
		Migration{Name: "add_orders_user_fk", DependsOn: []string{"add_orders_table", "add_users_table"}},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pos := make(map[string]int, len(order))
	for i, name := range order {
		pos[name] = i
	}
	if pos["add_users_table"] >= pos["add_orders_table"] {
		t.Errorf("add_users_table must come before add_orders_table: %v", order)
	}
	if pos["add_users_table"] >= pos["add_users_email_index"] {
		t.Errorf("add_users_table must come before add_users_email_index: %v", order)
	}
	if pos["add_orders_table"] >= pos["add_orders_user_fk"] {
		t.Errorf("add_orders_table must come before add_orders_user_fk: %v", order)
	}
	if pos["add_users_table"] >= pos["add_orders_user_fk"] {
		t.Errorf("add_users_table must come before add_orders_user_fk: %v", order)
	}
}

func TestSequenceIsDeterministic(t *testing.T) {
	t.Parallel()

	build := func() []Migration {
		return []Migration{
			{Name: "c"},
			{Name: "a"},
			{Name: "b", DependsOn: []string{"a"}},
		}
	}

	first, err := Sequence(build()...)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i := 0; i < 10; i++ {
		got, err := Sequence(build()...)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("run %d: order = %v, want %v (must be deterministic)", i, got, first)
		}
	}
}

func TestSequenceDetectsCycle(t *testing.T) {
	t.Parallel()

	_, err := Sequence(
		Migration{Name: "a", DependsOn: []string{"b"}},
		Migration{Name: "b", DependsOn: []string{"a"}},
	)
	if err == nil {
		t.Fatal("expected a cycle error")
	}
}

func TestSequenceDetectsUnknownDependency(t *testing.T) {
	t.Parallel()

	_, err := Sequence(Migration{Name: "a", DependsOn: []string{"ghost"}})
	if err == nil {
		t.Fatal("expected an unknown-dependency error")
	}
}

func TestSequenceDetectsDuplicateName(t *testing.T) {
	t.Parallel()

	_, err := Sequence(Migration{Name: "a"}, Migration{Name: "a"})
	if err == nil {
		t.Fatal("expected a duplicate-name error")
	}
}

func TestSequenceEmptyIsEmpty(t *testing.T) {
	t.Parallel()

	order, err := Sequence()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(order) != 0 {
		t.Fatalf("order = %v, want empty", order)
	}
}
```

## Review

`Sequence` is correct when every migration's position strictly follows
every one of its dependencies, the same graph always yields the same
order, and a graph that cannot be ordered (a cycle) fails loudly instead
of returning a truncated or arbitrary list. The senior points are
validating the input graph itself before running Kahn's algorithm — a
duplicate name or a dangling dependency should be its own clear error, not
something that surfaces as a confusing cycle failure three steps later —
and breaking ties deterministically, since "any valid order" is a correct
answer mathematically but an unshippable one operationally: a migration
runner that reorders steps between runs of the identical migration set is
not something anyone can safely operate.

## Resources

- [Wikipedia: Topological sorting — Kahn's algorithm](https://en.wikipedia.org/wiki/Topological_sorting#Kahn's_algorithm)
- [`sort.Strings`](https://pkg.go.dev/sort#Strings)
- [golang-migrate](https://github.com/golang-migrate/migrate) — a real Go migration runner whose ordering guarantees this exercise mirrors in miniature.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [28-payload-validation-rule-engine.md](28-payload-validation-rule-engine.md) | Next: [30-multipart-upload-validator-rules.md](30-multipart-upload-validator-rules.md)

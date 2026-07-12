# 7. Migration Patterns

Migrations are operational code: each step needs identity, order, and a failure mode that can be inspected.

## Concepts

### Keep the Boundary Small

The Go database guide separates query execution, transactions, cancellation, and pool configuration. A small package boundary lets tests exercise the rule being taught without requiring a network database.

### Preserve the Database Contract

Even with an offline fake, the code keeps the same production habits: validate before executing, wrap sentinel errors with `%w`, expose accessors instead of raw fields, and test caller-visible behavior.

### Verify With Tests, Not Printed Output

The demo runs through exported API, but `go test` is the gate. Table-driven tests pin success and validation behavior, while the example is checked automatically by the test runner.

## Exercises

Set up the module:

```bash
go mod edit -go=1.26
```

This is a library package. The demo is under `cmd/demo`, but the contract is verified by `go test`.

Create `store.go`:

```go
package migrate

import (
	"errors"
	"fmt"
)

var ErrEmptyName = errors.New("name must not be empty")

type Migration struct {
	name string
}

func NewMigration(name string) (Migration, error) {
	if name == "" {
		return Migration{}, fmt.Errorf("migration: %w", ErrEmptyName)
	}
	return Migration{name: name}, nil
}

func (v Migration) Name() string {
	return v.name
}

func (v Migration) Ready() bool {
	return v.name != ""
}

func (v Migration) Describe() string {
	if !v.Ready() {
		return "not ready"
	}
	return fmt.Sprintf("migration:%s", v.name)
}
```

Create `store_test.go`:

```go
package migrate

import (
	"errors"
	"testing"
)

func TestNewMigration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
		err   error
	}{
		{name: "valid", input: "primary", want: "migration:primary"},
		{name: "empty", input: "", err: ErrEmptyName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := NewMigration(tt.input)
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if tt.err != nil {
				return
			}
			if got.Describe() != tt.want {
				t.Fatalf("Describe() = %q, want %q", got.Describe(), tt.want)
			}
			if !got.Ready() {
				t.Fatal("Ready() = false, want true")
			}
		})
	}
}
```

Create `example_test.go`:

```go
package migrate

import "fmt"

func ExampleNewMigration() {
	v, _ := NewMigration("primary")
	fmt.Println(v.Describe())
	// Output: migration:primary
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/07-migration-patterns"
)

func main() {
	v, err := migrate.NewMigration("primary")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(v.Describe())
}
```

Add one more table-driven test of your own before running the gate. Prefer an edge case that would fail if a validation branch were removed.

## Common Mistakes

Wrong: Check validation errors by comparing `err.Error()`. What happens: wrapping with context breaks the test. Fix: wrap sentinel errors with `%w` and assert with `errors.Is`.

Wrong: Make fields exported so the demo can inspect them. What happens: representation becomes API. Fix: keep fields private and add small accessors.

Wrong: Treat `go run ./cmd/demo` as the verification. What happens: regressions survive if the printed text is not inspected. Fix: make `go test -race ./...` the required gate.

## Verification

From the module directory:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All commands must pass. `go test` is the verification; the demo only proves that exported API is usable by another package.

## Summary

- The package models one database-pattern boundary without external drivers.
- Validation failures are sentinel errors and remain matchable after wrapping.
- Tests, examples, and the demo exercise the same exported API.

## What's Next

Continue with [sqlc Type-Safe SQL](../08-sqlc-type-safe-sql/08-sqlc-type-safe-sql.md).

## Resources

- Go database guide: https://go.dev/doc/database/
- `database/sql` package reference: https://pkg.go.dev/database/sql
- Context package reference: https://pkg.go.dev/context
- Go testing package reference: https://pkg.go.dev/testing

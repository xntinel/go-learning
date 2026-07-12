# Exercise 1: Migration Tool: Build and Inspect a Three-Layer Wrapping Chain

A schema-migration tool is a perfect place to see a wrapping chain earn its keep:
loading the migration file, parsing it, and running the steps are three distinct
phases, and when one fails an operator needs to know *which* phase and *why*, all
the way down to the OS error. This module builds that tool so a single returned
error answers both questions through `errors.Is` and `errors.As`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
migration/                  independent module: example.com/migration
  go.mod                    go 1.24
  migration.go              sentinels ErrNotFound/ErrPermission/ErrCorrupt; LoadFile,
                            ParseConfig, Migrator, *MigrationError (Unwrap), RunMigrator
  cmd/
    demo/
      main.go               runnable demo: run against a missing file, inspect the chain
  migration_test.go         table-driven: success, not-found, corrupt, failing step, OS-reach
```

Files: `migration.go`, `cmd/demo/main.go`, `migration_test.go`.
Implement: `LoadFile`/`ParseConfig` mapping `os` errors to package sentinels with `%w`, a `Migrator` running steps, and `RunMigrator` wrapping each phase in a `*MigrationError` whose `Unwrap` exposes the cause.
Test: `errors.Is` finds the innermost sentinel across three layers, `errors.As` extracts `*MigrationError` with the right `Phase`, and `os.ErrNotExist` stays reachable through the whole chain.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/06-error-wrapping-chains/01-migration-tool-wrapping-chain/cmd/demo
cd go-solutions/10-error-handling/06-error-wrapping-chains/01-migration-tool-wrapping-chain
go mod edit -go=1.24
```

### How the three layers stack

`RunMigrator` runs three phases in order and wraps each in a `*MigrationError`
tagged with the phase name. Underneath that, `LoadFile` and `ParseConfig` do their
own wrapping: `LoadFile` calls `os.ReadFile`, and on failure translates the OS
error into a package sentinel — `os.IsNotExist(err)` becomes `ErrNotFound`,
`os.IsPermission(err)` becomes `ErrPermission` — but wraps the *original* OS error
so it stays reachable. The result for a missing file is a three-link chain:

```
*MigrationError{Phase:"load"}  ->  "load file: ..."  ->  ErrNotFound  ->  *fs.PathError (os.ErrNotExist)
```

`*MigrationError.Unwrap` returning `e.Err` is what makes the whole chain walkable.
Because `LoadFile` wraps `os.ReadFile`'s error rather than replacing it, an
`errors.Is(err, os.ErrNotExist)` at the very top still succeeds — the OS-layer
identity survives translation. That is the property the final test pins: the
domain sentinel `ErrNotFound` drives control flow, and the OS error remains for a
debugger who wants to know it was specifically a missing path.

A subtle point on the translation: `LoadFile` wraps the sentinel with `%w`
(`"load file: %w"`, `ErrNotFound`) *and* the sentinel itself is what carries the
not-found meaning, but to also keep `os.ErrNotExist` reachable we wrap the raw
`err` when neither `IsNotExist` nor `IsPermission` matches. For the not-found and
permission branches we wrap both the domain sentinel and the OS error by joining
them, so `errors.Is` finds `ErrNotFound` *and* `os.ErrNotExist`.

Create `migration.go`:

```go
package migration

import (
	"errors"
	"fmt"
	"os"
)

// Package-level sentinels are the vocabulary callers classify on. They are stable
// identities for errors.Is and never change without a semver bump.
var (
	ErrNotFound   = errors.New("migration file not found")
	ErrPermission = errors.New("migration file permission denied")
	ErrCorrupt    = errors.New("migration file corrupt")
)

// Step is one named migration step.
type Step struct {
	Name string
	Run  func() error
}

// Migrator runs an ordered list of steps.
type Migrator struct {
	Steps []Step
}

// Run executes each step, wrapping the first failure with the step name.
func (m *Migrator) Run() error {
	for _, s := range m.Steps {
		if err := s.Run(); err != nil {
			return fmt.Errorf("step %q: %w", s.Name, err)
		}
	}
	return nil
}

// LoadFile reads path, translating OS errors into domain sentinels while keeping
// the original OS error reachable through the chain.
func LoadFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		switch {
		case os.IsNotExist(err):
			return nil, fmt.Errorf("load file %q: %w", path, errors.Join(ErrNotFound, err))
		case os.IsPermission(err):
			return nil, fmt.Errorf("load file %q: %w", path, errors.Join(ErrPermission, err))
		default:
			return nil, fmt.Errorf("load file %q: %w", path, err)
		}
	}
	return data, nil
}

// ParseConfig turns raw bytes into a config map. An empty file is corrupt.
func ParseConfig(data []byte) (map[string]string, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("parse config: %w", ErrCorrupt)
	}
	return map[string]string{"raw": string(data)}, nil
}

// MigrationError tags a failure with the phase it happened in and wraps the cause.
type MigrationError struct {
	Phase string
	Err   error
}

func (e *MigrationError) Error() string {
	return fmt.Sprintf("migration failed in phase %q: %s", e.Phase, e.Err)
}

// Unwrap exposes the cause so errors.Is/As walk through the phase wrapper.
func (e *MigrationError) Unwrap() error { return e.Err }

// RunMigrator runs load, parse, and steps, tagging each phase's failure.
func RunMigrator(m *Migrator, path string) error {
	data, err := LoadFile(path)
	if err != nil {
		return &MigrationError{Phase: "load", Err: err}
	}
	if _, err := ParseConfig(data); err != nil {
		return &MigrationError{Phase: "parse", Err: err}
	}
	if err := m.Run(); err != nil {
		return &MigrationError{Phase: "run", Err: err}
	}
	return nil
}
```

### The runnable demo

The demo runs the migrator against a path that does not exist and inspects the
returned error three ways: the phase (via `errors.As`), the domain sentinel, and
the still-reachable OS error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"os"

	"example.com/migration"
)

func main() {
	m := &migration.Migrator{}
	err := migration.RunMigrator(m, "/no/such/dir/schema.sql")

	var me *migration.MigrationError
	if errors.As(err, &me) {
		fmt.Printf("failed phase: %s\n", me.Phase)
	}
	fmt.Printf("errors.Is ErrNotFound: %v\n", errors.Is(err, migration.ErrNotFound))
	fmt.Printf("errors.Is os.ErrNotExist: %v\n", errors.Is(err, os.ErrNotExist))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
failed phase: load
errors.Is ErrNotFound: true
errors.Is os.ErrNotExist: true
```

### Tests

The tests are table-driven over the four phases plus the OS-reachability property.
`t.TempDir` gives each case an isolated directory; the success and corrupt cases
write real files, the not-found case points at a missing path, and the failing
step injects a step that returns an error. Every case asserts `errors.As`
extracts a `*MigrationError` with the expected `Phase`, and the sentinel cases
assert `errors.Is` finds the innermost domain sentinel. The dedicated
`TestChainPreservesOSError` pins the contract that `os.ErrNotExist` stays
reachable through the full three-layer chain.

Create `migration_test.go`:

```go
package migration

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunMigrator(t *testing.T) {
	t.Parallel()

	failingStep := &Migrator{Steps: []Step{
		{Name: "create_users", Run: func() error { return errors.New("syntax error") }},
	}}

	tests := []struct {
		name       string
		path       string
		migrator   *Migrator
		wantErr    bool
		wantPhase  string
		wantSentin error
		wantInMsg  string
	}{
		{
			name:     "success",
			path:     writeFile(t, []byte("CREATE TABLE users;")),
			migrator: &Migrator{},
			wantErr:  false,
		},
		{
			name:       "not found",
			path:       "/no/such/dir/schema.sql",
			migrator:   &Migrator{},
			wantErr:    true,
			wantPhase:  "load",
			wantSentin: ErrNotFound,
		},
		{
			name:       "corrupt empty file",
			path:       writeFile(t, nil),
			migrator:   &Migrator{},
			wantErr:    true,
			wantPhase:  "parse",
			wantSentin: ErrCorrupt,
		},
		{
			name:      "failing step",
			path:      writeFile(t, []byte("CREATE TABLE users;")),
			migrator:  failingStep,
			wantErr:   true,
			wantPhase: "run",
			wantInMsg: "create_users",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := RunMigrator(tt.migrator, tt.path)
			if tt.wantErr == (err == nil) {
				t.Fatalf("RunMigrator error = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			var me *MigrationError
			if !errors.As(err, &me) {
				t.Fatalf("errors.As did not find *MigrationError in %v", err)
			}
			if me.Phase != tt.wantPhase {
				t.Errorf("Phase = %q, want %q", me.Phase, tt.wantPhase)
			}
			if tt.wantSentin != nil && !errors.Is(err, tt.wantSentin) {
				t.Errorf("errors.Is(err, %v) = false, want true", tt.wantSentin)
			}
			if tt.wantInMsg != "" && !strings.Contains(err.Error(), tt.wantInMsg) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantInMsg)
			}
		})
	}
}

func TestChainPreservesOSError(t *testing.T) {
	t.Parallel()
	err := RunMigrator(&Migrator{}, "/no/such/dir/schema.sql")

	if !errors.Is(err, ErrNotFound) {
		t.Errorf("errors.Is(err, ErrNotFound) = false, want true")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("errors.Is(err, os.ErrNotExist) = false, want true (OS error must stay reachable)")
	}
}

func ExampleRunMigrator() {
	err := RunMigrator(&Migrator{}, "/no/such/dir/schema.sql")
	var me *MigrationError
	errors.As(err, &me)
	fmt.Println(me.Phase, errors.Is(err, ErrNotFound))
	// Output: load true
}
```

## Review

The migrator is correct when a single returned error answers both operational
questions: `errors.As` tells you the phase, and `errors.Is` tells you the root
cause. The three-layer property is what `TestChainPreservesOSError` proves — the
domain sentinel `ErrNotFound` and the raw `os.ErrNotExist` are *both* reachable
from the top, which is only true because `LoadFile` wraps rather than replaces the
OS error and `MigrationError.Unwrap` keeps the chain walkable. The most common way
to break this is to translate by returning `fmt.Errorf("...: %w", ErrNotFound)`
alone, which drops the OS error; joining the sentinel with the original preserves
both identities. If `errors.Is(err, os.ErrNotExist)` ever returns false, some
layer replaced instead of wrapped.

## Resources

- [errors package](https://pkg.go.dev/errors) — `Is`, `As`, `Join`, `Unwrap`, and tree traversal.
- [fmt.Errorf](https://pkg.go.dev/fmt#Errorf) — the `%w` wrapping verb.
- [os.IsNotExist / os.ErrNotExist](https://pkg.go.dev/os#IsNotExist) — predicates and the sentinel behind file-not-found.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-repository-sentinel-translation.md](02-repository-sentinel-translation.md)

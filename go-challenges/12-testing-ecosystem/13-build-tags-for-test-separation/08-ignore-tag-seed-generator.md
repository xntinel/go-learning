# Exercise 8: //go:build ignore for a seed program beside the package

Repositories routinely keep a small `package main` next to a library — a database
seeder, a code generator, an example command — that must be runnable with
`go run` but must never join the package build or the release binary. The idiom
that makes this work is `//go:build ignore`, a constraint that is never satisfied.
This module builds a deterministic seed generator that lives in the same directory
as its library, stays out of `go build`/`go test`, yet runs via `go run seed.go`
and wires through `go generate`.

Self-contained module: the library package, the ignored seed program, an untagged
demo, and the library test.

## What you'll build

```text
seeddemo/                  independent module: example.com/seeddemo
  go.mod
  user.go                  package seeddemo: User, GenerateUsers; //go:generate directive
  seed.go                  //go:build ignore; package main; prints INSERT statements
  seed_test.go             untagged: TestGenerateUsers, ExampleGenerateUsers
  cmd/
    demo/
      main.go              prints a couple of generated users
```

- Files: `user.go`, `seed.go`, `seed_test.go`, `cmd/demo/main.go`.
- Implement: a deterministic `GenerateUsers(n)` in the library, and a `package main` seed program carrying `//go:build ignore` that reuses it.
- Test: `TestGenerateUsers` proves the library builds and tests clean with the ignore file present; the seed program runs via `go run seed.go`.
- Verify: `go build ./...` and `go test ./...` ignore `seed.go` (no `main` in the build, no redeclaration); `go run seed.go -n 3` executes it; `go generate ./...` triggers the directive.

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/13-build-tags-for-test-separation/08-ignore-tag-seed-generator/cmd/demo
cd go-solutions/12-testing-ecosystem/13-build-tags-for-test-separation/08-ignore-tag-seed-generator
go mod edit -go=1.26
```

### Why ignore is the right tag, and how go run bypasses it

Put a bare `package main` file in the same directory as `package seeddemo` and the
build breaks: a directory may hold only one package, so `go build ./...` reports a
package-name conflict. You could move the seeder into its own `cmd/seed/`
directory, and that is fine for a permanent tool — but for a script that
conceptually belongs *beside* the code it seeds, `//go:build ignore` is the
idiomatic answer. `ignore` is a tag the toolchain never sets, so the file is
excluded from every package build: no name conflict, no stray `main`, nothing in
the release binary.

The part that makes it useful rather than merely inert is that naming a file
*explicitly* on the command line bypasses its build constraints. `go run seed.go`
compiles and runs that exact file even though `//go:build ignore` would exclude it
from a package build. This is the whole basis of the seed-and-generator idiom: the
file is invisible to `go build ./...` and `go test ./...`, yet directly runnable,
and referenceable from a `//go:generate` directive.

The seed program is not a throwaway `fmt.Println` — it reuses the library's
`GenerateUsers` so the seeded data and the tested data come from one source of
truth. That is the point of keeping it beside the package: the generator and the
production code share the same types and logic.

Create `user.go`:

```go
package seeddemo

import "fmt"

//go:generate go run seed.go -n 3

// User is a seed record.
type User struct {
	ID    int
	Email string
}

// GenerateUsers returns n deterministic seed users with ids 1..n, so a seed run
// and a test observe exactly the same data.
func GenerateUsers(n int) []User {
	users := make([]User, 0, n)
	for i := range n {
		id := i + 1
		users = append(users, User{ID: id, Email: fmt.Sprintf("user%d@example.com", id)})
	}
	return users
}
```

Create `seed.go` — the ignored seed program:

```go
//go:build ignore

package main

import (
	"flag"
	"fmt"

	"example.com/seeddemo"
)

func main() {
	n := flag.Int("n", 5, "number of users to seed")
	flag.Parse()

	for _, u := range seeddemo.GenerateUsers(*n) {
		fmt.Printf("INSERT INTO users (id, email) VALUES (%d, %q);\n", u.ID, u.Email)
	}
}
```

### The runnable demo

The demo is an ordinary `cmd/demo` command over the library — separate from the
seed program, to show that both consumers share the same `GenerateUsers`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/seeddemo"
)

func main() {
	for _, u := range seeddemo.GenerateUsers(2) {
		fmt.Printf("%d %s\n", u.ID, u.Email)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
1 user1@example.com
2 user2@example.com
```

### Running the seed program and the generator directive

`seed.go` is excluded from the package build but runs directly, and the
`//go:generate` directive in `user.go` invokes it:

```bash
go run seed.go -n 3
```

Expected output:

```text
INSERT INTO users (id, email) VALUES (1, "user1@example.com");
INSERT INTO users (id, email) VALUES (2, "user2@example.com");
INSERT INTO users (id, email) VALUES (3, "user3@example.com");
```

```bash
go generate ./...
```

`go generate` scans for `//go:generate` directives and runs each one, so this
executes `go run seed.go -n 3` from the directive in `user.go`. Neither
`go generate` nor the directive affects `go build`/`go test`, which continue to
ignore `seed.go`.

### The test

Create `seed_test.go`:

```go
package seeddemo

import (
	"fmt"
	"testing"
)

func TestGenerateUsers(t *testing.T) {
	t.Parallel()
	users := GenerateUsers(3)
	if len(users) != 3 {
		t.Fatalf("GenerateUsers(3) len = %d, want 3", len(users))
	}
	for i, u := range users {
		wantID := i + 1
		wantEmail := fmt.Sprintf("user%d@example.com", wantID)
		if u.ID != wantID || u.Email != wantEmail {
			t.Fatalf("users[%d] = %+v, want {%d %s}", i, u, wantID, wantEmail)
		}
	}
}

func TestGenerateUsersZero(t *testing.T) {
	t.Parallel()
	if got := GenerateUsers(0); len(got) != 0 {
		t.Fatalf("GenerateUsers(0) = %v, want empty", got)
	}
}

func ExampleGenerateUsers() {
	for _, u := range GenerateUsers(1) {
		fmt.Println(u.ID, u.Email)
	}
	// Output: 1 user1@example.com
}
```

## Review

The idiom holds when `go build ./...` and `go test ./...` succeed with `seed.go`
present — proof that `//go:build ignore` kept the second `package main` out of the
package build and out of the release binary — while `go run seed.go` still
executes it because an explicitly-named file bypasses its own constraint. The
value over a `cmd/seed/` directory is locality: the seed program sits beside the
types it seeds and shares `GenerateUsers` with the tests, so the seeded data can
never drift from the tested data. Keep the generator deterministic; a seed that
depends on wall-clock time or randomness makes both the demo output and any golden
test flaky.

## Resources

- [go command: Build constraints](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — the `ignore` convention and how named files bypass constraints.
- [go command: Generate Go files by processing source (go generate)](https://pkg.go.dev/cmd/go#hdr-Generate_Go_files_by_processing_source) — the `//go:generate` directive.
- [flag package](https://pkg.go.dev/flag) — the `-n` flag the seed program parses.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-platform-filename-constraints.md](07-platform-filename-constraints.md) | Next: [09-ci-tier-orchestration.md](09-ci-tier-orchestration.md)

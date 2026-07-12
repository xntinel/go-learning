# Exercise 3: Immutable Value Builder You Can Fork from a Shared Base

Sometimes you want to configure a builder *once* and derive many products from it
without the derivations stepping on each other. A base request spec pre-loaded with
an auth header and a base URL, from which every call site forks its own path and
body, is the real scenario. That only works if setters return a *copy* — a
value-receiver builder — and if the reference fields are deep-copied so no two forks
share a map.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
specfork/                   independent module: example.com/specfork
  go.mod                    go 1.26
  builder.go                package spec: Spec, Builder (value receiver), New, setters, Build
  cmd/
    demo/
      main.go               runnable demo: one base, two forked specs
  builder_test.go           fork-isolation tests, run under -race
```

- Files: `builder.go`, `cmd/demo/main.go`, `builder_test.go`.
- Implement: a `Builder` whose setters take a value receiver and return `Builder` (a copy), where `Header` clones the map before writing (copy-on-write) so forks never share storage. `Build` clones once more so the product owns its headers.
- Test: fork a base into two specs, build both, mutate one result's headers, and prove the other result and the base are unaffected; prove a header added to one fork is invisible to a sibling fork.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/11-builder-pattern-for-complex-structs/03-immutable-value-builder-fork/cmd/demo
cd go-solutions/07-structs-and-methods/11-builder-pattern-for-complex-structs/03-immutable-value-builder-fork
```

### Why a value receiver, and where the deep copy goes

A pointer-receiver builder mutates one shared instance: `base.Path("/x")` and
`base.Path("/y")` would both write into the *same* builder, and the second would
clobber the first. That is correct for a linear single-use chain and wrong for
forking. A value-receiver setter (`func (b Builder) Path(p string) Builder`)
operates on a copy of the struct and returns it, so `a := base.Path("/x")` and
`b := base.Path("/y")` are two independent builders that both inherit everything the
base had.

But "independent" needs one more step. Copying the `Builder` struct copies the map
*header*, not the entries, so both forks and the base initially point at the same
underlying map. A scalar setter (`Path`, `Body`, `Method`) is fine — it never
touches the map. The dangerous one is `Header`: if it wrote straight into the shared
map, adding a header to fork `a` would silently appear in fork `b` and in the base.
The fix is copy-on-write: `Header` clones the map with `maps.Clone`, writes into the
clone, stores the clone on the returned copy, and leaves every other holder pointing
at the original. `Build` clones one final time, so the produced `Spec` owns its
headers and no later fork operation can reach into it.

Create `builder.go`:

```go
package spec

import (
	"errors"
	"maps"
)

// ErrNoBaseURL is returned by Build when the base URL was never set.
var ErrNoBaseURL = errors.New("base URL is required")

// Spec is the fully-owned product: an outbound request description.
type Spec struct {
	BaseURL string
	Method  string
	Path    string
	Body    string
	Headers map[string]string
}

// Builder is a value type. Every setter returns a copy, so a configured base can
// be forked per call site without aliasing. Reference fields are copy-on-write.
type Builder struct {
	baseURL string
	method  string
	path    string
	body    string
	headers map[string]string
}

// New returns an empty value builder with an initialized headers map.
func New() Builder {
	return Builder{
		method:  "GET",
		headers: make(map[string]string),
	}
}

func (b Builder) BaseURL(u string) Builder {
	b.baseURL = u
	return b
}

func (b Builder) Method(m string) Builder {
	b.method = m
	return b
}

func (b Builder) Path(p string) Builder {
	b.path = p
	return b
}

func (b Builder) Body(body string) Builder {
	b.body = body
	return b
}

// Header is copy-on-write: it clones the map before writing so a header added to
// one fork is invisible to the base and to sibling forks.
func (b Builder) Header(key, value string) Builder {
	h := maps.Clone(b.headers)
	if h == nil {
		h = make(map[string]string)
	}
	h[key] = value
	b.headers = h
	return b
}

// Build validates and returns a Spec that owns its headers via a final clone.
func (b Builder) Build() (Spec, error) {
	if b.baseURL == "" {
		return Spec{}, ErrNoBaseURL
	}
	return Spec{
		BaseURL: b.baseURL,
		Method:  b.method,
		Path:    b.path,
		Body:    b.body,
		Headers: maps.Clone(b.headers),
	}, nil
}
```

### The runnable demo

The demo configures a base with an auth header and a base URL once, then forks two
independent specs from it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/specfork"
)

func main() {
	base := spec.New().
		BaseURL("https://api.example.com").
		Header("Authorization", "Bearer tok-123")

	users, _ := base.Method("GET").Path("/users").Build()
	create, _ := base.Method("POST").Path("/orders").Body(`{"item":"book"}`).Build()

	fmt.Printf("%s %s%s auth=%s\n", users.Method, users.BaseURL, users.Path, users.Headers["Authorization"])
	fmt.Printf("%s %s%s body=%s\n", create.Method, create.BaseURL, create.Path, create.Body)

	// Mutating one built spec's headers does not touch the sibling: Build cloned.
	users.Headers["X-Trace"] = "abc"
	fmt.Printf("users headers=%d create headers=%d\n", len(users.Headers), len(create.Headers))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET https://api.example.com/users auth=Bearer tok-123
POST https://api.example.com/orders body={"item":"book"}
users headers=2 create headers=1
```

Both specs inherited the base's `Authorization` header; adding `X-Trace` to the
`users` spec left `create` with its single header, because each `Build` handed back
an independently-owned map.

### Tests

The tests prove fork isolation from two directions: mutating a built result never
leaks (because `Build` clones), and adding a header to one fork never appears in a
sibling fork or the base (because `Header` is copy-on-write).

Create `builder_test.go`:

```go
package spec

import (
	"errors"
	"fmt"
	"testing"
)

func TestForkedResultsAreIndependent(t *testing.T) {
	t.Parallel()

	base := New().BaseURL("https://api.example.com").Header("Authorization", "tok")
	a, err := base.Path("/x").Build()
	if err != nil {
		t.Fatal(err)
	}
	b, err := base.Path("/y").Build()
	if err != nil {
		t.Fatal(err)
	}

	a.Headers["X-Only-A"] = "1"
	if _, ok := b.Headers["X-Only-A"]; ok {
		t.Fatal("mutation of a leaked into sibling b")
	}
	if a.Path == b.Path {
		t.Fatalf("forks should differ: a=%q b=%q", a.Path, b.Path)
	}
}

func TestHeaderOnForkDoesNotTouchBase(t *testing.T) {
	t.Parallel()

	base := New().BaseURL("https://api.example.com").Header("Authorization", "tok")
	fork := base.Header("X-Fork", "1")

	baseSpec, err := base.Build()
	if err != nil {
		t.Fatal(err)
	}
	forkSpec, err := fork.Build()
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := baseSpec.Headers["X-Fork"]; ok {
		t.Fatal("header added to fork leaked into base")
	}
	if _, ok := forkSpec.Headers["X-Fork"]; !ok {
		t.Fatal("fork lost its own header")
	}
	if _, ok := forkSpec.Headers["Authorization"]; !ok {
		t.Fatal("fork should inherit the base's Authorization header")
	}
}

func TestBuildRejectsMissingBaseURL(t *testing.T) {
	t.Parallel()

	if _, err := New().Path("/x").Build(); !errors.Is(err, ErrNoBaseURL) {
		t.Fatalf("err = %v, want ErrNoBaseURL", err)
	}
}

func ExampleBuilder_Build() {
	s, _ := New().BaseURL("https://example.com").Path("/health").Build()
	fmt.Println(s.Method, s.BaseURL+s.Path)
	// Output: GET https://example.com/health
}
```

## Review

The value builder is correct when a configured base can be forked without any fork
disturbing another or the base. `TestForkedResultsAreIndependent` mutates a built
result and proves siblings are untouched — the guarantee `Build`'s final
`maps.Clone` provides. `TestHeaderOnForkDoesNotTouchBase` adds a header to a fork
and proves the base never sees it while the fork still inherits the base's original
header — the guarantee `Header`'s copy-on-write provides. The pointer-builder
pitfall to avoid: had these setters used a pointer receiver, `base.Path("/x")` and
`base.Path("/y")` would mutate the same instance, and the two "forks" would be the
same object with the last-written path. The value receiver plus copy-on-write on the
map is what makes forking safe. Run `go test -race` to confirm no fork shares mutable
storage.

## Resources

- [maps.Clone](https://pkg.go.dev/maps#Clone) — the copy-on-write primitive that isolates forks.
- [Go spec: Method sets](https://go.dev/ref/spec#Method_sets) — why a value receiver copies and a pointer receiver aliases.
- [Effective Go: Pointers vs. Values](https://go.dev/doc/effective_go#pointers_vs_values) — receiver choice and its semantics.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-sql-select-query-builder.md](04-sql-select-query-builder.md)

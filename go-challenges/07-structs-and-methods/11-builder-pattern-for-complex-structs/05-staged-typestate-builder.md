# Exercise 5: Type-State Builder that Enforces Required Fields at Compile Time

When forgetting a required field is a real hazard, you can move the requirement out
of runtime validation and into the type system. A staged (type-state) builder
returns a *narrower* interface at each step, so "call Build before setting the URL"
is not an error you catch at runtime — it does not compile, because that method does
not exist on that stage's type.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
staged/                     independent module: example.com/staged
  go.mod                    go 1.26
  builder.go                package staged: stage interfaces, concrete builder, New, Build
  cmd/
    demo/
      main.go               runnable demo: a full staged chain
  builder_test.go           positive chain, defaults, runtime timeout check
```

- Files: `builder.go`, `cmd/demo/main.go`, `builder_test.go`.
- Implement: `New()` returns a `NeedsMethod` whose only method is `Method(...)` returning `NeedsURL`; `URL(...)` returns `ReadyBuilder` which finally exposes optional setters (`Header`, `Timeout`) and `Build() (Request, error)`. One unexported concrete struct implements all three stages.
- Test: a full staged chain compiles and `Build` succeeds with the expected `Request`; optional-field defaults apply; `Build` still enforces a runtime-only invariant (a non-positive timeout override is rejected).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/11-builder-pattern-for-complex-structs/05-staged-typestate-builder/cmd/demo
cd go-solutions/07-structs-and-methods/11-builder-pattern-for-complex-structs/05-staged-typestate-builder
```

### How the type system enforces ordering

Each construction stage is a distinct interface exposing only the methods legal at
that point. `New()` returns `NeedsMethod`, and the *only* method on `NeedsMethod`
is `Method`, which returns `NeedsURL`. The only method on `NeedsURL` is `URL`, which
returns `ReadyBuilder`. `ReadyBuilder` is where the optional setters and `Build`
finally appear. So the compiler will accept `New().Method("GET").URL("...").Build()`
but reject `New().Build()` and `New().URL("...")` — those methods are simply not in
the returned type's method set. One unexported concrete struct implements all three
interfaces; the staging is purely in *which interface* each method hands back, not
in separate structs.

This is the trade-off in miniature. You gain a compile-time guarantee that no caller
can build without a method and a URL — a whole class of runtime error deleted. You
pay with API surface: three exported interface types and a discipline about which
method returns which. It is worth it for a hard required-ordering invariant and is
over-engineering for a struct that is all optional fields, where a plain builder or
functional options are lighter. Note what type-state does *not* cover: invariants
that depend on the *value*, not on whether a field was set. A negative timeout is
still a runtime check in `Build`, because no type can express "positive duration".

The illegal ordering, shown as code that does *not* compile (do not add this to the
module — it is here to illustrate the guarantee):

```text
// Will not compile: NeedsMethod has no Build method.
r, err := staged.New().Build()

// Will not compile: NeedsURL has no Header method.
r, err := staged.New().Method("GET").Header("k", "v")
```

Create `builder.go`:

```go
package staged

import (
	"errors"
	"maps"
	"time"
)

// ErrNonPositiveTimeout is a runtime invariant the type system cannot express.
var ErrNonPositiveTimeout = errors.New("timeout must be positive")

// Request is the fully-owned product.
type Request struct {
	Method  string
	URL     string
	Headers map[string]string
	Timeout time.Duration
}

// The three construction stages. Each exposes only the methods legal at that
// point, so illegal orderings fail to compile.
type NeedsMethod interface {
	Method(m string) NeedsURL
}

type NeedsURL interface {
	URL(u string) ReadyBuilder
}

type ReadyBuilder interface {
	Header(key, value string) ReadyBuilder
	Timeout(d time.Duration) ReadyBuilder
	Build() (Request, error)
}

// builder is the single concrete type implementing all three stages.
type builder struct {
	method  string
	url     string
	headers map[string]string
	timeout time.Duration
}

// New returns the first stage. The default timeout is applied here so the
// optional Timeout setter is genuinely optional.
func New() NeedsMethod {
	return &builder{
		headers: make(map[string]string),
		timeout: 30 * time.Second,
	}
}

func (b *builder) Method(m string) NeedsURL {
	b.method = m
	return b
}

func (b *builder) URL(u string) ReadyBuilder {
	b.url = u
	return b
}

func (b *builder) Header(key, value string) ReadyBuilder {
	b.headers[key] = value
	return b
}

func (b *builder) Timeout(d time.Duration) ReadyBuilder {
	b.timeout = d
	return b
}

func (b *builder) Build() (Request, error) {
	if b.timeout <= 0 {
		return Request{}, ErrNonPositiveTimeout
	}
	return Request{
		Method:  b.method,
		URL:     b.url,
		Headers: maps.Clone(b.headers),
		Timeout: b.timeout,
	}, nil
}
```

### The runnable demo

The demo walks the full staged chain. The compiler is what guarantees the ordering;
the demo simply shows a valid path.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/staged"
)

func main() {
	r, err := staged.New().
		Method("POST").
		URL("https://api.example.com/orders").
		Header("Idempotency-Key", "abc-123").
		Timeout(5 * time.Second).
		Build()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("%s %s\n", r.Method, r.URL)
	fmt.Printf("idempotency=%s timeout=%s\n", r.Headers["Idempotency-Key"], r.Timeout)

	// A default-timeout request skips the optional Timeout stage entirely.
	d, _ := staged.New().Method("GET").URL("https://api.example.com/health").Build()
	fmt.Printf("default timeout=%s\n", d.Timeout)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
POST https://api.example.com/orders
idempotency=abc-123 timeout=5s
default timeout=30s
```

### Tests

The positive test walks the full chain and checks the product. The defaults test
proves the optional `Timeout` stage can be skipped. The runtime test proves `Build`
still enforces the value invariant the types cannot: a non-positive timeout override
is rejected via `ErrNonPositiveTimeout`.

Create `builder_test.go`:

```go
package staged

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestStagedChainBuilds(t *testing.T) {
	t.Parallel()

	r, err := New().
		Method("POST").
		URL("https://api.example.com/orders").
		Header("Idempotency-Key", "k1").
		Timeout(2 * time.Second).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != "POST" || r.URL != "https://api.example.com/orders" {
		t.Fatalf("method/url mismatch: %+v", r)
	}
	if r.Headers["Idempotency-Key"] != "k1" {
		t.Fatalf("header = %q", r.Headers["Idempotency-Key"])
	}
	if r.Timeout != 2*time.Second {
		t.Fatalf("timeout = %s", r.Timeout)
	}
}

func TestDefaultTimeoutWhenOptionalSkipped(t *testing.T) {
	t.Parallel()

	r, err := New().Method("GET").URL("https://api.example.com/health").Build()
	if err != nil {
		t.Fatal(err)
	}
	if r.Timeout != 30*time.Second {
		t.Fatalf("default timeout = %s, want 30s", r.Timeout)
	}
}

func TestBuildRejectsNonPositiveTimeout(t *testing.T) {
	t.Parallel()

	_, err := New().Method("GET").URL("https://x").Timeout(-1).Build()
	if !errors.Is(err, ErrNonPositiveTimeout) {
		t.Fatalf("err = %v, want ErrNonPositiveTimeout", err)
	}
}

func ExampleNew() {
	r, _ := New().Method("GET").URL("https://example.com").Build()
	fmt.Println(r.Method, r.URL, r.Timeout)
	// Output: GET https://example.com 30s
}
```

## Review

The staged builder is correct when the happy path compiles and builds, and when the
type system is doing the ordering work. `TestStagedChainBuilds` walks the full chain;
that it compiles at all is the guarantee — `New()` hands back `NeedsMethod`, so there
is no way to reach `Build` without passing through `Method` and `URL`. The two
illegal orderings shown above would fail `go build`, which is the point: the error
moved from runtime to compile time. `TestBuildRejectsNonPositiveTimeout` shows the
boundary of the technique — value invariants stay in `Build`, because no interface
can encode "positive duration". Reserve this ceremony for a genuine required-ordering
invariant; for an all-optional struct it is overkill. Run `go test -race` to confirm.

## Resources

- [Go spec: Interface types and method sets](https://go.dev/ref/spec#Interface_types) — why a returned interface constrains which methods are callable next.
- [Errors are values](https://go.dev/blog/errors-are-values) — the Go blog on treating errors and invariants as ordinary values rather than special control flow.
- [time.Duration](https://pkg.go.dev/time#Duration) — the optional timeout field's type.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-layered-config-builder.md](06-layered-config-builder.md)

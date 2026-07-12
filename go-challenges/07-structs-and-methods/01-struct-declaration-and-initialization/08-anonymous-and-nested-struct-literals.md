# Exercise 8: Anonymous Structs for Table Tests and a Nested Config Tree

Anonymous structs — struct types written inline with no name — earn their keep in
two real places: the case slice of a table-driven test, and a nested config tree
where a subsection (`HTTP.TLS`) is only ever used in one place. This module builds
both: a request validator tested with an anonymous `[]struct{...}` table, and a
nested `AppConfig` initialized with nested composite literals.

Fully self-contained: own `go mod init`, inline code, own demo and tests.

## What you'll build

```text
nestedcfg/                  independent module: example.com/nestedcfg
  go.mod                    go 1.24
  config.go                 nested AppConfig{HTTP{Addr; TLS{Cert,Key}}}; Load; Request; Validate
  cmd/
    demo/
      main.go               builds an AppConfig via nested literals, prints deep fields
  config_test.go            anonymous []struct table test; nested-literal config test
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: a nested `AppConfig` whose `HTTP` field is a named struct containing a nested `TLS` struct; a `Request` type and a `Validate(Request) (int, error)` returning an HTTP status; a `Load` that returns a default config.
- Test: a table-driven test whose cases are an anonymous `[]struct{name string; in Request; want int}` iterated with `t.Run`; a config test that builds an `AppConfig` via nested literals and asserts deep access (`cfg.HTTP.TLS.Cert`), plus that an omitted inner struct stays at its zero value.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/01-struct-declaration-and-initialization/08-anonymous-and-nested-struct-literals/cmd/demo
cd go-solutions/07-structs-and-methods/01-struct-declaration-and-initialization/08-anonymous-and-nested-struct-literals
go mod edit -go=1.24
```

### When an anonymous struct is the right tool, and when to name it

An anonymous struct type is written inline: `struct{ Addr string }`. It has no name,
so you cannot refer to it elsewhere or write a method on it. That is exactly why it
is right for a value used in one place and wrong for a value passed around.

The archetypal good use is the **table-test case slice**:
`[]struct{ name string; in Request; want int }{ ... }`. These cases exist only for
this test, never escape it, and naming the type would be noise. You build the slice
inline, range it, and call `t.Run(tc.name, ...)` so each case is a named subtest
that can fail and be run independently.

The second good use is a **nested config subsection**. `AppConfig.HTTP.TLS` groups
`Cert` and `Key`; if that grouping is only meaningful inside `HTTP`, an anonymous
nested struct keeps it local. The trade-off: the moment another type needs to
accept a `TLS` value, or you want to write a method on it, or it appears in more
than one place, promote it to a **named type**. In this module `HTTP` and `TLS` are
named (so the demo, a separate package, can construct them field by field), which
shows the promotion decision; the *literal* nesting is what the exercise
highlights. An omitted inner struct in a composite literal stays at its zero value
— `AppConfig{HTTP: HTTP{Addr: ":80"}}` leaves `HTTP.TLS` as a zero `TLS{}`.

Create `config.go`:

```go
package config

import (
	"errors"
	"net/http"
)

// ErrInvalidRequest is returned by Validate for a malformed request.
var ErrInvalidRequest = errors.New("invalid request")

// TLS is a named nested subsection: cert and key paths.
type TLS struct {
	Cert string
	Key  string
}

// HTTP groups the HTTP-server settings and nests a TLS subsection.
type HTTP struct {
	Addr string
	TLS  TLS
}

// AppConfig is the root config tree, initialized with nested composite literals.
type AppConfig struct {
	HTTP HTTP
	Name string
}

// Load returns the default configuration, built with nested literals.
func Load() AppConfig {
	return AppConfig{
		Name: "api",
		HTTP: HTTP{
			Addr: ":8080",
			TLS: TLS{
				Cert: "/etc/tls/cert.pem",
				Key:  "/etc/tls/key.pem",
			},
		},
	}
}

// TLSEnabled reports whether both cert and key are set.
func (c AppConfig) TLSEnabled() bool {
	return c.HTTP.TLS.Cert != "" && c.HTTP.TLS.Key != ""
}

// Request is a minimal inbound request to validate.
type Request struct {
	Method string
	Path   string
	Body   int // content length
}

// Validate returns an HTTP status code for the request and an error when it is
// rejected. It is the function the anonymous-struct table test exercises.
func Validate(r Request) (int, error) {
	if r.Path == "" {
		return http.StatusBadRequest, ErrInvalidRequest
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		return http.StatusMethodNotAllowed, ErrInvalidRequest
	}
	if r.Method == http.MethodPost && r.Body == 0 {
		return http.StatusBadRequest, ErrInvalidRequest
	}
	return http.StatusOK, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/nestedcfg"
)

func main() {
	cfg := config.Load()
	fmt.Printf("name=%s addr=%s\n", cfg.Name, cfg.HTTP.Addr)
	fmt.Printf("cert=%s tls=%t\n", cfg.HTTP.TLS.Cert, cfg.TLSEnabled())

	// A config with the inner TLS omitted: it stays at its zero value.
	plain := config.AppConfig{HTTP: config.HTTP{Addr: ":80"}}
	fmt.Printf("plain tls=%t cert=%q\n", plain.TLSEnabled(), plain.HTTP.TLS.Cert)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
name=api addr=:8080
cert=/etc/tls/cert.pem tls=true
plain tls=false cert=""
```

### Tests

`TestValidate` is the anonymous-struct table test: the cases are an inline
`[]struct{...}` slice, each run as a named subtest via `t.Run`, asserting both the
status code and, for the failures, the wrapped sentinel with `errors.Is`.
`TestNestedLiteral` builds an `AppConfig` with nested composite literals and reads
`cfg.HTTP.TLS.Cert` three levels deep, then builds one with the inner `TLS` omitted
and asserts it is the zero value.

Create `config_test.go`:

```go
package config

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      Request
		want    int
		wantErr bool
	}{
		{"ok get", Request{Method: "GET", Path: "/x"}, http.StatusOK, false},
		{"ok post", Request{Method: "POST", Path: "/x", Body: 12}, http.StatusOK, false},
		{"empty path", Request{Method: "GET", Path: ""}, http.StatusBadRequest, true},
		{"bad method", Request{Method: "DELETE", Path: "/x"}, http.StatusMethodNotAllowed, true},
		{"empty post body", Request{Method: "POST", Path: "/x", Body: 0}, http.StatusBadRequest, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Validate(tc.in)
			if got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
			if tc.wantErr && !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
		})
	}
}

func TestNestedLiteral(t *testing.T) {
	t.Parallel()
	cfg := AppConfig{
		Name: "svc",
		HTTP: HTTP{
			Addr: ":9000",
			TLS:  TLS{Cert: "c.pem", Key: "k.pem"},
		},
	}
	if cfg.HTTP.TLS.Cert != "c.pem" {
		t.Fatalf("deep field = %q, want c.pem", cfg.HTTP.TLS.Cert)
	}
	if !cfg.TLSEnabled() {
		t.Fatal("TLSEnabled should be true when cert and key are set")
	}
}

func TestOmittedInnerStructIsZero(t *testing.T) {
	t.Parallel()
	cfg := AppConfig{HTTP: HTTP{Addr: ":80"}}
	var zero TLS
	if cfg.HTTP.TLS != zero {
		t.Fatalf("omitted TLS = %+v, want zero value", cfg.HTTP.TLS)
	}
	if cfg.TLSEnabled() {
		t.Fatal("TLSEnabled should be false when TLS is omitted")
	}
}

func ExampleLoad() {
	cfg := Load()
	fmt.Println(cfg.HTTP.Addr, cfg.TLSEnabled())
	// Output: :8080 true
}
```

## Review

The two uses land the same lesson from opposite directions. The table test shows
the anonymous struct at its best: a set of fields used exactly once, built inline,
never named — and `t.Run(tc.name, ...)` turns each row into an independent subtest.
The nested config shows composite literals initializing a tree in one expression,
with the rule that an omitted inner struct is its zero value, not an error. The
judgment the module trains is when to stop using an anonymous struct: the instant a
struct shape is needed in more than one place, accepted by a function, or given a
method, name it — which is why `HTTP` and `TLS` here are named types even though
their *literals* nest. Run `go test -race` and `go vet`; `go vet` will flag an
unkeyed literal of these types if you slip into the positional form.

## Resources

- [Go Spec: struct types (anonymous structs)](https://go.dev/ref/spec#Struct_types) — inline struct type syntax.
- [Effective Go: composite literals](https://go.dev/doc/effective_go#composite_literals) — nested literal initialization.
- [Table-driven tests](https://go.dev/wiki/TableDrivenTests) — the anonymous `[]struct{...}` case-slice idiom.
- [`testing.T.Run`](https://pkg.go.dev/testing#T.Run) — subtests from table cases.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-self-referential-structs-lru-node.md](09-self-referential-structs-lru-node.md)

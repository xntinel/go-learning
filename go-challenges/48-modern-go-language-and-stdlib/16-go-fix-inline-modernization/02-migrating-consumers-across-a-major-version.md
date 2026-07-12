# Exercise 2: Zero-Flag-Day Migration to a v2 Package

The hardest API change is not renaming a function; it is *moving* it. You are
splitting a monolithic SDK package: the URL-building logic that lived in the old
`legacyclient` package now belongs in a focused `client` package. A cross-package
`//go:fix inline` forwarder lets consumers migrate the import path *and* the call
sites with one `go fix`, exactly the way the standard library migrates
`io/ioutil.ReadFile` callers to `os.ReadFile`.

This module is self-contained: an old package, its successor, a consumer that
still imports the old path, a demo, and tests. Nothing here imports another
exercise.

## What you'll build

```text
svc/                           independent module: example.com/svc
  go.mod                       go 1.26
  client/
    client.go                  Endpoint (the successor API)
  legacyclient/
    legacyclient.go            BuildURL (Deprecated, //go:fix inline forwarder to client.Endpoint)
    legacyclient_test.go       behavioral-equivalence tests + Example
  consumer/
    consumer.go                imports legacyclient (what go fix rewrites)
  cmd/
    demo/
      main.go                  runnable demo of the deprecated forwarder
```

- Files: `client/client.go`, `legacyclient/legacyclient.go`, `consumer/consumer.go`, `cmd/demo/main.go`, `legacyclient/legacyclient_test.go`.
- Implement: `client.Endpoint(base, path)` in the new package, and a `Deprecated`, `//go:fix inline`-annotated `legacyclient.BuildURL(base, path)` that forwards to it.
- Test: a table-driven test asserting the old forwarder and the new function return identical results; an `Example`.
- Verify: `go test -count=1 -race ./...`, then `go fix -diff ./...`.

Set up the module:

```bash
go mod edit -go=1.26
```

### A package split, and how the inliner rewrites the import

When a symbol moves to a new package, a consumer needs two edits: the call must
name the new package, and the import must point to it. Hand-doing that across
dozens of files is where regex codemods corrupt import blocks. The inliner does
both automatically, because inlining a cross-package forwarder pastes the callee's
body — which references the *new* package — into the caller, and then rewrites the
caller's imports to include what the pasted body needs and drop what it no longer
uses. This is precisely the mechanism behind the standard library's own
`io/ioutil.ReadFile` becomes `os.ReadFile` migration.

The successor package holds the real logic. `Endpoint` joins a base URL and a
path with `net/url`, which cleans the result correctly (unlike string
concatenation).

Create `client/client.go`:

```go
// Package client is the successor to legacyclient. It holds the URL-building
// logic that used to live in legacyclient.
package client

import "net/url"

// Endpoint joins base and path into a single service URL, resolving separators
// the way net/url does rather than by string concatenation.
func Endpoint(base, path string) (string, error) {
	return url.JoinPath(base, path)
}
```

The old package keeps a forwarder so today's consumers still compile. `BuildURL`
now does nothing but call `client.Endpoint`; it carries a `Deprecated:` paragraph
and, immediately above the declaration, `//go:fix inline`. Because the body
references only the exported `client.Endpoint`, it inlines cleanly into any
consumer.

Create `legacyclient/legacyclient.go`:

```go
// Package legacyclient is the pre-split SDK package. Its URL logic moved to
// package client; the functions here are thin, inlinable forwarders.
package legacyclient

import "example.com/svc/client"

// BuildURL joins base and path into a service URL.
//
// Deprecated: the URL logic moved to package client. Use [client.Endpoint].
// BuildURL is a thin forwarder kept for one release so consumers can run
// `go fix`.
//
//go:fix inline
func BuildURL(base, path string) (string, error) {
	return client.Endpoint(base, path)
}
```

### The consumer that still imports the old path

The consumer imports `example.com/svc/legacyclient` and calls `BuildURL`. This is
the code a downstream team owns and has not yet migrated; it compiles and works
because the forwarder still exists.

Create `consumer/consumer.go`:

```go
// Package consumer still imports the old legacyclient path. Running
// `go fix ./...` rewrites both the import and the call sites to package client.
package consumer

import "example.com/svc/legacyclient"

// UserURL builds the canonical URL for a user resource.
func UserURL(id string) (string, error) {
	return legacyclient.BuildURL("https://api.example.com", "users/"+id)
}

// OrgURL builds the canonical URL for an org resource.
func OrgURL(id string) (string, error) {
	return legacyclient.BuildURL("https://api.example.com", "orgs/"+id)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/svc/legacyclient"
)

func main() {
	u, err := legacyclient.BuildURL("https://api.example.com", "users/42")
	fmt.Printf("BuildURL -> %s (err=%v)\n", u, err)

	o, err := legacyclient.BuildURL("https://api.example.com", "orgs/7")
	fmt.Printf("BuildURL -> %s (err=%v)\n", o, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
BuildURL -> https://api.example.com/users/42 (err=<nil>)
BuildURL -> https://api.example.com/orgs/7 (err=<nil>)
```

### Proving the forward is faithful

As in Exercise 1, you owe consumers proof that the forwarder is a true no-op over
the move. The test runs each input through both `BuildURL` and the exact
expression it forwards to, `client.Endpoint`, and asserts identical results and
errors.

Create `legacyclient/legacyclient_test.go`:

```go
package legacyclient

import (
	"fmt"
	"testing"

	"example.com/svc/client"
)

func TestBuildURLMatchesEndpoint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		base string
		path string
	}{
		{"simple", "https://api.example.com", "users/42"},
		{"trailing slash on base", "https://api.example.com/", "orgs/7"},
		{"leading slash on path", "https://api.example.com", "/health"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wURL, wErr := BuildURL(tc.base, tc.path)
			eURL, eErr := client.Endpoint(tc.base, tc.path)

			if wURL != eURL {
				t.Fatalf("BuildURL = %q, client.Endpoint = %q", wURL, eURL)
			}
			if (wErr == nil) != (eErr == nil) {
				t.Fatalf("error mismatch: BuildURL=%v, Endpoint=%v", wErr, eErr)
			}
		})
	}
}

func Example() {
	u, _ := BuildURL("https://api.example.com", "users/42")
	fmt.Println(u)
	// Output: https://api.example.com/users/42
}
```

### The migration, as a diff

From the module root:

```bash
go fix -diff ./...
```

The inliner rewrites the consumer to use the successor package directly — both the
import and every call:

```text
--- consumer/consumer.go (old)
+++ consumer/consumer.go (new)
-import "example.com/svc/legacyclient"
+import "example.com/svc/client"
 
 // UserURL builds the canonical URL for a user resource.
 func UserURL(id string) (string, error) {
-	return legacyclient.BuildURL("https://api.example.com", "users/"+id)
+	return client.Endpoint("https://api.example.com", "users/"+id)
 }
 
 // OrgURL builds the canonical URL for an org resource.
 func OrgURL(id string) (string, error) {
-	return legacyclient.BuildURL("https://api.example.com", "orgs/"+id)
+	return client.Endpoint("https://api.example.com", "orgs/"+id)
 }
```

The import flips from `legacyclient` to `client` because the pasted body names
`client.Endpoint`, and the old import is dropped once nothing references it. The
same `go fix -diff ./...` check from Exercise 1 gates CI: fail the build while any
consumer's diff is non-empty, then delete `BuildURL` in a later release once every
diff is clean.

### The same shape for a semver-major /v2 bump

A true major-version bump follows the identical mechanism, only the target's
import path ends in `/v2` and lives in a separate module. In the old module you
would write a forwarder to the v2 import path:

```go
import clientv2 "example.com/svc/v2/client"

// Deprecated: use the v2 client. See example.com/svc/v2.
//
//go:fix inline
func BuildURL(base, path string) (string, error) {
	return clientv2.Endpoint(base, path)
}
```

Running `go fix` then rewrites consumers to import `example.com/svc/v2/client` and
call `clientv2.Endpoint`. The one extra obligation is a `go.mod` one: the v2
module must be a real, importable dependency of the consumer (the inliner rewrites
the import, but it cannot add a module requirement for you), so `go get
example.com/svc/v2` is part of the migration. This exercise keeps a single
buildable module by splitting into a sibling package rather than a `/v2` module,
but the inliner's import rewrite is exactly the same.

## Review

Correctness here is the same discipline as Exercise 1, applied across a package
boundary: `BuildURL` must be observably identical to `client.Endpoint`, which
`TestBuildURLMatchesEndpoint` verifies over inputs that stress separator handling,
so the cross-package inline is safe to fan out. Building the URL with
`url.JoinPath` rather than string concatenation is what keeps the two paths equal
under trailing and leading slashes.

The traps specific to package moves: remember the inliner rewrites the *import*,
not the *module requirement* — for a `/v2` move the consumer must be able to
import the new module, so `go.mod` gains a requirement as part of the migration.
Keep the forwarder's body referencing only exported successor-package symbols, or
the inlined consumer will not compile. And, as always, keep the old package alive
for a release after annotating it; the directive enables the move but does not
perform it. Confirm with `go test -race ./...` and preview with
`go fix -diff ./...`.

## Resources

- [Automating your API migrations with go fix inline](https://go.dev/blog/inliner) — cross-package inlining and the `io/ioutil` becomes `os` import rewrite.
- [`net/url.JoinPath`](https://pkg.go.dev/net/url#JoinPath) — the standard joiner used by the successor package.
- [Go Modules: v2 and beyond](https://go.dev/blog/v2-go-modules) — why a major bump uses a `/v2` import path and a separate module.

---

Back to [01-deprecate-and-inline-a-function-api.md](01-deprecate-and-inline-a-function-api.md) | Next: [03-inline-constants-and-type-aliases.md](03-inline-constants-and-type-aliases.md)

# Exercise 2: Push Policy

What to push is the only application-specific decision in the entire push mechanism, and this module isolates it. A `Policy` is a map from request paths to the resources their responses reference, with exact matches taking priority over prefix matches so a specific rule can override a general one.

This module is fully self-contained: it has its own `go mod init`, all code inline, and its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
push-policy/
  go.mod
  policy.go            Policy, ResourcesFor
  policy_test.go       exact match, prefix match, exact-beats-prefix, no match, nil map
  cmd/demo/main.go     run four request paths through a policy
```

- Files: `policy.go`, `policy_test.go`, `cmd/demo/main.go`.
- Implement: `Policy` (a `map[string][]string`) with `ResourcesFor(requestPath string) []string`.
- Test: an exact key returns its resources; a prefix key matches longer paths; an exact key wins over a prefix key; an unmatched path and a nil policy both return nil.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/44-capstone-http2-implementation/04-server-push/02-push-policy/cmd/demo && cd go-solutions/44-capstone-http2-implementation/04-server-push/02-push-policy
go mod edit -go=1.26
```

### Exact match before prefix match

`ResourcesFor` answers one question: given the path a client requested, which resource paths should the server push alongside the response. It tries an exact map lookup first, and only if that misses does it scan for a key that is a prefix of the request path. The ordering is deliberate and it is what lets a map — whose iteration order is randomized — behave deterministically for the case that matters. A rule for `/api/` can push a schema for every endpoint under it, while a more specific rule keyed on the exact path `/api/schema.json` overrides that general rule, and because the exact lookup happens before the prefix scan the override never depends on which key the range loop visits first.

The nil-map guard at the top matters because the zero value of a `Policy` is a nil map, and a server with no push configuration should push nothing rather than panic. Returning `nil` for both the no-match and the nil-policy cases gives the caller a single, uniform "nothing to push" signal it can test with `len(resources) == 0`. The prefix scan is linear in the number of policy keys, which is fine for the handful of rules a real policy holds; a policy with thousands of prefix rules would reach for a trie instead, but that is a different module with a different cost model.

Create `policy.go`:

```go
package push

import "strings"

// Policy maps incoming request paths to lists of resource paths to push.
// An exact path match is tried first; if that fails, a prefix match against
// each key is used. The zero value is valid and pushes nothing.
//
// Example:
//
//	p := Policy{
//		"/index.html": {"/style.css", "/app.js"},
//		"/api/":       {"/api/schema.json"},
//	}
type Policy map[string][]string

// ResourcesFor returns the resource paths to push for the given requestPath.
// Exact match takes priority over prefix match. Returns nil if no pattern
// matches.
func (p Policy) ResourcesFor(requestPath string) []string {
	if p == nil {
		return nil
	}
	if resources, ok := p[requestPath]; ok {
		return resources
	}
	for pattern, resources := range p {
		if strings.HasPrefix(requestPath, pattern) {
			return resources
		}
	}
	return nil
}
```

### The runnable demo

The demo defines a three-rule policy and runs four request paths through it: two exact matches, one prefix match (`/api/users` matches the `/api/` rule), and one path that matches nothing.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	push "example.com/push-policy"
)

func main() {
	policy := push.Policy{
		"/index.html": {"/style.css", "/app.js"},
		"/about.html": {"/style.css"},
		"/api/":       {"/api/schema.json"},
	}

	fmt.Println("Push policy decisions:")
	for _, path := range []string{"/index.html", "/about.html", "/api/users", "/login"} {
		resources := policy.ResourcesFor(path)
		if len(resources) == 0 {
			fmt.Printf("  %-22s no push\n", path)
		} else {
			fmt.Printf("  %-22s push %v\n", path, resources)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Push policy decisions:
  /index.html            push [/style.css /app.js]
  /about.html            push [/style.css]
  /api/users             push [/api/schema.json]
  /login                 no push
```

`/api/users` has no exact rule, so it falls through to the prefix scan and matches `/api/`; `/login` matches neither and yields no push.

### Tests

The tests cover the four behaviors a policy must get right: an exact key returns its list, a prefix key matches a longer path, an exact key wins over a competing prefix key, and both an unmatched path and a nil policy return nil. The `Example` function doubles as executable documentation, verified by `go test` against its `// Output` comment.

Create `policy_test.go`:

```go
package push

import (
	"fmt"
	"testing"
)

func ExamplePolicy_ResourcesFor() {
	p := Policy{
		"/index.html": {"/style.css", "/app.js"},
	}
	resources := p.ResourcesFor("/index.html")
	fmt.Println(len(resources))
	fmt.Println(resources[0])
	fmt.Println(resources[1])
	// Output:
	// 2
	// /style.css
	// /app.js
}

func TestPolicyExactMatch(t *testing.T) {
	t.Parallel()

	p := Policy{"/index.html": {"/style.css", "/app.js"}}
	got := p.ResourcesFor("/index.html")
	if len(got) != 2 || got[0] != "/style.css" || got[1] != "/app.js" {
		t.Fatalf("ResourcesFor(/index.html) = %v", got)
	}
}

func TestPolicyPrefixMatch(t *testing.T) {
	t.Parallel()

	p := Policy{"/api/": {"/api/schema.json"}}
	got := p.ResourcesFor("/api/users")
	if len(got) != 1 || got[0] != "/api/schema.json" {
		t.Fatalf("ResourcesFor(/api/users) = %v", got)
	}
}

func TestPolicyExactBeatsPrefix(t *testing.T) {
	t.Parallel()

	p := Policy{
		"/api/":            {"/api/schema.json"},
		"/api/schema.json": {"/api/extra.json"},
	}
	got := p.ResourcesFor("/api/schema.json")
	if len(got) != 1 || got[0] != "/api/extra.json" {
		t.Fatalf("exact match must win over prefix: got %v", got)
	}
}

func TestPolicyNoMatch(t *testing.T) {
	t.Parallel()

	p := Policy{"/index.html": {"/style.css"}}
	if got := p.ResourcesFor("/404.html"); got != nil {
		t.Fatalf("ResourcesFor(/404.html) = %v, want nil", got)
	}
}

func TestNilPolicyReturnsNil(t *testing.T) {
	t.Parallel()

	var p Policy
	if got := p.ResourcesFor("/anything"); got != nil {
		t.Fatalf("nil Policy.ResourcesFor = %v, want nil", got)
	}
}
```

## Review

The policy is correct when the exact lookup precedes the prefix scan — `TestPolicyExactBeatsPrefix` is the test that would fail if the order were reversed or if the prefix scan happened to win on a given map iteration. The other properties are the nil-map guard (a zero-value policy must not panic) and the uniform nil return for "nothing to push" so callers have one condition to check. The subtle trap is assuming map iteration order is stable: it is not, so any policy logic that depends on visiting keys in a particular order is wrong, which is exactly why the exact-match lookup is separated from the prefix scan rather than folded into it.

## Resources

- [RFC 9113 §8.4 — Server Push](https://httpwg.org/specs/rfc9113.html#PushResources) — the constraints on what may be pushed, the policy operates within these.
- [`strings.HasPrefix`](https://pkg.go.dev/strings#HasPrefix) — the prefix test behind the fallback scan.
- [Go maps in action](https://go.dev/blog/maps) — why map iteration order is randomized and what that means for deterministic lookups.

---

Back to [01-push-promise-frame.md](01-push-promise-frame.md) | Next: [03-push-tracker.md](03-push-tracker.md)

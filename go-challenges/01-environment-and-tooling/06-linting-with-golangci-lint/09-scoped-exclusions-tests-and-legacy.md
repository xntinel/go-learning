# Exercise 9: Scoped Exclusions for Tests, Generated, and Legacy Code

Sometimes a finding is acceptable in one place and not another. The wrong fix is to
disable the linter for the whole module; the right fix is a narrow exclusion keyed on
path, linter, and text. This exercise builds an in-memory store, tolerates a terse
unchecked call in its test seed helper via a scoped `linters.exclusions.rule`, and
shows that the identical pattern in production still fails the gate.

This module is self-contained: its own `go mod init`, a `store` package, a demo, and
a test whose seed helper is the thing being excluded.

## What you'll build

```text
scopedexcl/                   independent module: example.com/scopedexcl
  go.mod                      go 1.24
  store.go                    concurrency-safe Store; Set/Get; ErrEmptyKey
  store_test.go               seed helper (terse, excluded) + behavior table
  cmd/
    demo/
      main.go                 sets and reads a couple of keys
  .golangci.yml               scoped exclusions.rules (shown in prose)
```

- Files: `store.go`, `store_test.go`, `cmd/demo/main.go`, plus the config in prose.
- Implement: a `Store` with `Set(key, value) error` (rejecting empty keys with `ErrEmptyKey`) and `Get(key) (string, bool)`.
- Test: a `seed` helper that sets keys tersely (the excluded pattern) plus a behavior table.
- Verify: `go test -count=1 -race ./...`; then `golangci-lint run ./...` with the scoped rule.

Set up the module:

```bash
mkdir -p ~/go-exercises/scopedexcl/cmd/demo
cd ~/go-exercises/scopedexcl
go mod init example.com/scopedexcl
```

### Narrow the exclusion, never the linter

The anti-pattern is to hit an `errcheck` finding in a test helper and remove
`errcheck` from the enable list. That silences the test *and* strips the check from
all of production — the exact code where an unchecked error is an incident. The
correct move is a rule under `linters.exclusions.rules` keyed on `path: _test\.go`
and `linters: [errcheck]`, so the relaxation applies only to test files. Production
code keeps full coverage; tests get to use terse seed helpers that discard errors
that cannot meaningfully fail in a controlled fixture.

The rule can be narrowed further with `text` (a regex matched against the finding
message) or `source` (a regex matched against the offending line), so you exclude a
*specific* finding rather than a whole linter even within the scoped path. v2 also
ships curated exclusion *presets* — `comments`, `std-error-handling`,
`common-false-positives`, `legacy` — which you opt into deliberately, and explicit
generated-code handling via `linters.exclusions.generated: lax | strict | disable`,
so machine-written files (`*.pb.go`, mocks) do not drown the gate. The through-line:
every relaxation is scoped and visible, so production never quietly loses a check.

Create `store.go` — production code, fully checked:

```go
package store

import (
	"errors"
	"sync"
)

// ErrEmptyKey is returned by Set when the key is empty.
var ErrEmptyKey = errors.New("empty key")

// Store is a concurrency-safe in-memory string map.
type Store struct {
	mu sync.RWMutex
	m  map[string]string
}

// New returns an empty Store.
func New() *Store {
	return &Store{m: make(map[string]string)}
}

// Set stores value under key. An empty key returns ErrEmptyKey.
func (s *Store) Set(key, value string) error {
	if key == "" {
		return ErrEmptyKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = value
	return nil
}

// Get returns the value under key and whether it was present.
func (s *Store) Get(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[key]
	return v, ok
}
```

### The config

Create `.golangci.yml`. The exclusion is scoped to test files only:

```yaml
version: "2"

linters:
  default: none
  enable:
    - errcheck
    - govet
    - staticcheck
  exclusions:
    generated: lax
    rules:
      # Test seed helpers may discard errors that cannot fail in a fixture.
      # This relaxes errcheck for _test.go ONLY; production keeps full coverage.
      - path: _test\.go
        linters:
          - errcheck
```

To see that the scope is real, add the identical terse `s.Set("k", "v")` (discarding
the error) into `store.go` and run the linter: production still fails with an
`errcheck` finding, because the rule matches only `_test.go`. Toggle the rule off and
both the production *and* the test occurrences reappear. That contrast — one fails,
one is excluded, and only by path — is the property a module-wide disable would
destroy.

```bash
golangci-lint run ./...
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/scopedexcl"
)

func main() {
	s := store.New()
	if err := s.Set("region", "eu-west-1"); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if v, ok := s.Get("region"); ok {
		fmt.Printf("region = %s\n", v)
	}
	if _, ok := s.Get("missing"); !ok {
		fmt.Println("missing = <absent>")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
region = eu-west-1
missing = <absent>
```

### Tests

Create `store_test.go`. The `seed` helper sets keys tersely, discarding the `Set`
error on purpose — this is the pattern the scoped exclusion permits *in tests*. The
behavior table then checks presence and the empty-key rejection, and `errors.Is`
matches the sentinel.

```go
package store

import (
	"errors"
	"testing"
)

// seed populates s from a map, discarding Set errors. In a controlled fixture
// these keys are non-empty, so Set cannot fail; the discarded return is what the
// scoped errcheck exclusion for _test.go permits.
func seed(s *Store, kv map[string]string) {
	for k, v := range kv {
		s.Set(k, v) // unchecked on purpose; excluded for _test.go
	}
}

func TestStore(t *testing.T) {
	t.Parallel()

	s := New()
	seed(s, map[string]string{"a": "1", "b": "2"})

	tests := []struct {
		name    string
		key     string
		wantVal string
		wantOK  bool
	}{
		{name: "present a", key: "a", wantVal: "1", wantOK: true},
		{name: "present b", key: "b", wantVal: "2", wantOK: true},
		{name: "absent", key: "z", wantVal: "", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := s.Get(tc.key)
			if ok != tc.wantOK || got != tc.wantVal {
				t.Fatalf("Get(%q) = %q,%v; want %q,%v", tc.key, got, ok, tc.wantVal, tc.wantOK)
			}
		})
	}
}

func TestSetEmptyKey(t *testing.T) {
	t.Parallel()

	s := New()
	if err := s.Set("", "x"); !errors.Is(err, ErrEmptyKey) {
		t.Fatalf("Set(\"\", _) err = %v, want ErrEmptyKey", err)
	}
}
```

## Review

The exclusion is correct when it is scoped so tightly that the same finding fails in
production and is excused only in `_test.go` — proven by adding the terse `Set` to
`store.go` and watching the gate reject it while the test seed helper passes. Keying
on `path` (and optionally `text`/`source`) is what keeps production coverage intact;
dropping the linter from the enable list would trade a test-only nuisance for a
production blind spot. The mistakes to avoid: disabling a linter module-wide to
silence a test, writing an exclusion so broad (`path: .` or no `linters:` list) that
it excuses everything, and forgetting generated code — set
`exclusions.generated` and, where needed, a rule for `*.pb.go` so machine output does
not either flood the gate or, worse, get held to hand-written standards. The `store`
code is deliberately clean; the artifact is the scoped rule.

## Resources

- [golangci-lint: False Positives / exclusions](https://golangci-lint.run/docs/configuration/false-positives/) — `exclusions.rules`, presets, and generated handling.
- [golangci-lint: Configuration File (v2)](https://golangci-lint.run/docs/configuration/file/) — the `linters.exclusions` schema.
- [errcheck](https://github.com/kisielk/errcheck) — the check being scoped to tests here.

---

Back to [08-disciplined-nolint-with-nolintlint.md](08-disciplined-nolint-with-nolintlint.md) | Next: [10-incremental-lint-and-ci-gating.md](10-incremental-lint-and-ci-gating.md)

# Exercise 5: Package-Level Variable Initialization Order for Precompiled Validators

Precompiling regexps and deriving dependent package-level values at load time is a
real, common pattern — and it is where the initialization-order rules bite. This
exercise builds a validation package whose vars depend on each other across files,
proving that Go initializes them in dependency order (not source order), and that
assuming source order is a latent zero-value bug.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
validate/                    independent module: example.com/validate
  go.mod                     module example.com/validate
  patterns.go                precompiled *regexp.Regexp vars (email, slug, semver)
  rules.go                   derived vars that depend on patterns.go vars (order proof)
  validate.go                ValidateUser using the precompiled patterns
  cmd/demo/main.go           validates a couple of records
  validate_test.go           table tests: patterns non-nil, match/reject, dependency-order proof
```

Files: `patterns.go`, `rules.go`, `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
Implement: package-level `regexp.MustCompile` patterns and a derived allow-list var that depends on a var declared in another file, plus a `ValidateUser` that uses them.
Test: each precompiled regexp is non-nil and matches/rejects known inputs; the derived var is correct despite being declared "before" its dependency in source.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/08-init-functions-and-package-initialization/05-package-var-init-order-precompiled-validators/cmd/demo
cd go-solutions/04-functions/08-init-functions-and-package-initialization/05-package-var-init-order-precompiled-validators
```

### Why precompile, and why order is not source order

Compiling a regexp is expensive relative to matching one, and a static pattern has
a correct answer known at build time. So the idiom is `regexp.MustCompile` at
package scope: the pattern compiles exactly once during package initialization, and
a malformed pattern panics at load — a build-time mistake caught the instant the
binary starts, never per request. The alternative failure modes are both real
bugs: `regexp.Compile` with the error dropped at init leaves a `nil` regexp that
panics on first use, and compiling per request in a hot path is a latency and
allocation regression. `MustCompile` at package scope is the intended shape.

The subtler lesson is initialization *order*. Go's spec is explicit: package-level
variables are initialized in *dependency order*, not the order they appear in the
source. If `var allowedTLDs = deriveTLDs()` (in `rules.go`) reads
`var knownDomains` (in `patterns.go`), Go guarantees `knownDomains` is fully
initialized before `deriveTLDs` runs — even if the filenames or line numbers would
suggest otherwise. Files are presented to the compiler in filename order, but that
governs only the relative order of *independent* initializers and of `init()`
functions; a genuine data dependency always wins. This exercise leans on that: a
derived var in `rules.go` depends on a base var in `patterns.go`, and it is
correct because Go resolves the dependency, not because of where the lines sit.

What you must NOT do is assume source position. If you wrote code that read another
package var inside a function-valued initializer and *assumed* it ran first because
it was "higher in the file", you would be relying on an accident. When two vars
have no dependency between them, only then does declaration order decide, and
building logic on that is how a var gets read as its zero value (a `nil` map, a
`nil` regexp, an empty slice) at load time.

Create `patterns.go` — the precompiled base patterns and a base data var:

```go
// patterns.go
package validate

import "regexp"

// Precompiled once at package initialization. A malformed pattern would panic
// here, at load, which is the intended fail-fast for a static regexp.
var (
	emailRE  = regexp.MustCompile(`^[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}$`)
	slugRE   = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	semverRE = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)
)

// knownDomains is a base data var. allowedTLDs in rules.go depends on it; Go
// initializes this first because of that dependency, regardless of file order.
var knownDomains = []string{"example.com", "corp.example.org", "mail.test.io"}
```

Create `rules.go` — a derived var that depends on `patterns.go`:

```go
// rules.go
package validate

import "strings"

// allowedTLDs is DERIVED from knownDomains (declared in patterns.go). Even
// though this file sorts after patterns.go only by filename, correctness does
// not rely on that: Go sees allowedTLDs depends on knownDomains and initializes
// knownDomains first. Change the filenames and this still holds.
var allowedTLDs = deriveTLDs(knownDomains)

func deriveTLDs(domains []string) []string {
	seen := make(map[string]struct{})
	var tlds []string
	for _, d := range domains {
		i := strings.LastIndex(d, ".")
		if i < 0 || i == len(d)-1 {
			continue
		}
		tld := d[i+1:]
		if _, ok := seen[tld]; ok {
			continue
		}
		seen[tld] = struct{}{}
		tlds = append(tlds, tld)
	}
	return tlds
}
```

Create `validate.go` — the validator that consumes the precompiled patterns:

```go
// validate.go
package validate

import (
	"fmt"
	"slices"
	"strings"
)

// User is a record to validate.
type User struct {
	Email   string
	Slug    string
	Version string
}

// ValidateUser checks each field against a precompiled pattern and confirms the
// email's TLD is in the derived allow-list. It returns a joined error naming
// every failure, or nil when the record is valid.
func ValidateUser(u User) error {
	var errs []error
	if !emailRE.MatchString(u.Email) {
		errs = append(errs, fmt.Errorf("email %q is malformed", u.Email))
	} else if !tldAllowed(u.Email) {
		errs = append(errs, fmt.Errorf("email %q has a disallowed TLD", u.Email))
	}
	if !slugRE.MatchString(u.Slug) {
		errs = append(errs, fmt.Errorf("slug %q is malformed", u.Slug))
	}
	if !semverRE.MatchString(u.Version) {
		errs = append(errs, fmt.Errorf("version %q is not semver", u.Version))
	}
	if len(errs) == 0 {
		return nil
	}
	// Join keeps every failure visible instead of only the first.
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.Error()
	}
	return fmt.Errorf("invalid user: %s", strings.Join(msgs, "; "))
}

func tldAllowed(email string) bool {
	i := strings.LastIndex(email, ".")
	if i < 0 {
		return false
	}
	return slices.Contains(allowedTLDs, email[i+1:])
}

// AllowedTLDs exposes the derived allow-list for the demo and tests.
func AllowedTLDs() []string { return slices.Clone(allowedTLDs) }
```

Note the deliberate negative example, kept in a comment so it is never compiled:

```text
// WRONG (illustrative only): assuming source order instead of dependency order.
//
//   var allowedTLDs = deriveTLDs(knownDomains) // in a file that sorts FIRST
//   var knownDomains = []string{...}           // in a file that sorts LATER
//
// A reader might fear allowedTLDs runs against a nil knownDomains. It does not:
// Go initializes knownDomains first BECAUSE allowedTLDs depends on it. The real
// bug is the opposite mistake -- writing an initializer that reads a var it does
// NOT reference through a dependency (e.g. via a closure over a pointer set in an
// init() that has not run yet), which genuinely reads a zero value.
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/validate"
)

func main() {
	fmt.Println("allowed TLDs:", validate.AllowedTLDs())

	records := []validate.User{
		{Email: "ada@example.com", Slug: "ada-lovelace", Version: "v1.2.3"},
		{Email: "bad-email", Slug: "Not A Slug", Version: "1.x"},
	}
	for _, u := range records {
		if err := validate.ValidateUser(u); err != nil {
			fmt.Println("reject:", err)
		} else {
			fmt.Printf("accept: %s\n", u.Email)
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
allowed TLDs: [com org io]
accept: ada@example.com
reject: invalid user: email "bad-email" is malformed; slug "Not A Slug" is malformed; version "1.x" is not semver
```

### Tests

Create `validate_test.go`:

```go
// validate_test.go
package validate

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestPatternsAreCompiled(t *testing.T) {
	t.Parallel()

	for name, re := range map[string]*regexp.Regexp{
		"email":  emailRE,
		"slug":   slugRE,
		"semver": semverRE,
	} {
		if re == nil {
			t.Fatalf("%s pattern is nil; MustCompile did not run", name)
		}
	}
}

func TestPatternMatching(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		re    *regexp.Regexp
		input string
		want  bool
	}{
		{"email ok", emailRE, "ada@example.com", true},
		{"email no domain", emailRE, "ada@", false},
		{"email spaces", emailRE, "ada @example.com", false},
		{"slug ok", slugRE, "ada-lovelace", true},
		{"slug caps", slugRE, "Ada", false},
		{"slug leading dash", slugRE, "-ada", false},
		{"semver ok", semverRE, "v1.2.3", true},
		{"semver bare", semverRE, "1.2.3", true},
		{"semver partial", semverRE, "1.2", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.re.MatchString(tc.input); got != tc.want {
				t.Fatalf("%s.MatchString(%q) = %v, want %v", tc.name, tc.input, got, tc.want)
			}
		})
	}
}

// TestDerivedVarSeesDependency proves allowedTLDs (rules.go) was initialized
// from knownDomains (patterns.go) in dependency order, not source order.
func TestDerivedVarSeesDependency(t *testing.T) {
	t.Parallel()

	got := AllowedTLDs()
	want := []string{"com", "org", "io"}
	if !slices.Equal(got, want) {
		t.Fatalf("AllowedTLDs() = %v, want %v (derived var did not see its dependency)", got, want)
	}
}

func TestValidateUser(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		user    User
		wantErr bool
		frag    string
	}{
		{"valid", User{Email: "ada@example.com", Slug: "ada", Version: "v1.0.0"}, false, ""},
		{"bad email", User{Email: "nope", Slug: "ada", Version: "v1.0.0"}, true, "malformed"},
		{"disallowed tld", User{Email: "x@example.net", Slug: "ada", Version: "v1.0.0"}, true, "disallowed TLD"},
		{"bad slug", User{Email: "ada@example.com", Slug: "Bad Slug", Version: "v1.0.0"}, true, "slug"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateUser(tc.user)
			if tc.wantErr != (err != nil) {
				t.Fatalf("ValidateUser err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.frag != "" && (err == nil || !strings.Contains(err.Error(), tc.frag)) {
				t.Fatalf("error %v does not contain %q", err, tc.frag)
			}
		})
	}
}
```

## Review

The package is correct when every precompiled pattern is non-nil (proof that
`MustCompile` ran at load) and matches the documented inputs, and when the derived
`allowedTLDs` equals `[com org io]` — which it does because Go initialized
`knownDomains` before `allowedTLDs` on the strength of the data dependency, not
because of filenames. `TestDerivedVarSeesDependency` is the order proof: if Go used
source order and `rules.go` sorted first, the derived var would have read a `nil`
slice and the test would catch it.

The mistakes this guards against are the two regexp traps and the ordering
assumption. Never `regexp.Compile` at init and drop the error (a `nil` regexp
panics on first `MatchString`); never compile per request in a hot path; use
`MustCompile` at package scope. And never build logic on the textual position of a
`var` declaration — express the dependency explicitly (one initializer references
the other) and let Go order them.

## Resources

- [Go spec: Package initialization](https://go.dev/ref/spec#Package_initialization) — the dependency-order rule for package-level variables.
- [regexp.MustCompile](https://pkg.go.dev/regexp#MustCompile) — compile-once, panic-on-bad-pattern at package scope.
- [regexp.Regexp.MatchString](https://pkg.go.dev/regexp#Regexp.MatchString) — the match method the validators call.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-sync-once-lazy-init-vs-init.md](06-sync-once-lazy-init-vs-init.md)

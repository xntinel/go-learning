# Exercise 2: Config-driven signer-identity policy

Keyless verification is only as safe as the identity policy behind it, because
anyone with an accepted OIDC identity can obtain a valid Fulcio certificate. This
exercise builds the policy layer: a declarative allowlist of signer rules compiled
into `verify.CertificateIdentities`, with a builder that refuses the two ways a
policy silently becomes worthless — an empty identity set and an unanchored SAN
regex.

This module is self-contained and fully offline: it imports only the `sigstore-go`
verify package and the standard library, and its tests are pure.

## What you'll build

```text
idpolicy/                    independent module: example.com/idpolicy
  go.mod                     requires sigstore/sigstore-go
  idpolicy.go                SignerRule, Allowlist, sentinels; Build, requireAnchored
  cmd/
    demo/
      main.go                accepted vs rejected candidate identity
  idpolicy_test.go           anchored-vs-unanchored matching, builder rejections, Example
```

- Files: `idpolicy.go`, `cmd/demo/main.go`, `idpolicy_test.go`.
- Implement: `Build(Allowlist) (verify.CertificateIdentities, error)` turning declarative rules into matchers, rejecting an empty allowlist, a rule with no SAN, and an unanchored SAN regex, each with a wrapped sentinel.
- Test: prove an unanchored SAN regex wrongly matches `repo-evil` and a substring host while the anchored form does not; issuer exact-value vs regex; the builder refuses empty and unanchored policies via `errors.Is`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/49-application-security-crypto-supplychain/11-sigstore-cosign-signing/02-identity-policy-as-code/cmd/demo
cd go-solutions/49-application-security-crypto-supplychain/11-sigstore-cosign-signing/02-identity-policy-as-code
go mod edit -go=1.25
go get github.com/sigstore/sigstore-go@v1.2.1
```

### The core invariant: a valid signature is worthless without a pinned signer

`verify.NewShortCertificateIdentity(issuer, issuerRegex, sanValue, sanRegex)` takes
four positional strings and returns a `verify.CertificateIdentity`. Its underlying
matchers, `verify.NewSANMatcher` and `verify.NewIssuerMatcher`, each accept an
exact value *and* a regex string; the value is used when non-empty, otherwise the
regex. The library itself has an escape hatch, `verify.WithoutIdentitiesUnsafe()`,
that lets a policy verify with no identity at all — its name is the warning. This
policy layer forbids that by construction: `Build` returns an error rather than
ever producing an identity-less policy, so "verify without a signer" is a
compile-time-visible mistake in *your* code path, not a runtime option.

### Anchoring is a policy-layer responsibility

The highest-value check in the whole lesson is anchoring. A SAN regex is compiled
into `regexp.Regexp`, and `regexp.MatchString` finds a match *anywhere* in the
string unless the pattern is anchored. So `github.com/org/repo` matches
`github.com/org/repo-evil` (a repo an attacker can register) and
`evil.com/x/github.com/org/repo` (the substring appears mid-string). Only
`^https://github.com/org/repo/\.github/workflows/release\.yml@refs/heads/main$`
pins exactly one identity. Rather than hope every rule author remembers this,
`Build` rejects any SAN regex that does not start with `^` and end with `$`. That
is a coarse guard — it does not prove the regex is *tight* — but it eliminates the
single most common and most dangerous mistake mechanically.

Create `idpolicy.go`:

```go
package idpolicy

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/verify"
)

// Sentinel errors for classifying policy build failures with errors.Is.
var (
	ErrEmptyAllowlist = errors.New("idpolicy: allowlist has no signer rules")
	ErrNoSAN          = errors.New("idpolicy: signer rule has neither SAN value nor SAN regex")
	ErrNoIssuer       = errors.New("idpolicy: signer rule has neither issuer value nor issuer regex")
	ErrUnanchoredSAN  = errors.New("idpolicy: SAN regex must be anchored with ^ and $")
	ErrBuildIdentity  = errors.New("idpolicy: cannot build certificate identity")
)

// SignerRule is one declarative allowlist entry. Set exactly one of SANValue or
// SANRegex, and one of Issuer or IssuerRegex. A regex SAN must be anchored.
type SignerRule struct {
	Issuer      string // exact OIDC issuer, e.g. https://token.actions.githubusercontent.com
	IssuerRegex string // regex alternative to Issuer
	SANValue    string // exact Subject Alternative Name
	SANRegex    string // anchored regex alternative to SANValue
}

// Allowlist is the whole signer policy: any rule that matches authorizes the
// artifact.
type Allowlist struct {
	Signers []SignerRule
}

// Build compiles the allowlist into verify.CertificateIdentities, refusing the
// policies that silently trust everything or nothing.
func Build(a Allowlist) (verify.CertificateIdentities, error) {
	if len(a.Signers) == 0 {
		return nil, ErrEmptyAllowlist
	}
	ids := make(verify.CertificateIdentities, 0, len(a.Signers))
	for i, s := range a.Signers {
		if s.SANValue == "" && s.SANRegex == "" {
			return nil, fmt.Errorf("rule %d: %w", i, ErrNoSAN)
		}
		if s.Issuer == "" && s.IssuerRegex == "" {
			return nil, fmt.Errorf("rule %d: %w", i, ErrNoIssuer)
		}
		if s.SANRegex != "" {
			if err := requireAnchored(s.SANRegex); err != nil {
				return nil, fmt.Errorf("rule %d: %w", i, err)
			}
		}
		id, err := verify.NewShortCertificateIdentity(s.Issuer, s.IssuerRegex, s.SANValue, s.SANRegex)
		if err != nil {
			return nil, fmt.Errorf("rule %d: %w: %v", i, ErrBuildIdentity, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// requireAnchored rejects a SAN regex that is not bounded at both ends, the
// commonest keyless supply-chain footgun.
func requireAnchored(re string) error {
	if !strings.HasPrefix(re, "^") || !strings.HasSuffix(re, "$") {
		return ErrUnanchoredSAN
	}
	return nil
}
```

### The runnable demo

The demo builds a one-rule allowlist for a GitHub Actions release workflow, then
shows the compiled SAN regex accepting the real workflow SAN and rejecting a
look-alike. It also shows the builder rejecting an empty allowlist — the safe
answer to "verify without an identity".

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"log"

	"example.com/idpolicy"
)

func main() {
	ids, err := idpolicy.Build(idpolicy.Allowlist{
		Signers: []idpolicy.SignerRule{{
			Issuer:   "https://token.actions.githubusercontent.com",
			SANRegex: `^https://github.com/acme/service/\.github/workflows/release\.yml@refs/heads/main$`,
		}},
	})
	if err != nil {
		log.Fatal(err)
	}

	rule := ids[0]
	good := "https://github.com/acme/service/.github/workflows/release.yml@refs/heads/main"
	evil := "https://github.com/acme/service-evil/.github/workflows/release.yml@refs/heads/main"
	fmt.Printf("accepts release workflow: %v\n", rule.SubjectAlternativeName.Regexp.MatchString(good))
	fmt.Printf("accepts look-alike repo: %v\n", rule.SubjectAlternativeName.Regexp.MatchString(evil))

	_, err = idpolicy.Build(idpolicy.Allowlist{})
	fmt.Printf("empty allowlist rejected: %v\n", errors.Is(err, idpolicy.ErrEmptyAllowlist))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
accepts release workflow: true
accepts look-alike repo: false
empty allowlist rejected: true
```

### Tests

The centerpiece test constructs SAN matchers directly with `verify.NewSANMatcher`
— bypassing the builder's guard on purpose — to demonstrate *why* the guard
exists: the unanchored pattern matches both a look-alike repository and a
substring host, while the anchored pattern matches only the intended SAN. The
issuer test shows exact-value matching versus regex matching. The builder tests
assert the wrapped sentinels for an empty allowlist, a rule with no SAN, and an
unanchored SAN regex. The "your turn" case, `TestBuilderRejectsMissingAnchor`, is
included as a real test rejecting a SAN regex missing its trailing `$`.

Create `idpolicy_test.go`:

```go
package idpolicy

import (
	"errors"
	"fmt"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/verify"
)

func TestSANAnchoring(t *testing.T) {
	t.Parallel()

	const (
		unanchored = "github.com/org/repo"
		anchored   = "^https://github.com/org/repo/.github/workflows/release.yml@refs/heads/main$"
		realSAN    = "https://github.com/org/repo/.github/workflows/release.yml@refs/heads/main"
		lookAlike  = "https://github.com/org/repo-evil/.github/workflows/release.yml@refs/heads/main"
		substring  = "https://evil.com/x/github.com/org/repo"
	)

	un, err := verify.NewSANMatcher("", unanchored)
	if err != nil {
		t.Fatalf("NewSANMatcher(unanchored): %v", err)
	}
	an, err := verify.NewSANMatcher("", anchored)
	if err != nil {
		t.Fatalf("NewSANMatcher(anchored): %v", err)
	}

	tests := []struct {
		name       string
		candidate  string
		wantUnanch bool
		wantAnch   bool
	}{
		{"real SAN", realSAN, true, true},
		{"look-alike repo", lookAlike, true, false},
		{"substring host", substring, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := un.Regexp.MatchString(tc.candidate); got != tc.wantUnanch {
				t.Fatalf("unanchored match(%q) = %v, want %v", tc.candidate, got, tc.wantUnanch)
			}
			if got := an.Regexp.MatchString(tc.candidate); got != tc.wantAnch {
				t.Fatalf("anchored match(%q) = %v, want %v", tc.candidate, got, tc.wantAnch)
			}
		})
	}
}

func TestIssuerMatcher(t *testing.T) {
	t.Parallel()

	exact, err := verify.NewIssuerMatcher("https://token.actions.githubusercontent.com", "")
	if err != nil {
		t.Fatalf("NewIssuerMatcher(exact): %v", err)
	}
	if exact.Issuer != "https://token.actions.githubusercontent.com" {
		t.Fatalf("exact issuer = %q", exact.Issuer)
	}

	re, err := verify.NewIssuerMatcher("", "^https://token\\.actions\\.githubusercontent\\.com$")
	if err != nil {
		t.Fatalf("NewIssuerMatcher(regex): %v", err)
	}
	if !re.Regexp.MatchString("https://token.actions.githubusercontent.com") {
		t.Fatal("regex issuer should match the GitHub OIDC issuer")
	}
	if re.Regexp.MatchString("https://accounts.google.com") {
		t.Fatal("regex issuer should not match Google")
	}
}

func TestBuildRejections(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		list    Allowlist
		wantErr error
	}{
		{
			name:    "empty allowlist",
			list:    Allowlist{},
			wantErr: ErrEmptyAllowlist,
		},
		{
			name: "rule missing SAN",
			list: Allowlist{Signers: []SignerRule{{
				Issuer: "https://token.actions.githubusercontent.com",
			}}},
			wantErr: ErrNoSAN,
		},
		{
			name: "unanchored SAN regex",
			list: Allowlist{Signers: []SignerRule{{
				Issuer:   "https://token.actions.githubusercontent.com",
				SANRegex: "github.com/org/repo",
			}}},
			wantErr: ErrUnanchoredSAN,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Build(tc.list)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Build err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestBuilderRejectsMissingAnchor is the "your turn" case: a SAN regex missing
// its trailing $ must be rejected.
func TestBuilderRejectsMissingAnchor(t *testing.T) {
	t.Parallel()
	_, err := Build(Allowlist{Signers: []SignerRule{{
		Issuer:   "https://token.actions.githubusercontent.com",
		SANRegex: "^https://github.com/org/repo/",
	}}})
	if !errors.Is(err, ErrUnanchoredSAN) {
		t.Fatalf("Build err = %v, want ErrUnanchoredSAN", err)
	}
}

func Example() {
	ids, _ := Build(Allowlist{Signers: []SignerRule{{
		Issuer:   "https://token.actions.githubusercontent.com",
		SANRegex: `^https://github.com/acme/service/\.github/workflows/release\.yml@refs/heads/main$`,
	}}})
	san := ids[0].SubjectAlternativeName.Regexp
	fmt.Println(san.MatchString("https://github.com/acme/service/.github/workflows/release.yml@refs/heads/main"))
	fmt.Println(san.MatchString("https://github.com/acme/service-evil/.github/workflows/release.yml@refs/heads/main"))
	// Output:
	// true
	// false
}
```

## Review

The policy layer is correct when it makes the two dangerous policies
unrepresentable. An empty allowlist yields `ErrEmptyAllowlist`, so a gate can
never accidentally verify with no signer pinned — the safe replacement for
`verify.WithoutIdentitiesUnsafe`. An unanchored SAN regex yields
`ErrUnanchoredSAN`, so the `repo-evil` and substring-host matches demonstrated in
`TestSANAnchoring` can never reach production. Both are asserted with `errors.Is`
against wrapped sentinels.

The mistakes to avoid mirror the guards. Do not transpose the four positional
arguments of `NewShortCertificateIdentity`; the table in `TestSANAnchoring` and
the explicit `NewSANMatcher`/`NewIssuerMatcher` calls keep each role named. Do not
treat anchoring as optional — the whole point of the exercise is that an
unanchored SAN regex silently trusts an attacker while every test that only checks
"the real SAN matches" still passes; you must also assert that the look-alike does
*not* match. Offline, the module needs the sigstore-go dependency in the module
cache; with it, `go test -count=1 ./...` runs with no network.

## Resources

- [`sigstore-go/pkg/verify` identity matchers](https://pkg.go.dev/github.com/sigstore/sigstore-go/pkg/verify#NewShortCertificateIdentity) — `NewShortCertificateIdentity`, `NewSANMatcher`, `NewIssuerMatcher`, `CertificateIdentities`, and `WithoutIdentitiesUnsafe`.
- [`regexp`](https://pkg.go.dev/regexp#Regexp.MatchString) — why `MatchString` matches anywhere unless anchored with `^` and `$`.
- [Sigstore certificate identity in cosign](https://docs.sigstore.dev/cosign/verifying/verify/) — `--certificate-identity` and `--certificate-oidc-issuer`, the CLI form of the same policy.

---

Back to [01-verify-bundle-gate.md](01-verify-bundle-gate.md) | Next: [03-keyless-sign-release.md](03-keyless-sign-release.md)

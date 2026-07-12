# Exercise 10: Decide proxy source, sumdb, TLS, and GOAUTH per module (Go 1.24)

Before rolling a private-registry configuration to the fleet, you want to document
and test exactly what the `go` command will do for each module: which proxy chain
applies, whether it consults the checksum database, whether it requires HTTPS, and
which Go 1.24 GOAUTH method authenticates the request. This exercise builds that
policy engine.

## What you'll build

```text
fetchpolicy/               independent module: example.com/fetchpolicy
  go.mod                   go 1.26
  policy.go                Env, Decision, AuthMethod, Decide(module, env)
  cmd/
    demo/
      main.go              resolves the policy for three modules under one env
  policy_test.go           table asserting every field of the Decision
  example_test.go          ExampleDecide with // Output
```

- Files: `policy.go`, `cmd/demo/main.go`, `policy_test.go`, `example_test.go`.
- Implement: `Decide(modulePath string, env Env) Decision` returning the proxy chain, whether the checksum database is consulted, whether HTTPS is required (or `GOINSECURE` permits http), and the selected `GOAUTH` method.
- Test: a `GOPRIVATE`-matched module bypasses proxy and sumdb and selects netrc; a public module consults `sum.golang.org` over HTTPS with no auth; a `GOINSECURE`-matched host permits http but still consults the sumdb; `GOSUMDB=off` disables the database globally.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/07-module-proxies-and-goproxy/10-fetch-policy-engine/cmd/demo
cd go-solutions/11-packages-and-modules/07-module-proxies-and-goproxy/10-fetch-policy-engine
go mod edit -go=1.26
```

### The policy is four independent decisions from one environment

The value of writing this as one function is that it forces the four knobs to be
disentangled, which is exactly where misconfigurations hide. Proxy routing:
`GONOPROXY` (seeded by `GOPRIVATE`) decides whether the module bypasses the proxy
and goes `direct`; otherwise the effective `GOPROXY` chain applies. Checksum
database: it is consulted unless disabled globally by `GOSUMDB=off` or excluded for
this module by `GONOSUMDB`/`GOPRIVATE`. Transport: `GOINSECURE` relaxes it —
matching modules may use plain http and skip TLS verification — but, and this is
the trap the engine encodes, `GOINSECURE` does NOT touch the checksum decision, so
a `GOINSECURE`-matched public module still gets its hash sent to `sum.golang.org`.
Authentication: Go 1.24 `GOAUTH` selects the credential mechanism (`off`, `netrc`,
`git`, or a `command`); credentials are attached when fetching a private module
direct from its source, and a public module fetched through the public proxy
carries none. `GOAUTH` unset defaults to `netrc` in Go 1.24.

The matching is the same prefix-glob (`MatchPrefixPatterns`) the `go` command uses
for all of `GOPRIVATE`/`GONOPROXY`/`GONOSUMDB`/`GOINSECURE`. `cmp.Or` expresses the
"first non-empty" defaulting for the effective sumdb, proxy, and the `GOPRIVATE`
shorthand cleanly. As a bonus policy check the engine parses the first proxy entry
with `net/url.Parse` and flags a plain-`http` proxy, since pointing `GOPROXY` at an
untrusted http server is a real risk the concepts warn about.

Create `policy.go`:

```go
// Package fetchpolicy decides, per module path, how the go command will fetch it:
// which proxy chain applies, whether the checksum database is consulted, whether
// HTTPS is required, and which Go 1.24 GOAUTH method authenticates the request.
package fetchpolicy

import (
	"cmp"
	"net/url"
	"path"
	"strings"
)

// AuthMethod is the GOAUTH mechanism selected for a fetch.
type AuthMethod string

const (
	AuthOff     AuthMethod = "off"
	AuthNetrc   AuthMethod = "netrc"
	AuthGit     AuthMethod = "git"
	AuthCommand AuthMethod = "command"
)

// Env is the subset of the module environment the policy reads.
type Env struct {
	GOPROXY    string
	GOPRIVATE  string
	GONOPROXY  string
	GONOSUMDB  string
	GOSUMDB    string
	GOINSECURE string
	GOAUTH     string
}

// Decision is the resolved fetch policy for one module path.
type Decision struct {
	Proxy         string // effective GOPROXY chain, or "direct" when bypassed
	BypassProxy   bool   // GONOPROXY (or GOPRIVATE) matched
	ProxyInsecure bool   // the first proxy entry uses http://
	ConsultSumDB  bool   // the checksum database is consulted for this module
	SumDB         string // effective GOSUMDB value
	RequireHTTPS  bool   // false when GOINSECURE permits http for this module
	Auth          AuthMethod
}

// matchPrefix reports whether target matches any comma-separated path.Match glob
// in globs on a leading path-element prefix. Empty/malformed patterns are skipped.
func matchPrefix(globs, target string) bool {
	for globs != "" {
		var glob string
		if i := strings.Index(globs, ","); i >= 0 {
			glob, globs = globs[:i], globs[i+1:]
		} else {
			glob, globs = globs, ""
		}
		glob = strings.TrimSuffix(glob, "/")
		if glob == "" {
			continue
		}
		n := strings.Count(glob, "/")
		prefix := target
		for i := 0; i < len(target); i++ {
			if target[i] == '/' {
				if n == 0 {
					prefix = target[:i]
					break
				}
				n--
			}
		}
		if n > 0 {
			continue
		}
		if ok, _ := path.Match(glob, prefix); ok {
			return true
		}
	}
	return false
}

// authMethod parses the GOAUTH configuration to its method keyword. The value
// begins with off, netrc, or git; anything else is a command form.
func authMethod(goauth string) AuthMethod {
	fields := strings.Fields(goauth)
	if len(fields) == 0 {
		return AuthNetrc // Go 1.24 default
	}
	switch fields[0] {
	case "off":
		return AuthOff
	case "netrc":
		return AuthNetrc
	case "git":
		return AuthGit
	default:
		return AuthCommand
	}
}

// firstProxyInsecure reports whether the first non-keyword proxy entry uses http.
func firstProxyInsecure(goproxy string) bool {
	for _, entry := range strings.FieldsFunc(goproxy, func(r rune) bool {
		return r == ',' || r == '|'
	}) {
		if entry == "direct" || entry == "off" {
			return false
		}
		u, err := url.Parse(entry)
		if err != nil {
			return false
		}
		return u.Scheme == "http"
	}
	return false
}

// Decide resolves the fetch policy for modulePath under env.
func Decide(modulePath string, env Env) Decision {
	noProxy := cmp.Or(env.GONOPROXY, env.GOPRIVATE)
	noSumDB := cmp.Or(env.GONOSUMDB, env.GOPRIVATE)
	sumdb := cmp.Or(env.GOSUMDB, "sum.golang.org")
	goproxy := cmp.Or(env.GOPROXY, "https://proxy.golang.org,direct")

	bypass := matchPrefix(noProxy, modulePath)
	proxy := goproxy
	if bypass {
		proxy = "direct"
	}

	// The checksum database is skipped when disabled globally (GOSUMDB=off) or
	// when the module matches GONOSUMDB/GOPRIVATE. GOINSECURE does NOT skip it.
	consult := sumdb != "off" && !matchPrefix(noSumDB, modulePath)

	// GOINSECURE relaxes transport (http, skip TLS verify) for matching modules.
	requireHTTPS := !matchPrefix(env.GOINSECURE, modulePath)

	// Credentials are attached when fetching a private module direct from its
	// source; a public module fetched via the public proxy carries no auth.
	auth := AuthOff
	if bypass {
		auth = authMethod(env.GOAUTH)
	}

	return Decision{
		Proxy:         proxy,
		BypassProxy:   bypass,
		ProxyInsecure: firstProxyInsecure(proxy),
		ConsultSumDB:  consult,
		SumDB:         sumdb,
		RequireHTTPS:  requireHTTPS,
		Auth:          auth,
	}
}
```

### The runnable demo

The demo documents one onboarding environment — a `GOPRIVATE` for the corp domain
plus a `GOINSECURE` legacy host — and resolves the policy for a private module, a
public module, and a private-and-insecure legacy module.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/fetchpolicy"
)

func main() {
	// Onboarding a private registry to the fleet.
	env := fetchpolicy.Env{
		GOPROXY:    "https://proxy.golang.org,direct",
		GOPRIVATE:  "*.corp.example.com",
		GOINSECURE: "legacy.corp.example.com",
		// GOAUTH unset: Go 1.24 defaults to netrc.
	}

	modules := []string{
		"git.corp.example.com/team/lib",
		"github.com/spf13/cobra",
		"legacy.corp.example.com/old/svc",
	}
	for _, m := range modules {
		d := fetchpolicy.Decide(m, env)
		fmt.Printf("%s\n", m)
		fmt.Printf("  proxy=%s bypass=%v\n", d.Proxy, d.BypassProxy)
		fmt.Printf("  sumdb=%s consult=%v\n", d.SumDB, d.ConsultSumDB)
		fmt.Printf("  https=%v auth=%s\n", d.RequireHTTPS, d.Auth)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
git.corp.example.com/team/lib
  proxy=direct bypass=true
  sumdb=sum.golang.org consult=false
  https=true auth=netrc
github.com/spf13/cobra
  proxy=https://proxy.golang.org,direct bypass=false
  sumdb=sum.golang.org consult=true
  https=true auth=off
legacy.corp.example.com/old/svc
  proxy=direct bypass=true
  sumdb=sum.golang.org consult=false
  https=false auth=netrc
```

### Tests

The table asserts every field of the `Decision` for each policy case: the private
module (bypass, no sumdb, netrc), the public module (proxy, sumdb, no auth), the
`GOINSECURE` host that permits http yet still consults the sumdb, `GOSUMDB=off`,
the `GOAUTH` command form, `GOAUTH=off` overriding the default, and a plain-http
proxy flagged insecure. Comparing the whole struct with `==` means no field can
silently regress.

Create `policy_test.go`:

```go
package fetchpolicy

import "testing"

func TestDecide(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		modPath string
		env     Env
		want    Decision
	}{
		{
			name:    "private module bypasses proxy and sumdb, uses netrc",
			modPath: "git.corp.example.com/team/lib",
			env:     Env{GOPRIVATE: "*.corp.example.com"},
			want: Decision{
				Proxy:        "direct",
				BypassProxy:  true,
				ConsultSumDB: false,
				SumDB:        "sum.golang.org",
				RequireHTTPS: true,
				Auth:         AuthNetrc,
			},
		},
		{
			name:    "public module consults sumdb over https with no auth",
			modPath: "github.com/spf13/cobra",
			env:     Env{},
			want: Decision{
				Proxy:        "https://proxy.golang.org,direct",
				BypassProxy:  false,
				ConsultSumDB: true,
				SumDB:        "sum.golang.org",
				RequireHTTPS: true,
				Auth:         AuthOff,
			},
		},
		{
			name:    "GOINSECURE permits http but still consults sumdb",
			modPath: "insecure.example.com/x",
			env:     Env{GOINSECURE: "insecure.example.com"},
			want: Decision{
				Proxy:        "https://proxy.golang.org,direct",
				BypassProxy:  false,
				ConsultSumDB: true,
				SumDB:        "sum.golang.org",
				RequireHTTPS: false,
				Auth:         AuthOff,
			},
		},
		{
			name:    "GOSUMDB=off disables the checksum database globally",
			modPath: "github.com/spf13/cobra",
			env:     Env{GOSUMDB: "off"},
			want: Decision{
				Proxy:        "https://proxy.golang.org,direct",
				BypassProxy:  false,
				ConsultSumDB: false,
				SumDB:        "off",
				RequireHTTPS: true,
				Auth:         AuthOff,
			},
		},
		{
			name:    "private module with GOAUTH command form",
			modPath: "git.corp.example.com/team/lib",
			env: Env{
				GOPRIVATE: "*.corp.example.com",
				GOAUTH:    "example-cred-helper --host git.corp.example.com",
			},
			want: Decision{
				Proxy:        "direct",
				BypassProxy:  true,
				ConsultSumDB: false,
				SumDB:        "sum.golang.org",
				RequireHTTPS: true,
				Auth:         AuthCommand,
			},
		},
		{
			name:    "GOAUTH=off yields no auth even for a private module",
			modPath: "git.corp.example.com/team/lib",
			env:     Env{GOPRIVATE: "*.corp.example.com", GOAUTH: "off"},
			want: Decision{
				Proxy:        "direct",
				BypassProxy:  true,
				ConsultSumDB: false,
				SumDB:        "sum.golang.org",
				RequireHTTPS: true,
				Auth:         AuthOff,
			},
		},
		{
			name:    "http proxy is flagged insecure",
			modPath: "github.com/spf13/cobra",
			env:     Env{GOPROXY: "http://scratch.example.com,direct"},
			want: Decision{
				Proxy:         "http://scratch.example.com,direct",
				BypassProxy:   false,
				ProxyInsecure: true,
				ConsultSumDB:  true,
				SumDB:         "sum.golang.org",
				RequireHTTPS:  true,
				Auth:          AuthOff,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Decide(tt.modPath, tt.env)
			if got != tt.want {
				t.Errorf("Decide(%q):\n got %+v\nwant %+v", tt.modPath, got, tt.want)
			}
		})
	}
}
```

Create `example_test.go`:

```go
package fetchpolicy

import "fmt"

func ExampleDecide() {
	d := Decide("git.corp.example.com/team/lib", Env{GOPRIVATE: "*.corp.example.com"})
	fmt.Printf("proxy=%s consult=%v auth=%s\n", d.Proxy, d.ConsultSumDB, d.Auth)
	// Output: proxy=direct consult=false auth=netrc
}
```

## Review

The engine is correct when the four decisions are genuinely independent and every
field of the `Decision` holds for each case. The single most important row is the
`GOINSECURE` one: it must show `RequireHTTPS` false AND `ConsultSumDB` true,
because conflating "insecure transport" with "no integrity checking" is the real
misconfiguration that leaks private module paths to the public sumdb. The `GOAUTH`
cases pin the Go 1.24 behavior — netrc by default, `off` suppressing auth even for
a private module, and any non-keyword value parsing as a command so credentials can
be minted dynamically instead of embedded in a URL. Compare the whole struct with
`==` so a regression in any one field fails the test, and run `go test -race`.

## Resources

- [Go 1.24 release notes: GOAUTH](https://go.dev/doc/go1.24) — the new authentication mechanism and its off/netrc/git/command forms.
- [Go Modules Reference: Environment variables](https://go.dev/ref/mod#environment-variables) — `GOSUMDB`, `GOINSECURE`, `GONOPROXY`, `GONOSUMDB`, `GOPRIVATE` and their interactions.
- [`golang.org/x/mod/module.MatchPrefixPatterns`](https://pkg.go.dev/golang.org/x/mod/module#MatchPrefixPatterns) — the prefix-glob matching the routing decisions use.
- [`net/url.Parse`](https://pkg.go.dev/net/url#Parse) — extracting a proxy entry's scheme to flag a plain-http proxy.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-modcache-integrity-scan.md](09-modcache-integrity-scan.md) | Next: [../08-vendor-directory/00-concepts.md](../08-vendor-directory/00-concepts.md)

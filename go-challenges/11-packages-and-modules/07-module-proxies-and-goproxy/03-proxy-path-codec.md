# Exercise 3: Encode module paths and build GOPROXY protocol request URLs

A caching proxy or a mirror-sync tool builds proxy URLs by hand, and the first
thing it must get right is the protocol's case-encoding: every uppercase letter
becomes `!` plus its lowercase form. This exercise implements that codec, builds
the five request URLs, and decodes a `.info` body.

## What you'll build

```text
proxycodec/                independent module: example.com/proxycodec
  go.mod                   go 1.26
  codec.go                 EncodePath/DecodePath, RequestURLs, ParseInfo, ErrBadEncoding
  cmd/
    demo/
      main.go              encode, round-trip, build URLs, decode a .info body
  codec_test.go            golden round-trip, invalid-encoding, URL, .info tests
  example_test.go          ExampleEncodePath with // Output
```

- Files: `codec.go`, `cmd/demo/main.go`, `codec_test.go`, `example_test.go`.
- Implement: `EncodePath`/`DecodePath` (case-encoding and its inverse), `RequestURLs(base, module, version) URLs` for the five protocol endpoints, and `ParseInfo([]byte) Info` decoding `{Version, Time}`.
- Test: `Azure` round-trips through `!azure`; the URL builder emits the exact protocol paths for a known `module@v1.2.3`; `.info` decode yields a parsed `time.Time`; invalid encodings (bare uppercase, dangling `!`) are rejected.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/07-module-proxies-and-goproxy/03-proxy-path-codec/cmd/demo
cd go-solutions/11-packages-and-modules/07-module-proxies-and-goproxy/03-proxy-path-codec
go mod edit -go=1.26
```

### Why case-encoding exists and where it bites

Module paths and versions are stored on filesystems and object stores that fold
case, so `example.com/Foo` and `example.com/foo` would collide and one module
could silently shadow another. The protocol prevents that by case-encoding: on the
wire, every uppercase letter is replaced by `!` followed by its lowercase form.
`github.com/Azure/azure-sdk-for-go` is requested as
`github.com/!azure/azure-sdk-for-go`. A literal `!` is not legal in an unencoded
module path, which is exactly what makes the scheme reversible — a `!` in an
encoded path always introduces an escaped uppercase letter.

The decoder has to be strict, because a hand-rolled mirror that accepts sloppy
input will silently request the wrong path. Three inputs are rejected: a dangling
`!` at the end, a `!` not followed by a lowercase letter, and a bare uppercase
letter (which can never appear in a valid encoded path — it would have been
escaped). Returning `ErrBadEncoding` for those, wrapped with `%w`, lets a caller
distinguish "malformed input" from a network error.

`RequestURLs` then encodes both the module path and the version and assembles the
five endpoints with `net/url.JoinPath`, which cleans the path without escaping the
protocol's `@` and `!` characters. The five URLs are `@v/list`,
`@v/$version.info`, `@v/$version.mod`, `@v/$version.zip`, and `@latest`. Finally
`ParseInfo` decodes the `.info` body; because the `Time` field is an RFC 3339
string, `encoding/json` parses it straight into a `time.Time` with no custom
unmarshaler.

Create `codec.go`:

```go
// Package proxycodec implements the case-encoding of the GOPROXY protocol and
// builds the five request URLs for a module version.
package proxycodec

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// ErrBadEncoding is returned by DecodePath for a malformed encoded path.
var ErrBadEncoding = errors.New("proxycodec: bad path encoding")

// EncodePath applies the protocol's case-encoding: every uppercase letter is
// replaced by '!' followed by its lowercase form. A literal '!' is not a legal
// character in a module path or version, so its presence is an error.
func EncodePath(s string) (string, error) {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '!':
			return "", fmt.Errorf("%w: '!' is not legal in an unencoded path", ErrBadEncoding)
		case unicode.IsUpper(r):
			b.WriteByte('!')
			b.WriteRune(unicode.ToLower(r))
		default:
			b.WriteRune(r)
		}
	}
	return b.String(), nil
}

// DecodePath inverts EncodePath. It rejects a dangling '!', a '!' not followed by
// a lowercase letter, and a bare uppercase letter (which cannot appear in a valid
// encoded path).
func DecodePath(s string) (string, error) {
	var b strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '!':
			if i+1 >= len(rs) {
				return "", fmt.Errorf("%w: dangling '!' at end", ErrBadEncoding)
			}
			next := rs[i+1]
			if !unicode.IsLower(next) {
				return "", fmt.Errorf("%w: '!' must precede a lowercase letter, got %q", ErrBadEncoding, next)
			}
			b.WriteRune(unicode.ToUpper(next))
			i++
		case unicode.IsUpper(r):
			return "", fmt.Errorf("%w: unexpected uppercase %q in encoded path", ErrBadEncoding, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String(), nil
}

// URLs holds the five GOPROXY protocol request URLs for a module version.
type URLs struct {
	List   string
	Info   string
	Mod    string
	Zip    string
	Latest string
}

// RequestURLs builds the protocol URLs for modulePath@version against base. The
// module path and version are case-encoded per the protocol.
func RequestURLs(base, modulePath, version string) (URLs, error) {
	encMod, err := EncodePath(modulePath)
	if err != nil {
		return URLs{}, fmt.Errorf("encode module path: %w", err)
	}
	encVer, err := EncodePath(version)
	if err != nil {
		return URLs{}, fmt.Errorf("encode version: %w", err)
	}
	join := func(elem ...string) (string, error) {
		return url.JoinPath(base, elem...)
	}
	list, err := join(encMod, "@v", "list")
	if err != nil {
		return URLs{}, err
	}
	info, err := join(encMod, "@v", encVer+".info")
	if err != nil {
		return URLs{}, err
	}
	mod, err := join(encMod, "@v", encVer+".mod")
	if err != nil {
		return URLs{}, err
	}
	zip, err := join(encMod, "@v", encVer+".zip")
	if err != nil {
		return URLs{}, err
	}
	latest, err := join(encMod, "@latest")
	if err != nil {
		return URLs{}, err
	}
	return URLs{List: list, Info: info, Mod: mod, Zip: zip, Latest: latest}, nil
}

// Info is the decoded body of a $version.info endpoint.
type Info struct {
	Version string
	Time    time.Time
}

// ParseInfo decodes a .info JSON body. The Time field is an RFC 3339 timestamp,
// which encoding/json parses into time.Time automatically.
func ParseInfo(data []byte) (Info, error) {
	var i Info
	if err := json.Unmarshal(data, &i); err != nil {
		return Info{}, fmt.Errorf("decode .info: %w", err)
	}
	return i, nil
}
```

### The runnable demo

The demo encodes `github.com/Azure/azure-sdk-for-go`, round-trips it, prints the
five protocol URLs for `v1.2.3`, and decodes a real `.info` body.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/proxycodec"
)

func main() {
	const base = "https://proxy.golang.org"
	const mod = "github.com/Azure/azure-sdk-for-go"
	const ver = "v1.2.3"

	enc, _ := proxycodec.EncodePath(mod)
	fmt.Printf("encoded path: %s\n", enc)

	dec, _ := proxycodec.DecodePath(enc)
	fmt.Printf("round-trip:   %s\n", dec)

	urls, _ := proxycodec.RequestURLs(base, mod, ver)
	fmt.Printf("list:   %s\n", urls.List)
	fmt.Printf("info:   %s\n", urls.Info)
	fmt.Printf("mod:    %s\n", urls.Mod)
	fmt.Printf("zip:    %s\n", urls.Zip)
	fmt.Printf("latest: %s\n", urls.Latest)

	body := []byte(`{"Version":"v1.2.3","Time":"2019-11-09T21:39:31Z"}`)
	info, _ := proxycodec.ParseInfo(body)
	fmt.Printf("info.Version: %s\n", info.Version)
	fmt.Printf("info.Time:    %s\n", info.Time.UTC().Format("2006-01-02"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
encoded path: github.com/!azure/azure-sdk-for-go
round-trip:   github.com/Azure/azure-sdk-for-go
list:   https://proxy.golang.org/github.com/!azure/azure-sdk-for-go/@v/list
info:   https://proxy.golang.org/github.com/!azure/azure-sdk-for-go/@v/v1.2.3.info
mod:    https://proxy.golang.org/github.com/!azure/azure-sdk-for-go/@v/v1.2.3.mod
zip:    https://proxy.golang.org/github.com/!azure/azure-sdk-for-go/@v/v1.2.3.zip
latest: https://proxy.golang.org/github.com/!azure/azure-sdk-for-go/@latest
info.Version: v1.2.3
info.Time:    2019-11-09
```

### Tests

The golden round-trip covers the canonical `Azure` case plus a double-uppercase
path (`FooBar` becomes `!foo!bar`), and the invalid-encoding table pins each of
the three rejection rules. The URL test asserts the exact protocol paths, and the
`.info` test proves the RFC 3339 timestamp parses to the right instant.

Create `codec_test.go`:

```go
package proxycodec

import (
	"errors"
	"testing"
	"time"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		enc  string
	}{
		{"azure", "github.com/Azure/azure-sdk-for-go", "github.com/!azure/azure-sdk-for-go"},
		{"masterminds", "github.com/Masterminds/semver", "github.com/!masterminds/semver"},
		{"all lower unchanged", "golang.org/x/mod", "golang.org/x/mod"},
		{"mixed caps", "example.com/FooBar", "example.com/!foo!bar"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := EncodePath(tt.raw)
			if err != nil {
				t.Fatalf("EncodePath(%q) error: %v", tt.raw, err)
			}
			if got != tt.enc {
				t.Errorf("EncodePath(%q) = %q; want %q", tt.raw, got, tt.enc)
			}
			back, err := DecodePath(got)
			if err != nil {
				t.Fatalf("DecodePath(%q) error: %v", got, err)
			}
			if back != tt.raw {
				t.Errorf("round-trip = %q; want %q", back, tt.raw)
			}
		})
	}
}

func TestDecodeRejectsInvalid(t *testing.T) {
	t.Parallel()
	bad := []string{
		"example.com/Foo",  // bare uppercase
		"example.com/!",    // dangling !
		"example.com/!Bar", // ! before uppercase
		"example.com/!/x",  // ! before slash
	}
	for _, s := range bad {
		if _, err := DecodePath(s); !errors.Is(err, ErrBadEncoding) {
			t.Errorf("DecodePath(%q) err = %v; want ErrBadEncoding", s, err)
		}
	}
}

func TestEncodeRejectsBang(t *testing.T) {
	t.Parallel()
	if _, err := EncodePath("example.com/a!b"); !errors.Is(err, ErrBadEncoding) {
		t.Errorf("EncodePath with '!' err = %v; want ErrBadEncoding", err)
	}
}

func TestRequestURLs(t *testing.T) {
	t.Parallel()
	urls, err := RequestURLs("https://proxy.golang.org", "github.com/Azure/azure-sdk-for-go", "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	want := URLs{
		List:   "https://proxy.golang.org/github.com/!azure/azure-sdk-for-go/@v/list",
		Info:   "https://proxy.golang.org/github.com/!azure/azure-sdk-for-go/@v/v1.2.3.info",
		Mod:    "https://proxy.golang.org/github.com/!azure/azure-sdk-for-go/@v/v1.2.3.mod",
		Zip:    "https://proxy.golang.org/github.com/!azure/azure-sdk-for-go/@v/v1.2.3.zip",
		Latest: "https://proxy.golang.org/github.com/!azure/azure-sdk-for-go/@latest",
	}
	if urls != want {
		t.Errorf("RequestURLs mismatch:\n got %+v\nwant %+v", urls, want)
	}
}

func TestParseInfo(t *testing.T) {
	t.Parallel()
	info, err := ParseInfo([]byte(`{"Version":"v1.2.3","Time":"2019-11-09T21:39:31Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "v1.2.3" {
		t.Errorf("Version = %q; want v1.2.3", info.Version)
	}
	want := time.Date(2019, 11, 9, 21, 39, 31, 0, time.UTC)
	if !info.Time.Equal(want) {
		t.Errorf("Time = %v; want %v", info.Time, want)
	}
}
```

Create `example_test.go`:

```go
package proxycodec

import "fmt"

func ExampleEncodePath() {
	enc, _ := EncodePath("github.com/Azure/azure-sdk-for-go")
	fmt.Println(enc)
	// Output: github.com/!azure/azure-sdk-for-go
}
```

## Review

The codec is correct when encoding and decoding are exact inverses for every valid
module path and the decoder rejects the three malformed shapes. The double-caps
case (`FooBar` becoming `!foo!bar`) is the one that catches a naive implementation
that only escapes the first uppercase letter. The URL builder is correct when it
encodes both the module path and the version and leaves `@` and `!` unescaped —
`net/url.JoinPath` already does the right thing, so do not hand-escape and
double-encode. Assert the malformed-input rejection with `errors.Is` against
`ErrBadEncoding`, and confirm the `.info` timestamp parses to the exact instant
rather than merely being non-zero.

## Resources

- [Go Modules Reference: GOPROXY protocol](https://go.dev/ref/mod#goproxy-protocol) — the five endpoints, the case-encoding, and the `.info` JSON shape.
- [`net/url.JoinPath`](https://pkg.go.dev/net/url#JoinPath) — path assembly that cleans without over-escaping.
- [`encoding/json.Unmarshal`](https://pkg.go.dev/encoding/json#Unmarshal) — decoding into a struct, including RFC 3339 into `time.Time`.
- [`unicode.IsUpper` / `unicode.ToLower`](https://pkg.go.dev/unicode) — the case predicates the encoding is built on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-goproxy-chain-parser.md](02-goproxy-chain-parser.md) | Next: [04-minimal-goproxy-server.md](04-minimal-goproxy-server.md)

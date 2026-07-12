# Exercise 9: Locale-Aware Content Negotiation

The locale a response is rendered in is set at the edge from `Accept-Language` and
read far downstream by whatever renders user-facing text. That distance — parsed at
the boundary, consumed deep in the handler — is exactly what makes it a legitimate
request-scoped value. This exercise builds the i18n edge: a parser, a middleware
that stores the resolved locale in the context, and a renderer that reads it to pick
a translated catalog.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
locale/                      independent module: example.com/locale
  go.mod
  locale.go                  ParseAcceptLanguage; WithLocale, LocaleFromContext;
                             Negotiate middleware; Render
  cmd/
    demo/
      main.go                renders greeting in fr, es, and the default
  locale_test.go             parser q-value ordering; middleware fallback; renderer reads ctx
```

Files: `locale.go`, `cmd/demo/main.go`, `locale_test.go`.
Implement: `ParseAcceptLanguage(header, supported, def)`, `WithLocale`/`LocaleFromContext`, a `Negotiate` middleware, and a `Render(ctx, key)` that reads the locale from the context.
Test: `Accept-Language: fr-FR,fr;q=0.9` resolves to `fr` and renders the French string; an unsupported `de` falls back to the default; a missing header uses the default; the parser handles q-value ordering and whitespace; the renderer takes no locale argument.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/06-context-withvalue/09-locale-content-negotiation/cmd/demo
cd go-solutions/14-select-and-context/06-context-withvalue/09-locale-content-negotiation
```

### Why locale is request-scoped, and how negotiation works

`Render` deep in a handler needs to know the locale, but forcing every function
between the edge and the renderer to carry a `locale string` parameter is precisely
the pollution context values exist to avoid. Deleting the locale does not break
correctness — the response still renders, just in the default language — so by the
boundary rule it is observability/presentation, not a required argument: legitimately
request-scoped. `Negotiate` resolves it once at the edge; `Render` reads it with
`LocaleFromContext`, defaulting to the fallback when absent so it never fails.

`ParseAcceptLanguage` implements the quality-value negotiation from RFC 9110. The
header is a comma-separated list of language ranges, each with an optional `;q=`
weight from 0 to 1 (default 1). The parser splits on commas, trims whitespace, reads
each tag and its q-value, and picks the highest-weighted tag that the server
supports — matching either the full tag (`fr-FR`) or its primary subtag (`fr`) against
the supported set. A `q=0` explicitly rejects a language. When nothing matches, it
returns the provided default. The renderer then indexes a small in-memory catalog by
locale, falling back to the default locale's catalog for an unknown key or language.

Matching by primary subtag is what lets `fr-FR` resolve to a server that supports
`fr`: browsers send region-qualified tags, servers usually stock language-level
catalogs, and the negotiation has to bridge that. Sorting by q-value descending
(stably, to keep the header's left-to-right preference among equal weights) is what
makes `en;q=0.8,fr;q=0.9` correctly prefer French.

Create `locale.go`:

```go
package locale

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type ctxKey struct{}

// WithLocale attaches a resolved locale to ctx.
func WithLocale(ctx context.Context, loc string) context.Context {
	return context.WithValue(ctx, ctxKey{}, loc)
}

// LocaleFromContext returns the resolved locale, or "" and false if absent.
func LocaleFromContext(ctx context.Context) (string, bool) {
	loc, ok := ctx.Value(ctxKey{}).(string)
	return loc, ok
}

type langPref struct {
	tag string
	q   float64
	pos int
}

// ParseAcceptLanguage resolves the best supported locale from an Accept-Language
// header, returning def when none of the offered languages is supported.
func ParseAcceptLanguage(header string, supported []string, def string) string {
	supportedSet := make(map[string]bool, len(supported))
	for _, s := range supported {
		supportedSet[s] = true
	}

	var prefs []langPref
	for i, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		tag := part
		q := 1.0
		if semi := strings.Index(part, ";"); semi >= 0 {
			tag = strings.TrimSpace(part[:semi])
			params := part[semi+1:]
			if eq := strings.Index(params, "q="); eq >= 0 {
				if v, err := strconv.ParseFloat(strings.TrimSpace(params[eq+2:]), 64); err == nil {
					q = v
				}
			}
		}
		if tag == "" {
			continue
		}
		prefs = append(prefs, langPref{tag: strings.ToLower(tag), q: q, pos: i})
	}

	// Highest q first; ties keep header order (stable by original position).
	sort.SliceStable(prefs, func(a, b int) bool {
		if prefs[a].q != prefs[b].q {
			return prefs[a].q > prefs[b].q
		}
		return prefs[a].pos < prefs[b].pos
	})

	for _, p := range prefs {
		if p.q == 0 {
			continue // q=0 explicitly rejects this language
		}
		if supportedSet[p.tag] {
			return p.tag
		}
		if primary := strings.SplitN(p.tag, "-", 2)[0]; supportedSet[primary] {
			return primary
		}
	}
	return def
}

// catalog maps locale -> message key -> translated text.
var catalog = map[string]map[string]string{
	"en": {"greeting": "Hello"},
	"fr": {"greeting": "Bonjour"},
	"es": {"greeting": "Hola"},
}

const defaultLocale = "en"

// Negotiate resolves the request's locale from Accept-Language and stores it in
// the context for downstream renderers.
func Negotiate(next http.Handler) http.Handler {
	supported := []string{"en", "fr", "es"}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loc := ParseAcceptLanguage(r.Header.Get("Accept-Language"), supported, defaultLocale)
		next.ServeHTTP(w, r.WithContext(WithLocale(r.Context(), loc)))
	})
}

// Render returns the translated message for key in the context's locale, falling
// back to the default locale. It takes no locale argument by design.
func Render(ctx context.Context, key string) string {
	loc, ok := LocaleFromContext(ctx)
	if !ok {
		loc = defaultLocale
	}
	if msgs, ok := catalog[loc]; ok {
		if msg, ok := msgs[key]; ok {
			return msg
		}
	}
	return catalog[defaultLocale][key]
}
```

### The demo

The demo drives three requests with different `Accept-Language` headers through
`Negotiate` into a handler that renders the greeting from the context locale.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/locale"
)

func main() {
	handler := locale.Negotiate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, locale.Render(r.Context(), "greeting"))
	}))

	for _, header := range []string{"fr-FR,fr;q=0.9", "es", "de"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Accept-Language", header)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		fmt.Printf("%s -> %s\n", header, rec.Body.String())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fr-FR,fr;q=0.9 -> Bonjour
es -> Hola
de -> Hello
```

### The tests

The parser is unit-tested directly for primary-subtag matching, q-value ordering,
whitespace tolerance, and the unsupported/empty fallbacks. The integration tests
drive `Negotiate -> Render` and assert the rendered body, with `Render` reading the
locale purely from the context — it is never handed a locale argument.

Create `locale_test.go`:

```go
package locale

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseAcceptLanguage(t *testing.T) {
	t.Parallel()

	supported := []string{"en", "fr", "es"}
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"primary subtag match", "fr-FR,fr;q=0.9", "fr"},
		{"q ordering prefers higher", "en;q=0.8,fr;q=0.9", "fr"},
		{"whitespace tolerated", "  es ; q=0.5 , en ; q=0.1 ", "es"},
		{"unsupported falls back", "de,it", "en"},
		{"empty falls back", "", "en"},
		{"q=0 rejects", "fr;q=0,es", "es"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ParseAcceptLanguage(tc.header, supported, "en"); got != tc.want {
				t.Fatalf("ParseAcceptLanguage(%q) = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

func TestNegotiateAndRender(t *testing.T) {
	t.Parallel()

	tests := []struct {
		header string
		want   string
	}{
		{"fr-FR,fr;q=0.9", "Bonjour"},
		{"es", "Hola"},
		{"de", "Hello"},
	}
	for _, tc := range tests {
		t.Run(tc.header, func(t *testing.T) {
			t.Parallel()
			var body string
			handler := Negotiate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body = Render(r.Context(), "greeting")
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Accept-Language", tc.header)
			handler.ServeHTTP(httptest.NewRecorder(), req)
			if body != tc.want {
				t.Fatalf("header %q rendered %q, want %q", tc.header, body, tc.want)
			}
		})
	}
}

func TestMissingHeaderUsesDefault(t *testing.T) {
	t.Parallel()

	var body string
	handler := Negotiate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = Render(r.Context(), "greeting")
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if body != "Hello" {
		t.Fatalf("missing header rendered %q, want Hello", body)
	}
}

func TestRenderDefaultsWithoutLocaleInContext(t *testing.T) {
	t.Parallel()

	if got := Render(context.Background(), "greeting"); got != "Hello" {
		t.Fatalf("Render on bare context = %q, want Hello", got)
	}
}
```

## Review

The negotiation is correct when `fr-FR` resolves to the server's `fr` catalog, when
a higher q-value wins regardless of header order, and when anything unsupported or
absent renders the default — the table test pins each case. The design point is that
`Render` accepts no locale argument: it reads the locale purely from the context set
by `Negotiate`, which is what lets a renderer buried under several call frames stay
locale-aware without every intermediate function growing a `locale` parameter. That
is the boundary rule's permissive side — a value that shapes presentation, not
correctness, and is consumed far from where it is set, is exactly what context values
are for. The parser's stable sort preserves header order among equal weights, so the
negotiation is deterministic.

## Resources

- [RFC 9110: Accept-Language](https://www.rfc-editor.org/rfc/rfc9110#field.accept-language) — the quality-value negotiation this parser implements.
- [sort.SliceStable](https://pkg.go.dev/sort#SliceStable) — stable ordering to keep header preference among equal q-values.
- [context.WithValue](https://pkg.go.dev/context#WithValue) — the request-scoped-value contract the locale rides on.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-boundary-rule-refactor.md](08-boundary-rule-refactor.md) | Next: [../07-context-propagation/00-concepts.md](../07-context-propagation/00-concepts.md)

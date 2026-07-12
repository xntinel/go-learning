# Exercise 1: A Batched, Normalized Embedding Client

You will build an `Embedder` that turns a slice of texts into pgvector-ready
vectors: it batches many inputs per request, applies an optional Matryoshka
dimension reduction, converts the API's `[]float64` into `[]float32`, and
L2-normalizes each vector so cosine and inner-product distance agree. The real
provider sits behind a small interface, so the default build and every test run
offline against a deterministic fake, and the network call lives behind a build
tag.

## What you'll build

```text
embed/                            independent module: example.com/embed
  go.mod                          go 1.26 (stdlib-only default build)
  embed.go                        Provider, Config, Embedder; batching, float32, L2 normalize
  provider_openai.go              //go:build online — OpenAIProvider over the SDK
  cmd/
    demo/
      main.go                     offline demo driven by HashProvider
  embed_test.go                   table-driven unit tests; sentinels via errors.Is; Example
  online_test.go                  //go:build online — real-endpoint smoke test
```

- Files: `embed.go`, `provider_openai.go`, `cmd/demo/main.go`, `embed_test.go`, `online_test.go`.
- Implement: a `Provider` port, a deterministic `HashProvider`, an `Embedder` that batches with an input cap, restores input order via the returned `Index`, converts `float64` to `float32`, and L2-normalizes; plus an `OpenAIProvider` behind `//go:build online`.
- Test: table-driven tests for the batch splitter, the `float64` to `float32` conversion, L2 normalization (magnitude approx 1.0), order preservation, and validation sentinels asserted with `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module. The default build is stdlib-only; the OpenAI SDK is needed
only for the `online`-tagged files:

```bash
mkdir -p go-solutions/52-ai-llm-backends/04-embeddings-and-pgvector/01-embedding-client/cmd/demo
cd go-solutions/52-ai-llm-backends/04-embeddings-and-pgvector/01-embedding-client
go get github.com/openai/openai-go/v3@latest   # only needed for -tags online
```

### The port, and why the real provider is behind a build tag

The `Embedder` never talks to a vendor directly. It depends on a one-method
`Provider` interface — "embed this batch of strings, return one vector each,
tagged with its position" — and everything vendor-specific lives on the other
side of that line. This is the same swap-the-provider discipline the chapter
uses for chat: Anthropic ships no embeddings endpoint (their guidance points to
Voyage AI), OpenAI's `text-embedding-3-*` is the concrete choice here, and the
interface is what lets a service change that decision in one place.

Concretely, the OpenAI adapter lives in a file tagged `//go:build online`, so
the default build — and therefore the whole test suite and the demo — compiles
with nothing but the standard library and runs with no API key and no network.
The deterministic `HashProvider` stands in for the real one everywhere offline.
That is not a testing hack; a deterministic local embedder is genuinely useful
for development and for reproducible tests, because a real embedding call is
non-deterministic across model versions.

### Batching, order, and the float64/float32 boundary

Three mechanical facts drive the design. First, the API embeds an array of
inputs in one call and returns them each tagged with an `Index`, so the
`Embedder` splits inputs into batches of at most `MaxBatch`, calls the provider
per batch, and places each returned vector at `batchStart + Index` — restoring
the caller's order even if a provider returns results out of order. Second, the
SDK returns `[]float64` but pgvector's `vector` is `float32`; the narrowing
conversion is mandatory and lossy, so it is one explicit, tested function rather
than an implicit truncation. Third, every stored vector is L2-normalized, so
cosine distance, negated inner product, and L2 all rank identically downstream —
and a Matryoshka-reduced vector, whose truncation changed its magnitude, is put
back on the unit sphere.

Create `embed.go`:

```go
package embed

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
)

const defaultMaxBatch = 96

// Sentinel errors, wrapped with %w at the call site so callers match with
// errors.Is.
var (
	ErrEmptyInput        = errors.New("embed: no input texts")
	ErrEmptyText         = errors.New("embed: input text is empty")
	ErrDimensionMismatch = errors.New("embed: provider returned wrong dimension")
	ErrShortResult       = errors.New("embed: provider returned wrong number of vectors")
)

// IndexedVector is one embedding returned by a Provider, tagged with its
// position within the request batch so the Embedder can restore input order.
type IndexedVector struct {
	Index  int
	Vector []float64
}

// Provider is the swappable embedding backend: one call embeds one batch of
// inputs. OpenAIProvider (build tag "online") is the production implementation;
// HashProvider is a deterministic offline stand-in for tests and local dev.
type Provider interface {
	Embed(ctx context.Context, model string, dimensions int, inputs []string) ([]IndexedVector, error)
}

// Config controls how the Embedder calls its Provider.
type Config struct {
	Model      string // provider model id, e.g. "text-embedding-3-small"
	Dimensions int    // 0 = model default; >0 = Matryoshka reduction and column width
	MaxBatch   int    // 0 = defaultMaxBatch; max inputs per provider call
}

// Embedder batches inputs, converts the provider's []float64 to pgvector-ready
// []float32, and L2-normalizes each vector.
type Embedder struct {
	provider Provider
	cfg      Config
}

// NewEmbedder builds an Embedder, defaulting an unset MaxBatch.
func NewEmbedder(p Provider, cfg Config) *Embedder {
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = defaultMaxBatch
	}
	return &Embedder{provider: p, cfg: cfg}
}

// Embed returns one unit-normalized float32 vector per input, in input order.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, fmt.Errorf("%w", ErrEmptyInput)
	}
	for i, t := range texts {
		if t == "" {
			return nil, fmt.Errorf("%w: index %d", ErrEmptyText, i)
		}
	}

	out := make([][]float32, len(texts))
	for _, b := range batches(len(texts), e.cfg.MaxBatch) {
		vecs, err := e.provider.Embed(ctx, e.cfg.Model, e.cfg.Dimensions, texts[b.start:b.end])
		if err != nil {
			return nil, err
		}
		if len(vecs) != b.end-b.start {
			return nil, fmt.Errorf("%w: batch [%d,%d) got %d", ErrShortResult, b.start, b.end, len(vecs))
		}
		for _, iv := range vecs {
			global := b.start + iv.Index
			if iv.Index < 0 || global >= b.end {
				return nil, fmt.Errorf("%w: index %d outside batch [%d,%d)", ErrShortResult, iv.Index, b.start, b.end)
			}
			if e.cfg.Dimensions > 0 && len(iv.Vector) != e.cfg.Dimensions {
				return nil, fmt.Errorf("%w: want %d got %d", ErrDimensionMismatch, e.cfg.Dimensions, len(iv.Vector))
			}
			out[global] = normalize(toFloat32(iv.Vector))
		}
	}
	return out, nil
}

type batchRange struct{ start, end int }

// batches splits n items into contiguous ranges of at most size, in order.
func batches(n, size int) []batchRange {
	if size <= 0 {
		size = defaultMaxBatch
	}
	var out []batchRange
	for start := 0; start < n; start += size {
		end := min(start+size, n)
		out = append(out, batchRange{start, end})
	}
	return out
}

// toFloat32 narrows the API's []float64 to the []float32 pgvector stores. The
// conversion is lossy and deliberate.
func toFloat32(v []float64) []float32 {
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(x)
	}
	return out
}

// normalize scales v to unit L2 length in place and returns it. A zero vector
// has no direction and is returned unchanged.
func normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := math.Sqrt(sum)
	if norm == 0 {
		return v
	}
	for i, x := range v {
		v[i] = float32(float64(x) / norm)
	}
	return v
}

// HashProvider is a deterministic, offline Provider: it derives a stable vector
// from each input's bytes. It is not a real embedder (nearby vectors carry no
// meaning), but it makes tests, demos, and local development run with no API key
// and no network.
type HashProvider struct {
	// Dim is the vector length when the caller requests the model default
	// (dimensions == 0). Defaults to 8 when zero.
	Dim int
}

// Embed implements Provider deterministically.
func (h HashProvider) Embed(_ context.Context, _ string, dimensions int, inputs []string) ([]IndexedVector, error) {
	dim := dimensions
	if dim <= 0 {
		dim = h.Dim
	}
	if dim <= 0 {
		dim = 8
	}
	out := make([]IndexedVector, len(inputs))
	for i, in := range inputs {
		hsh := fnv.New64a()
		_, _ = hsh.Write([]byte(in))
		seed := float64(hsh.Sum64() % 1000)
		vec := make([]float64, dim)
		for j := range dim {
			vec[j] = math.Sin(seed + float64(j))
		}
		out[i] = IndexedVector{Index: i, Vector: vec}
	}
	return out, nil
}
```

### The production provider, behind the online tag

The real adapter is small: build the params, set the optional `Dimensions`
through the `param.Opt[int64]` constructor `openai.Int` so an unset reduction is
omitted rather than sent as zero, call the endpoint, and map each
`openai.Embedding` (with its `Index` and `[]float64` `Embedding`) into the
neutral `IndexedVector`. It is compiled only under `-tags online`.

Create `provider_openai.go`:

```go
//go:build online

package embed

import (
	"context"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// OpenAIProvider is the production Provider backed by the OpenAI embeddings
// endpoint. Compiled only under the "online" build tag so the default build and
// test path stay offline and dependency-free.
type OpenAIProvider struct {
	client openai.Client
}

// NewOpenAIProvider constructs the adapter. Pass option.WithAPIKey (and
// option.WithBaseURL to target a gateway); the zero-arg SDK client reads
// OPENAI_API_KEY from the environment.
func NewOpenAIProvider(opts ...option.RequestOption) *OpenAIProvider {
	return &OpenAIProvider{client: openai.NewClient(opts...)}
}

// Embed sends the whole batch as an array input and maps the response back.
func (p *OpenAIProvider) Embed(ctx context.Context, model string, dimensions int, inputs []string) ([]IndexedVector, error) {
	params := openai.EmbeddingNewParams{
		Model: openai.EmbeddingModel(model),
		Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: inputs},
	}
	if dimensions > 0 {
		// param.Opt constructor: set only when a reduction was asked for.
		params.Dimensions = openai.Int(int64(dimensions))
	}

	resp, err := p.client.Embeddings.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("openai embeddings: %w", err)
	}

	out := make([]IndexedVector, len(resp.Data))
	for i, d := range resp.Data {
		out[i] = IndexedVector{Index: int(d.Index), Vector: d.Embedding}
	}
	return out, nil
}
```

### The runnable demo

The demo runs fully offline against `HashProvider`. It reduces to 8 dimensions
and uses a `MaxBatch` of 2 to exercise the splitter, then prints each vector's
L2 magnitude — which is `1.0000` for every one, proving normalization ran.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"math"

	"example.com/embed"
)

func main() {
	e := embed.NewEmbedder(embed.HashProvider{}, embed.Config{
		Model:      "text-embedding-3-small",
		Dimensions: 8,
		MaxBatch:   2,
	})

	texts := []string{
		"invoices past due",
		"reset my password",
		"cancel subscription",
		"upgrade to enterprise",
		"export data to csv",
	}

	vecs, err := e.Embed(context.Background(), texts)
	if err != nil {
		fmt.Println("embed:", err)
		return
	}

	fmt.Printf("embedded %d texts into %d-dim vectors\n", len(vecs), len(vecs[0]))
	for i, v := range vecs {
		fmt.Printf("vector %d magnitude: %.4f\n", i, magnitude(v))
	}
}

func magnitude(v []float32) float64 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	return math.Sqrt(sum)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
embedded 5 texts into 8-dim vectors
vector 0 magnitude: 1.0000
vector 1 magnitude: 1.0000
vector 2 magnitude: 1.0000
vector 3 magnitude: 1.0000
vector 4 magnitude: 1.0000
```

### Tests

The unit tests pin each mechanism independently. `TestEmbedBatchesRespectCap`
uses a provider that records the size of each batch it receives and asserts a
five-input, cap-two run arrives as `[2, 2, 1]`. `TestEmbedPreservesOrder` drives
a provider that returns one-hot vectors *reversed*, tagged with the correct
`Index`, and asserts the Embedder still lays them out in input order — proving
order comes from `Index`, not slice position. `TestToFloat32` and the
normalization tests check the two numeric helpers directly, and
`TestEmbedValidation` asserts each sentinel with `errors.Is`.

Create `embed_test.go`:

```go
package embed

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"
)

// recordingProvider records the size of each batch it is asked to embed.
type recordingProvider struct{ sizes []int }

func (r *recordingProvider) Embed(_ context.Context, _ string, _ int, inputs []string) ([]IndexedVector, error) {
	r.sizes = append(r.sizes, len(inputs))
	out := make([]IndexedVector, len(inputs))
	for i := range inputs {
		out[i] = IndexedVector{Index: i, Vector: []float64{1, 2, 3}}
	}
	return out, nil
}

// oneHotProvider returns unit basis vectors, placed in REVERSE order but tagged
// with the correct Index, so a correct Embedder still restores input order.
type oneHotProvider struct{}

func (oneHotProvider) Embed(_ context.Context, _ string, _ int, inputs []string) ([]IndexedVector, error) {
	n := len(inputs)
	out := make([]IndexedVector, n)
	for i := range inputs {
		vec := make([]float64, n)
		vec[i] = 1
		out[n-1-i] = IndexedVector{Index: i, Vector: vec}
	}
	return out, nil
}

// fixedDimProvider ignores the requested dimension and always returns dim-length
// vectors, to trigger the dimension-mismatch guard.
type fixedDimProvider struct{ dim int }

func (f fixedDimProvider) Embed(_ context.Context, _ string, _ int, inputs []string) ([]IndexedVector, error) {
	out := make([]IndexedVector, len(inputs))
	for i := range inputs {
		out[i] = IndexedVector{Index: i, Vector: make([]float64, f.dim)}
	}
	return out, nil
}

func TestEmbedBatchesRespectCap(t *testing.T) {
	t.Parallel()
	rp := &recordingProvider{}
	e := NewEmbedder(rp, Config{Model: "m", MaxBatch: 2})
	if _, err := e.Embed(context.Background(), []string{"1", "2", "3", "4", "5"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if want := []int{2, 2, 1}; !slices.Equal(rp.sizes, want) {
		t.Fatalf("batch sizes = %v, want %v", rp.sizes, want)
	}
}

func TestEmbedPreservesOrder(t *testing.T) {
	t.Parallel()
	texts := []string{"a", "b", "c", "d"}
	e := NewEmbedder(oneHotProvider{}, Config{Model: "m", MaxBatch: 16})
	got, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	for k := range texts {
		for j := range got[k] {
			want := float32(0)
			if j == k {
				want = 1
			}
			if got[k][j] != want {
				t.Fatalf("vector %d component %d = %v, want %v", k, j, got[k][j], want)
			}
		}
	}
}

func TestToFloat32(t *testing.T) {
	t.Parallel()
	got := toFloat32([]float64{0, 1, -0.5, 3.25})
	if want := []float32{0, 1, -0.5, 3.25}; !slices.Equal(got, want) {
		t.Fatalf("toFloat32 = %v, want %v", got, want)
	}
}

func TestNormalizeUnitLength(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []float32
	}{
		{"axis", []float32{3, 0, 0}},
		{"mixed", []float32{1, 2, 2}},
		{"negatives", []float32{-1, -1, -1, -1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := normalize(slices.Clone(tc.in))
			var sum float64
			for _, x := range v {
				sum += float64(x) * float64(x)
			}
			if mag := math.Sqrt(sum); math.Abs(mag-1) > 1e-6 {
				t.Fatalf("magnitude = %v, want approx 1", mag)
			}
		})
	}
}

func TestNormalizeZeroVector(t *testing.T) {
	t.Parallel()
	v := normalize([]float32{0, 0, 0})
	for _, x := range v {
		if x != 0 {
			t.Fatalf("zero vector changed: %v", v)
		}
	}
}

func TestEmbedValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		texts   []string
		cfg     Config
		prov    Provider
		wantErr error
	}{
		{"empty input", nil, Config{Model: "m"}, HashProvider{}, ErrEmptyInput},
		{"empty text", []string{"ok", ""}, Config{Model: "m"}, HashProvider{}, ErrEmptyText},
		{"dimension mismatch", []string{"x"}, Config{Model: "m", Dimensions: 16}, fixedDimProvider{dim: 4}, ErrDimensionMismatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := NewEmbedder(tc.prov, tc.cfg)
			_, err := e.Embed(context.Background(), tc.texts)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func ExampleEmbedder_Embed() {
	e := NewEmbedder(HashProvider{}, Config{Model: "text-embedding-3-small", Dimensions: 8})
	vecs, _ := e.Embed(context.Background(), []string{"hello", "world"})
	fmt.Printf("%d vectors, %d dims each\n", len(vecs), len(vecs[0]))
	// Output: 2 vectors, 8 dims each
}
```

The online smoke test hits the real endpoint and is compiled only with
`-tags online` and a key present. It proves the adapter's wiring and that
`text-embedding-3-*` returns already-normalized vectors.

Create `online_test.go`:

```go
//go:build online

package embed

import (
	"context"
	"math"
	"os"
	"testing"

	"github.com/openai/openai-go/v3/option"
)

// TestOpenAIProviderSmoke calls the real embeddings endpoint. Run with:
//
//	OPENAI_API_KEY=sk-... go test -tags online -run OpenAI ./...
func TestOpenAIProviderSmoke(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	e := NewEmbedder(NewOpenAIProvider(option.WithAPIKey(key)), Config{
		Model:      "text-embedding-3-small",
		Dimensions: 256,
	})
	vecs, err := e.Embed(context.Background(), []string{"hello", "world"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 2 || len(vecs[0]) != 256 {
		t.Fatalf("got %d vectors, first dim %d; want 2 vectors of 256", len(vecs), len(vecs[0]))
	}
	var sum float64
	for _, x := range vecs[0] {
		sum += float64(x) * float64(x)
	}
	if mag := math.Sqrt(sum); math.Abs(mag-1) > 1e-3 {
		t.Fatalf("vector magnitude = %v, want approx 1", mag)
	}
}
```

## Review

The Embedder is correct when the output slice is one unit vector per input, in
input order, regardless of how the provider batches or orders its results. The
order guarantee comes from placing each vector at `batchStart + Index`, which is
why `TestEmbedPreservesOrder` deliberately feeds reversed results and still
expects input order; if a reimplementation used the provider's slice position
instead of `Index`, that test fails. Normalization is verified by magnitude
rather than by exact components, because float32 rounding perturbs the last
digits — the invariant is "unit length", not "these bytes".

The mistakes to avoid are the ones the code is shaped to prevent. Do not store
the SDK's `[]float64` as if pgvector could take it: `toFloat32` exists because
`pgvector.NewVector` needs `[]float32`, and the narrowing is lossy. Do not skip
re-normalization after a Matryoshka reduction — a truncated vector is off the
unit sphere, and mixing normalized and un-normalized vectors makes inner-product
distance meaningless. Do not send a zero `Dimensions`; the adapter only sets the
parameter when a reduction was asked for, so the model default is used
otherwise. And keep the provider behind the interface: swapping OpenAI for
Voyage or a local model should touch only the adapter file, never the Embedder.
Run `go test -race` to confirm the Embedder is safe when one instance is shared
across request-handling goroutines.

## Resources

- [OpenAI Go SDK v3 — pkg.go.dev](https://pkg.go.dev/github.com/openai/openai-go/v3) — `EmbeddingNewParams`, `EmbeddingNewParamsInputUnion`, and `EmbeddingModel`.
- [openai-go — GitHub](https://github.com/openai/openai-go) — client construction, the `option` package, and embeddings usage in the README.
- [OpenAI embeddings guide](https://platform.openai.com/docs/guides/embeddings) — `text-embedding-3-*`, the `dimensions` parameter, and normalization.
- [Matryoshka Representation Learning](https://arxiv.org/abs/2205.13147) — why a `text-embedding-3-*` vector can be truncated and still be useful.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-pgvector-store.md](02-pgvector-store.md)

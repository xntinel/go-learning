# 01 — The Quality Standard

What a finished Go lesson must look like. Canonical source:
`../AGENT-ORDER-go-quality.md` + `challenges/CURRICULUM-METHODOLOGY.md`
(§5 skeleton, §14.1 rubric A-E, §15 self-consistency, §0 form rules). This file
is the working summary; when in doubt, the methodology wins.

## §5 — Document skeleton

```
# N. Title
<short intro: what is distinctive/hard about this topic; no patronizing opener>
## Concepts          ### subsections: the model, the hard "why", trade-offs, failure modes
## Exercises         2-3 real whole-task exercises that BUILD the artifact + a real *_test.go
## Common Mistakes   highest-value traps; Wrong approach beside the Fix, tied to THIS lesson's code
## Verification      exact commands to check the artifact against ground truth
## Summary           tight bullets
## What's Next        one line; the relative link to the next lesson .md must resolve
## Resources         3-5 good primary sources, cited where factual
```

Keep the existing `# N. Title` H1. Do not rename or reorder files/folders.

### Banned sections / markers (remove on sight)

- The HTML metadata comment block (`difficulty/concepts/tools/bloom_level/
  prerequisites/estimated_time`). The `bloom_level:` marker is THE signal of an
  un-rewritten lesson.
- `## Prerequisites` ("Go 1.26+ installed" boilerplate).
- `## Learning Objectives` (bolded Bloom verbs).
- `<details>` blocks, emojis, decorative symbols, non-English text.
- NOT banned: `## Common Mistakes` is part of the skeleton and is encouraged.

> Never delete a `## The Problem` section without migrating its content first —
> it often holds the only code and the problem statement. A lesson left with a
> placeholder `## Exercises` or a `## Verification` referencing code that no
> longer exists is BROKEN, not clean.

## §14.1 — Exercise quality (the highest-priority rule)

A library lesson is verified with a real `*_test.go`, NOT with a `main()` that
prints an "Expected output" block. A demo `main()` does not fail when the code
regresses. Every library lesson ships THREE things:

1. A real `*_test.go` — table-driven, `t.Parallel()`, validation errors asserted
   with `errors.Is` against package-level **sentinel errors wrapped with `%w`**.
   Use `httptest`/`fstest` where relevant. No network in the default test path
   (put any network smoke behind `//go:build online` in a separate file).
2. An `Example` function with an `// Output:` comment (auto-verified by
   `go test`):

   ```go
   func ExampleNew() {
   	s, _ := New(WithPort(443), WithHost("localhost"))
   	fmt.Printf("%s:%d\n", s.Addr(), s.Port())
   	// Output: localhost:443
   }
   ```

3. A real `cmd/demo/main.go`, runnable with `go run ./cmd/demo`, with an
   Expected-output block that matches a real run. Because `cmd/demo` is a
   separate `package main`, it can only touch EXPORTED API — add small exported
   accessors (`func (s *Server) Port() int`) rather than exporting raw fields.

Rules:
- The lesson is a real package (e.g. `package server`), NOT `package main`, so
  same-package tests can reach unexported fields.
- Each exercise builds the artifact; the final exercise is the `*_test.go`.
- `## Verification` runs `go test -count=1 -race ./...`, never `go run main.go`
  (unless the lesson IS a real CLI).
- End the test exercise with one "Your turn:" test for the learner to add.

The gold template for converting a `main()`-based lesson into a real testable one
is the functional-options walkthrough in `../AGENT-ORDER-go-quality.md`
(Exercises 1-4). Imitate its shape.

## §15 — Self-consistency (run on every lesson)

The gate proves the code compiles; §15 proves the prose matches the code. Every
"the code does X" sentence must be true of the code. If Common Mistakes says
"the code wraps with `%w`", it must. If an exercise says "write a test asserting
X", the lesson code must implement X. Every Expected-output block must match a
real run; no fabricated output or line numbers. A lesson can pass the gate and
still fail §15.

## §0 / Hard rules

- **TABS, not spaces**, for all Go in fenced blocks. `gofmt -l` flags
  space-indented Go and that fails the gate. Verify: extract a block, run
  `gofmt -l` — must be empty. Space-indented Go = automatic batch failure.
- No emojis / decorative symbols (`✓ ⭐ ▶ ⚠ 🚀` …). Use "OK"/"FAIL",
  "Wrong"/"Fix". Only functional math operators allowed.
- English everywhere.
- Never invent a Go API. Every stdlib signature must be real and compile on
  `go 1.26`. Prefer stdlib (`strconv.Itoa`, `http.StatusText`, …) over
  hand-rolled equivalents.
- No dead code: every branch reachable, and exercised by a test where it matters.
- Modern Go idioms: Go 1.22+ loop-var scoping (NO `tc := tc` / `i := i`),
  `for i := range n`, `t.Context()` (1.24+), `cmp.Ordered`, generics where they
  fit.

## Research before writing (quality lift)

Ground `## Concepts` in correct, current material and verify every API. Priority:
1. Official Go: `go.dev/ref/spec`, `go.dev/doc/effective_go`, the Go Blog,
   `pkg.go.dev` for exact stdlib signatures/behavior.
2. The real source of any library the lesson names (how `grpc`, `net/http`,
   `cobra`, `zap` actually do it) so it reflects real usage, not a toy.
3. Idiom references: Go Code Review Comments, Uber Go Style Guide.

If a source contradicts memory, trust the source. Keep `## Resources` to the
3-5 best primary pages you actually consulted (cite the specific page).

## Verification regime by chapter type

- **Compilable-offline chapters:** run the full gate (gofmt/vet/build/test-race).
  A lesson that does not PASS is rewritten, not marked OK. `SKIP` for "no
  extractable code" is unacceptable in a lesson that should have code.
- **Capstones / not-compilable-offline** (Linux namespaces, cgroups, cgo,
  external modules, asm, profiles): the bar is a full §5 rewrite with realistic
  substantial code, validated by §15 + `gofmt` + `go vet` on extractable code.
  Build/test is deferred to a networked/Linux run. State plainly in the report:
  "not compiled offline; validated by §15 + gofmt + vet" (mode "bar").

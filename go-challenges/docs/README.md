# Go Curriculum — Authoring & Quality Docs

This folder is the self-contained playbook for **producing and expanding** the
Go curriculum under `challenges/go/` at the same quality bar. Read it before
adding a new lesson, a new chapter, or a new topic.

Status: the existing curriculum (47 chapters, ch01-47) is **gate-clean** —
`bloom_level` marker = 0, every compilable lesson passes the gate, capstones
meet the offline bar. These docs capture the criteria, tools, and process used
to get there so future work returns with the same quality.

## Start here: the quality criteria

**Content quality is the only judge.** The explicit rubric — the A-E criteria,
artifact quality, self-consistency, and (critically) how to MAINTAIN and INCREASE
quality from "passes" to "excellent" — is in
[`05-quality-criteria.md`](05-quality-criteria.md). Read it first and apply it to
every lesson, new or existing. Everything else (shape, gate, engine) exists to
serve these criteria.

## The pillars

0. **The quality criteria (the rubric)** — what makes content correct, deep,
   honest, and senior-grade, and how to raise it.
   See [`05-quality-criteria.md`](05-quality-criteria.md).

1. **The standard (shape)** — what a finished lesson must look like.
   See [`01-quality-standard.md`](01-quality-standard.md).
   Canonical source: `../AGENT-ORDER-go-quality.md` and the user's
   `challenges/CURRICULUM-METHODOLOGY.md` (§5 skeleton, §6/§7 artifact quality &
   bloat, §14.1 rubric, §15 self-consistency, §0 form rules).

2. **The gate** — the model-independent ground truth that says a lesson is real.
   See [`02-gate-tool.md`](02-gate-tool.md).
   Tool: `challenges/tools/go_gate_append.py`.

3. **The engine** — the generate -> adversarial-verify -> repair pipeline that
   scales lesson production, plus the methodology and batching rules.
   See [`03-engine-and-methodology.md`](03-engine-and-methodology.md).
   Tools: `../.go-quality-engine.workflow.js`, `../.gen-engine-args.py`,
   `../.go-fix-gofmt.workflow.js`.

## Adding new exercises / topics

The end-to-end runbook — pick topics, scaffold files, choose gate vs bar, run
the engine in batches, verify, and update the registry — is in
[`04-expansion-runbook.md`](04-expansion-runbook.md). It also carries the
hard-won lessons (session limits, stalled agents, mechanical sweeps) and the
current chapter map.

## Single sources of truth (do not duplicate; link)

- Standard / authoring order: `../AGENT-ORDER-go-quality.md`
- Methodology (§5/§14.1/§15/§0): `challenges/CURRICULUM-METHODOLOGY.md`
- Live progress + history: `../QUALITY-PROGRESS.md`
- Gold reference lessons to imitate (structure/depth, not content):
  - `../07-structs-and-methods/01-struct-declaration-and-initialization/`
  - `../10-error-handling/` (any lesson)
  - `../01-environment-and-tooling/01-your-first-go-program/` (CLI artifact)

## Non-negotiables (the short list)

- TAB-indented Go in every fenced block (spaces fail `gofmt -l` = batch failure).
- No emojis or decorative symbols. English only.
- A library lesson is verified by a real `*_test.go` PLUS an `Example` with
  `// Output:` PLUS a `cmd/demo/main.go` — never by an eyeballed `printf`.
- Never invent a Go API; verify every signature on `pkg.go.dev`; prefer stdlib.
- The prose must describe what the code actually does (§15).

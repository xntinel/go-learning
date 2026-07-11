# Helm Packaging and GitOps Delivery — Concepts

A senior backend engineer rarely hand-writes a chart in a vacuum and runs `helm
install` from a laptop. The real work is the tooling and the CI/CD gates that
render, validate, and deliver charts safely at scale: the PR check that rejects a
bad `values.yaml` before it merges, the pipeline that renders a chart to plain
manifests and diffs them against what is live, the controller that embeds the same
rendering logic inside the cluster. All of that is Go, and all of it is built on
Helm's Go SDK rather than on shelling out to the `helm` binary. This file is the
conceptual foundation for the three exercises that follow: rendering and
schema-gating a chart offline, turning a render into a diffable desired-state tree
with drift detection, and delivering that tree to a git repository as an
idempotent commit. Every one of those steps is pure computation with no cluster
and no network, which is exactly what makes it testable in CI and exactly what
makes it the on-the-job reality.

## Concepts

### Helm as a library, not a CLI

The `helm` command-line tool is a thin wrapper over a Go SDK. The package
`helm.sh/helm/v3/pkg/action` is the same client the CLI drives; underneath it,
`pkg/chart/loader` loads charts, `pkg/chartutil` composes and validates values,
and `pkg/engine` renders templates. When you build backend tooling you import the
SDK directly instead of running `os/exec` against the binary. The payoff is
concrete: you get typed Go errors instead of parsing stderr, you get deterministic
offline rendering you can assert in a unit test, and you get code you can embed
inside an operator, an admission webhook, or a GitOps controller. The SDK has
rough edges — it drags in a large `k8s.io/*` transitive dependency tree, and some
function names are historical — but the project commits to API stability across
the v3 major version, which is what makes it safe to depend on.

Shelling out to the CLI throws all of that away. You lose typed errors, you cannot
test without the binary on `PATH`, and you inherit whatever Helm version the host
happens to have. Importing the SDK is the difference between a testable component
and a fragile script.

### The two-phase model: rendering vs installing

Helm does two very different things, and keeping them separate is the key mental
model. *Rendering* is pure: given a chart and a set of values, `engine.Render`
produces Kubernetes manifests as strings. It needs no cluster, no kubeconfig, no
network — it is a template evaluation. *Installing* is not pure: `pkg/action`'s
`Install.RunWithContext` talks to an API server, creates objects, and records
release state. Rendering is deterministic and unit-testable; installing requires a
live cluster and belongs in integration tests.

Almost everything valuable for CI lives on the rendering side of that line. A PR
check renders and validates; it does not install. Trying to unit-test an install
path is a category error — you would need a real cluster, at which point it is an
integration test, not a unit test. Draw the boundary explicitly and keep your
fast, offline logic entirely within the render phase.

### Composing render values

A template does not just read `.Values`. It reads `.Release.Name`,
`.Release.Namespace`, `.Chart.Version`, `.Capabilities.KubeVersion`, `.Files`, and
`.Template.Name`. That whole top-level context is what `chartutil.ToRenderValues`
builds:

```go
vals, err := chartutil.ToRenderValues(chrt, overrides, relOpts, chartutil.DefaultCapabilities)
out, err := engine.Engine{Strict: true}.Render(chrt, vals)
```

`ToRenderValues(chrt, chrtVals, options, caps)` merges the user-supplied
`chrtVals` with the chart's own defaults and assembles the `.Values`,
`.Release`, `.Chart`, `.Capabilities`, and `.Files` keys. The single most common
SDK mistake is handing `engine.Render` a raw values map — `map[string]interface{}{
"replicaCount": 3}` — instead of a context built by `ToRenderValues`. A template
that reads `.Release.Name` or `.Capabilities.KubeVersion` then renders empty or
panics, because those keys simply are not present. `DefaultCapabilities` supplies a
sane built-in Kubernetes version so you do not need a cluster to answer
`.Capabilities` questions during an offline render.

### Values precedence and coalescing

`chartutil.CoalesceValues` (which `ToRenderValues` calls internally) merges the
user's overrides over the chart's defaults, and parent-chart values over
subchart values. The rule that trips people up is that the merge is asymmetric:
**maps are deep-merged, but scalars and arrays are replaced wholesale**. If the
chart default is `ports: [80, 443]` and you override with `ports: [8080]`, you get
`[8080]`, not `[80, 443, 8080]`. People expect arrays to merge the way maps do and
are surprised when an override silently drops the defaults. Subchart values live
under the subchart's name as a key, so an override for a dependency named `redis`
goes under `redis:` at the top level.

### Schema-gated values

A chart can ship a `values.schema.json` — a JSON Schema document — and
`chartutil.ValidateAgainstSchema(chrt, values)` checks the coalesced values
against it. This is the production guardrail that turns a typo into a fast PR
failure instead of a broken rollout. If someone sets `replicaCount: "three"` as a
string, schema validation fails in the pull request; without it, the bad value
sails through until a Deployment refuses to apply. Schema validation is separate
from — and complementary to — the template `required` function and strict
rendering. `ValidateAgainstSchema` walks the chart and its subcharts;
`ValidateAgainstSingleSchema(values, schemaJSON)` checks one values map against one
schema and is the primitive underneath it.

### Strict vs LintMode rendering

`engine.Engine` has two flags that change how missing values are treated. With
`Strict: true`, a template that references a value which was never provided fails
the render — a misspelled `.Values.replicaCont` becomes an error instead of an
empty string. With `LintMode: true`, some missing `required` values are tolerated,
which is what a linter wants. For a CI gate, choose `Strict`. The default
(non-strict) engine substitutes `<no value>` or empty for an undefined reference,
so a typo produces a syntactically valid but semantically wrong manifest that
ships silently to the cluster. Strict rendering is how you catch that at build
time.

### GitOps first principles

GitOps rests on four ideas. The desired state of the system is declared
*declaratively* and stored in *git* as the single source of truth. An agent
running in or near the cluster *continuously reconciles* actual state toward the
declared state. *Drift* — any divergence between actual and desired — is detected
and corrected. And every change is an *auditable, revertable commit*. The pull
model that Argo CD and Flux implement (an in-cluster agent pulls from git) beats
the older push model (a CI job runs `kubectl apply`) on two axes that matter to a
senior engineer: auditability, because the git history is the deploy log, and
blast radius, because the cluster's credentials never leave the cluster.

### The rendered-manifests pattern

There are two ways to combine Helm with GitOps. You can commit the *chart* and let
the in-cluster controller template it at apply time, or you can render the chart to
plain manifests in CI and commit the *output* to an environment repository that
the controller then reconciles verbatim. The second is the rendered-manifests
pattern, and it is what mature Argo CD and Flux shops converge on. The trade-off is
explicit: you gain reviewable diffs (a reviewer sees exactly which Kubernetes
fields change), deterministic deploys (no in-cluster templating surprises from a
different Helm version), and a clean audit trail, at the cost of a larger
repository and a render step per environment. The exercises build this pattern's
core: render, split into individually named manifests, diff for drift, and commit.

### Determinism is a hard requirement

The rendered-manifests pattern only works if the render is *reproducible*. If the
same inputs can produce different bytes, the GitOps controller sees perpetual
drift and never reaches a steady state. The usual sources of non-determinism are
all in your control: Go map iteration order (never write files or concatenate
documents in map order — sort the keys first), timestamps (`time.Now()` embedded
in output makes every render different), and unsorted lists. Filenames must be
derived deterministically from stable identity — apiVersion, kind, and name — not
from iteration order. Where reproducibility matters, even commits must use a fixed
author and committer signature rather than the wall clock.

### Idempotent delivery

Delivering rendered manifests to a git repository must be idempotent: if nothing
changed, the delivery is a no-op, not an empty commit. Check the worktree status
after staging; if it is clean, return without committing. Producing an empty commit
on unchanged input makes the reconciler churn and fills the history with noise. The
failure modes to guard against are exactly three: empty commits on no-change,
non-deterministic ordering causing perpetual drift, and — the security one —
committing rendered `Secret` objects in plaintext. Secrets belong in
sealed-secrets, SOPS, or an external-secrets reference, never as base64 in a repo
that GitOps makes widely readable.

### Environment promotion

Dev, stage, and prod are modeled as separate value overlays or as distinct paths
or branches in the environment repo. Promotion from stage to prod becomes a diff
and merge of *rendered manifests*, which means a reviewer can see precisely what
will change in production before it happens — not "bump the chart version and
hope", but the exact field-level delta. This is the operational reason the pattern
is worth its extra machinery.

### Dependency hygiene

Two pins matter for production. Use `github.com/go-git/go-git/v5`, which is stable,
rather than the v6 alpha. Pin the `helm.sh/helm/v3` module, and be aware it pulls
in a large `k8s.io/*` dependency graph that affects build times and CVE surface —
budget for keeping it patched. Because these are external modules, resolving them
requires module-mode fetching (`GOFLAGS=-mod=mod`) in any environment where they
are not already in the module cache.

## Common Mistakes

### Shelling out to the helm CLI

Wrong: `exec.Command("helm", "template", ...)` from Go, then scraping stdout and
stderr. You lose typed errors, cannot test without the binary, and inherit the
host's Helm version.

Fix: import `pkg/chart/loader`, `pkg/chartutil`, and `pkg/engine` and render in
process. The result is a typed, testable, embeddable component.

### Passing a raw values map to engine.Render

Wrong: `engine.Render(chrt, chartutil.Values{"replicaCount": 3})`. Templates that
read `.Release.Name` or `.Capabilities.KubeVersion` render empty or panic because
those keys are absent.

Fix: build the context with `chartutil.ToRenderValues(chrt, overrides, relOpts,
chartutil.DefaultCapabilities)` and pass that to the engine.

### Expecting arrays to deep-merge

Wrong: assuming an override array is appended to the default array during coalesce.
Arrays are replaced wholesale, so the defaults vanish silently.

Fix: know that only maps deep-merge; when overriding an array, supply the complete
intended list.

### Skipping schema validation

Wrong: rendering without `values.schema.json`, so `replicaCount: "three"` is only
caught when the Deployment fails to apply.

Fix: ship a schema and call `ValidateAgainstSchema` in the PR gate. A type error
becomes a fast, cheap failure.

### Rendering non-strict

Wrong: using the default engine, so a misspelled value key yields an empty
substitution that quietly ships to the cluster.

Fix: set `engine.Engine{Strict: true}` in CI so an undefined reference fails the
render.

### Non-deterministic output

Wrong: iterating a Go map to write files or concatenate documents, embedding
`time.Now()`, or leaving list order unstable. The controller reports perpetual
drift.

Fix: sort keys before every write and concatenation, derive filenames from stable
identity, and use fixed signatures where reproducibility matters.

### Empty commits on no change

Wrong: committing unconditionally, producing an empty commit whenever inputs are
unchanged, which makes the reconciler churn.

Fix: stage, check `Worktree.Status().IsClean()`, and skip the commit when nothing
changed.

### Committing plaintext secrets

Wrong: writing rendered `Secret` objects with base64 data straight into the
environment repo.

Fix: use sealed-secrets, SOPS, or external-secrets so the repo never holds
recoverable secret material.

### Confusing rendering with installing

Wrong: trying to unit-test an install path that needs a kubeconfig and API server.

Fix: unit-test the render phase, which is offline and pure; put install coverage in
integration tests behind a build tag.

### Depending on go-git v6 alpha

Wrong: importing the v6 alpha, or forgetting module-mode fetching so the external
module never resolves in the gate.

Fix: pin `go-git/v5` and pin `helm/v3`; ensure module-mode fetching where the cache
is cold.

Next: [01-render-and-validate-chart.md](01-render-and-validate-chart.md)

# Protobuf Schemas and the buf Workflow — Concepts

In a real organization the `.proto` files are not an implementation detail of one
service; they are the API contract, a shared and versioned artifact that every
service and every client language compiles against. The interesting engineering
is therefore not "how do I run `protoc`" but "how do I keep a schema module
lintable, backward-compatible, and machine-validated while dozens of engineers
evolve it in parallel." That is the job `buf` does: it turns a directory of
`.proto` files into a governed module with enforced style, a CI gate that
mathematically rejects wire-breaking edits before they merge, reproducible
codegen, and a runtime validation contract that lives in the schema instead of
being re-implemented in every service. This file is the conceptual foundation for
the three exercises that follow; read it once and you have the model you need for
all of them.

Because `buf` is a CLI toolchain and the generated code depends on plugins and
modules that are not present in the offline gate sandbox, these exercises are
bar-mode: every consumer of generated code sits behind a `//go:build bufgen`
constraint, and correctness is proven by `gofmt`-clean sources plus documented,
reproducible `buf` commands rather than by an offline compile.

## Concepts

### The schema module is the product

A protobuf schema is a shared, versioned artifact consumed by every language and
service that talks to the API. `buf` formalizes that artifact as a *module*: a
directory of `.proto` files with a `buf.yaml` at its root that declares the module
boundary, and a `buf.lock` that pins the digests of any dependencies so a build is
reproducible byte-for-byte. Thinking of the schema as a product — with an owner, a
version, a compatibility policy, and a changelog — is the mental shift that makes
the rest of the tooling make sense. The lint rules, the breaking-change gate, and
the validation constraints are all governance over that product.

`buf` moved from a v1 to a v2 configuration in a way worth internalizing, because
most stale tutorials still show v1. In v1 you had a `buf.work.yaml` at the
workspace root plus a separate `buf.yaml` in each module directory. In v2 there is
a single `buf.yaml` at the repository root with a `modules:` list; the workspace
file is gone. The command names changed too: `buf mod update` became `buf dep
update`. Writing v1 layout against a v2 toolchain is a common and confusing
failure, so anchor on v2: one `buf.yaml`, a `modules:` list, `buf dep update`.

### Why buf over raw protoc

`protoc` compiles `.proto` files into generated code and nothing else. It has no
notion of linting, no breaking-change detection, no formatter, no dependency
management, and no reproducible way to describe a codegen configuration; teams
end up scripting all of that by hand around `protoc` invocations. `buf` provides
each of those as a first-class, CI-friendly command over a module: `buf format`
(deterministic formatting), `buf lint` (style enforcement), `buf build` (compile
the module to a single self-contained image), `buf breaking` (compatibility
checking against a previous version), and `buf generate` (codegen driven by a
declarative `buf.gen.yaml`). The point is not that `protoc` is wrong; it is that
`buf` is the layer of governance the schema-as-product model requires.

### The lint strictness ladder

`buf lint` groups rules into categories that form a strictness ladder: `MINIMAL`
is the loosest, `BASIC` adds more, and `STANDARD` — the recommended default —
adds still more. The ladder is cumulative: enabling `STANDARD` implies `BASIC`
and `MINIMAL`. Two further categories are orthogonal to the ladder and off by
default: `COMMENTS` (require doc comments on every declaration) and `UNARY_RPC`
(forbid streaming RPCs). What matters is understanding what `STANDARD` adds beyond
plain naming conventions, because those additions encode hard-won API-design
lessons:

- `PACKAGE_VERSION_SUFFIX` forces a version suffix on the package, e.g.
  `acme.order.v1` (or `v1beta1`). Without it you have no room to ship a `v2`
  side-by-side later; the version becomes part of the type identity from day one.
- `PACKAGE_DIRECTORY_MATCH` requires the file's directory to match its package,
  so `acme/order/v1/order.proto` declares `package acme.order.v1`.
- `ENUM_ZERO_VALUE_SUFFIX` requires the enum's zero value to end in
  `_UNSPECIFIED`. In proto3 an unset enum field decodes to the zero value, so if
  the zero value is a real state you cannot distinguish "unset" from "that state"
  on the wire. `ORDER_STATUS_UNSPECIFIED = 0` reserves the zero for "no value."
- `SERVICE_SUFFIX` requires services to end in `Service` (`OrderService`).
- `RPC_REQUEST_RESPONSE_UNIQUE` requires each RPC to have its own request and
  response message types, so that adding a field to one RPC never silently
  changes another. Reusing one message across two RPCs couples their evolution.

These are not arbitrary style; each rule prevents a specific future mistake.

### Breaking-change scope is a spectrum, not a boolean

`buf breaking` compares the current schema against a baseline and fails on
incompatible edits. The crucial senior insight is that "breaking" is not one
thing — it is a spectrum of categories, and choosing the wrong scope either
blocks safe refactors or ships silently incompatible APIs:

- `FILE` is the strictest: it protects per-file *generated source* compatibility.
  Languages like C++ and Python generate code whose layout depends on which file
  a message lives in, so moving a message to another file breaks them even though
  the wire format is unchanged. Use `FILE` when a generated-source-sensitive
  language consumes the schema.
- `PACKAGE` protects per-package source compatibility. For a Go-only backend this
  is the pragmatic choice: Go types can move between files within a package
  without breaking importers, so `FILE` would block harmless refactors that
  `PACKAGE` correctly allows.
- `WIRE_JSON` protects binary *and* JSON compatibility. This is the true minimum
  for a Connect, grpc-gateway, or gRPC-JSON API, because those serve JSON where
  field *names* (not just tag numbers) are part of the contract.
- `WIRE` protects only the binary wire format. Use it only when you own every
  client and none of them speak JSON; it will let a JSON-breaking rename through.

Picking too loose a category is the dangerous error: `WIRE` on a public JSON API
lets a field rename ship that breaks every JSON client while the check stays
green. Picking too strict a category is merely annoying (it blocks safe moves).

### The wire-compatibility invariants

Protobuf's compatibility rules reduce to a small set of invariants that the
breaking gate enforces mechanically, and that every engineer touching a schema
must hold in their head:

- Tag numbers are the contract, not names. The wire encoding identifies a field
  by its number; the name exists only for source and JSON.
- Never reuse a retired tag number. If field 5 was deleted and a later edit binds
  5 to a different type, every serialized payload that still carries a 5 will be
  silently mis-decoded. This is the single most destructive schema mistake.
- Deleting a field requires *reserving* both its number and its name
  (`reserved 5; reserved "note";`), which makes the compiler reject any future
  attempt to re-introduce either. Reserving is how you delete safely.
- Changing a field's type is a wire break (a `uint32` and a `string` do not share
  an encoding), so the gate rejects it.
- Renaming a field is source- and JSON-breaking even when the binary wire is
  unaffected, because JSON keys derive from the field name.

### Managed mode centralizes file options

Every `.proto` normally carries language-specific file options such as
`go_package`, which tells `protoc-gen-go` the import path of the generated
package. Hand-writing `go_package` in every file is brittle: the schema (owned by
the API team) ends up encoding a consumer's import-path convention, and a repo
move means editing dozens of files. Managed mode moves these options out of the
`.proto` files and into `buf.gen.yaml`: you set a single `go_package_prefix`
override and `buf` computes each file's `go_package` from it at generation time.
The schema stays free of consumer concerns and import paths stay consistent.
Pair it with `clean: true`, which wipes the output directory before generating so
that a renamed or deleted message leaves no orphaned `.pb.go` behind — stale
generated files still compile and can mask a deletion, so deterministic codegen
means starting from an empty output tree every time.

### Plugins: local and remote

Codegen is pluggable. A *local* plugin is a binary on your `PATH` — `protoc-gen-go`
(base messages), `protoc-gen-go-grpc` (gRPC stubs), `protoc-gen-connect-go`
(Connect stubs). A *remote* plugin is hosted on the Buf Schema Registry and
pinned by version, which makes generation hermetic (no local toolchain to install
or drift). In `buf.gen.yaml` each plugin declares its `out` directory and `opt`
flags; `paths=source_relative` is the common option that lays generated files out
mirroring the proto directory structure rather than under the full package path.

### Two layers of contract enforcement

There are two distinct, complementary layers of enforcement, and confusing them
is a classic error:

- `buf lint` and `buf breaking` are *static*, at-merge-time checks over the
  schema. They guarantee the schema is well-styled and that this revision does
  not break the previous one. They say nothing about the data flowing at runtime.
- `protovalidate` is *runtime* enforcement of field- and message-level
  constraints declared in the schema. A request whose `email` field violates
  `string.email` is rejected when a server calls `Validate` on it.

A passing breaking check does not mean incoming data is valid, and — critically —
constraints are inert until a server actually calls `Validate`. The rules live in
the schema so every language sees the same contract, but they only do work when
code evaluates them.

### protovalidate replaces generated Validate() code

`protovalidate` is the successor to `protoc-gen-validate` (PGV). PGV *generated* a
`Validate()` method per message; that generated code could drift from the schema
and had to be regenerated and committed. `protovalidate` instead interprets the
constraints at runtime from the compiled descriptors — the constraint expressions
are CEL (Common Expression Language) under the hood — so there is one validator
and no generated validation code to maintain. You build the validator once at
startup with `protovalidate.New()` (which returns a `protovalidate.Validator`
interface value, safe for concurrent use) and reuse it for the life of the
process. Note the import path: the module moved from
`github.com/bufbuild/protovalidate-go` to `buf.build/go/protovalidate`; the old
path is archived.

### CI wiring and failing closed

The breaking gate belongs in CI as a required check. The idiomatic form is
`buf breaking --against '.git#branch=main'`, which builds an image of the schema
as it exists on `main` (the merge base) and compares the PR tree against it. The
gate must fail the build on any violation — "fail closed" — so an incompatible
change cannot merge by ignoring a warning. Schemas still under design can be
exempted with `ignore_unstable_packages: true`, which skips packages whose final
path component is an unstable version (`v1alpha1`, `v1beta1`, ...), while stable
`v1` packages stay locked. That lets you iterate freely pre-1.0 and lock down the
moment you cut `v1`.

## Common Mistakes

### Reusing a retired tag number

Wrong: delete field 5 and later add a new field at number 5, or delete a field
without reserving its number and name. A future edit then rebinds 5 to a
different type and every stored payload carrying a 5 is silently mis-decoded.

Fix: when you delete a field, add `reserved 5;` and `reserved "old_name";`. The
compiler will reject any attempt to re-bind that number or name, and `buf
breaking` protects the reserved range.

### Choosing the wrong breaking scope

Wrong: leaving `buf breaking` on the default `FILE` category for a Go-only
backend (which blocks safe moves), or dropping to `WIRE` for a public JSON API
(which lets a JSON-breaking field rename ship green).

Fix: `PACKAGE` for a Go-only backend, `WIRE_JSON` as the compatibility floor for
any Connect, grpc-gateway, or gRPC-JSON API. Pick the scope that matches how the
schema is actually consumed.

### Hand-editing generated code or hand-writing go_package

Wrong: editing a `.pb.go` file by hand, or writing a `go_package` option into
every `.proto`. Both cause import-path drift and merge churn, and hand edits are
erased on the next `buf generate`.

Fix: never edit generated files; use managed mode with a single
`go_package_prefix` override so import paths are computed consistently.

### Treating lint/breaking as input validation

Wrong: assuming that because the schema lints clean and the breaking check
passes, incoming requests are valid. Static schema checks say nothing about
runtime data.

Fix: declare constraints with `protovalidate` and actually call `Validate` on
every request at the server boundary. The constraints are advisory until a server
evaluates them.

### Using the v1 config layout or stale commands

Wrong: a `buf.work.yaml` plus per-directory `buf.yaml`, or `buf mod update`.

Fix: a single v2 `buf.yaml` with a `modules:` list and `buf dep update`.

### Forgetting the package version suffix

Wrong: `package acme.order;`. This fails `PACKAGE_VERSION_SUFFIX` and, worse,
leaves no room to ship a `v2` alongside `v1` later.

Fix: `package acme.order.v1;` from the first commit.

### Not making the enum zero value _UNSPECIFIED

Wrong: `enum OrderStatus { ORDER_STATUS_PENDING = 0; ... }`. An unset field is
now indistinguishable from `PENDING` on the wire.

Fix: `ORDER_STATUS_UNSPECIFIED = 0;` as the first value; real states start at 1.

### Generating into a dirty output directory

Wrong: running `buf generate` without `clean: true`. A renamed or removed message
leaves an orphaned generated file that still compiles, masking the deletion.

Fix: set `clean: true` so generation starts from an empty output tree.

### Importing the archived protovalidate path

Wrong: `import "github.com/bufbuild/protovalidate-go"`.

Fix: `import "buf.build/go/protovalidate"`; the old module is archived.

Next: [01-schema-module-and-codegen.md](01-schema-module-and-codegen.md)

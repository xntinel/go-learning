# 06 — Trending Go Topics: 50-candidate ranking (2026-06)

Research-backed candidate list of NEW / trending Go topics (2024-2026) to expand
`challenges/go/` beyond the current 47 chapters. Sources: official Go release
notes (1.23/1.24/1.25, 1.26 in-dev), go.dev/blog, pkg.go.dev, and ecosystem
trend research. Pairs with `04-expansion-runbook.md` (how to add) and
`05-quality-criteria.md` (bar).

## Scoring rubric

Composite score (max 100) = weighted sum of:

- **Relevance** (0-35): senior/staff backend leverage.
- **Novelty** (0-25): genuinely new/trending since early 2024.
- **Gap** (0-25): net-new vs the existing 47 chapters (high = not covered).
- **Teachable** (0-15): can be proven by a real offline `gate` artifact
  (vs `bar`-mode external/Linux/network deps).

`Status` column: `NEW` = net-new topic; `DEEPEN-NN` = extends existing chapter NN
(prefer deepening over duplicating, per the runbook). `Mode`: `gate` (offline,
engine-verifiable) or `bar` (external deps; gofmt-sweep + §15 only).

## Already covered (deliberately EXCLUDED from the new list — deepen, don't duplicate)

- Consensus/raft/gossip/CRDT/vector-clocks/saga/event-sourcing/CQRS/2PC/consistent-hashing/quorum -> ch37 (24 lessons).
- mTLS / TLS / QUIC / HTTP3 / gRPC streaming+interceptors / SOCKS5 / wire protocols -> ch33 (31 lessons).
- pprof CPU/heap/mutex, escape analysis, zero-alloc, sync.Pool, GOGC/ballast, false sharing -> ch26.
- range-over-func / iter / slices+maps helpers / loopvar -> ch25.
- slog structured logging -> ch21. ServeMux 1.22 routing / middleware / REST -> ch17.
- fuzzing / race detector / property-based / httptest / testcontainers-ish build-tag integration -> ch12.
- OTel instrumentation / distributed tracing / circuit breaker / retry+jitter / feature flags / graceful shutdown / health -> ch30.
- k8s client-go + controller / prometheus / OTel collector -> ch31. sqlc / migrations / tx / pool -> ch22.

## The ranked 50

| # | Topic | Cluster | Status | Mode | Score |
|---|-------|---------|--------|------|-------|
| 1 | `os.Root` directory-confined filesystem (anti path-traversal) | modern-stdlib | NEW | gate | 95 |
| 2 | `testing/synctest` deterministic concurrency (virtual time) | modern-stdlib | NEW | gate | 94 |
| 3 | `encoding/json/v2` + `jsontext` token layer | modern-stdlib | NEW | gate | 92 |
| 4 | FIPS 140-3 mode + Go Cryptographic Module (`GODEBUG=fips140`) | security-crypto | NEW | bar | 90 |
| 5 | Post-quantum hybrid TLS (`crypto/mlkem`, X25519MLKEM768) | security-crypto | NEW | gate | 89 |
| 6 | `unique` value interning (`Make`/`Handle`) | modern-stdlib | NEW | gate | 88 |
| 7 | `weak.Pointer` + `runtime.AddCleanup` (leak-free caches) | modern-stdlib | NEW | gate | 87 |
| 8 | `sync.WaitGroup.Go` + `waitgroup` vet check | modern-stdlib | NEW | gate | 86 |
| 9 | govulncheck reachability-based CVE scanning in CI | security-supplychain | NEW | bar | 85 |
| 10 | Supply chain: SLSA provenance + Sigstore/cosign + SBOM | security-supplychain | NEW | bar | 85 |
| 11 | go.mod `tool` directives + `go tool` (replaces tools.go) | build-tooling | NEW | gate | 84 |
| 12 | Container-aware GOMAXPROCS (cgroup CPU, anti CFS-throttle) | runtime-prod | NEW | gate | 84 |
| 13 | `runtime/trace.FlightRecorder` (last-N-seconds ring buffer) | observability | NEW | bar | 83 |
| 14 | ConnectRPC (curl-testable gRPC over HTTP/1.1+2) | rpc-api | NEW | bar | 83 |
| 15 | MCP servers in Go (expose tools/resources to LLMs) | ai-llm | NEW | bar | 82 |
| 16 | PASETO vs JWT (alg-confusion-free tokens) | security-crypto | NEW | gate | 82 |
| 17 | OAuth2 / OIDC flows (`golang.org/x/oauth2`) | security-crypto | NEW | bar | 81 |
| 18 | Secrets management + envelope encryption (Vault/KMS) | security-crypto | NEW | bar | 80 |
| 19 | Kafka with franz-go (transactions, EOS) | messaging | NEW | bar | 80 |
| 20 | NATS JetStream (persistence, new jetstream API) | messaging | NEW | bar | 79 |
| 21 | Temporal durable execution / saga workflows | messaging | NEW | bar | 79 |
| 22 | River: transactional Postgres job queue | messaging | NEW | bar | 78 |
| 23 | Watermill event-driven (pub/sub, event sourcing, sagas) | messaging | NEW | bar | 77 |
| 24 | pgx advanced (pool, COPY, batch, LISTEN/NOTIFY) | data-postgres | NEW | bar | 77 |
| 25 | pgvector from Go (embeddings, RAG, similarity search) | data-postgres/ai | NEW | bar | 77 |
| 26 | LLM SDK clients (Anthropic/OpenAI Go), streaming + tools | ai-llm | NEW | bar | 76 |
| 27 | wazero Wasm host runtime (no-cgo sandbox/plugins) | wasm-ext | NEW | gate | 76 |
| 28 | TinyGo / WASI guest modules | wasm-ext | NEW | bar | 73 |
| 29 | cilium/ebpf from Go (observability/net/security) | systems | NEW | bar | 73 |
| 30 | gqlgen schema-first GraphQL (dataloaders, N+1) | rpc-api | NEW | bar | 72 |
| 31 | grpc-gateway (REST/JSON from protobuf) | rpc-api | NEW | bar | 72 |
| 32 | golangci-lint v2 (config model, fmt, migrate) | build-tooling | DEEPEN-12 | bar | 71 |
| 33 | testcontainers-go (real ephemeral deps in tests) | testing | DEEPEN-12 | bar | 71 |
| 34 | failsafe-go composable resilience (policies) | resilience | DEEPEN-30 | bar | 70 |
| 35 | Continuous profiling (Grafana Pyroscope, OTLP) | observability | NEW | bar | 70 |
| 36 | Ristretto v2 cache (TinyLFU, generics) | caching | NEW | gate | 69 |
| 37 | Green Tea GC internals + tuning (1.25 exp / 1.26 default) | runtime | DEEPEN-35 | bar | 68 |
| 38 | Swiss-table map internals (1.24 rewrite) | runtime | DEEPEN-34 | gate | 67 |
| 39 | Generic type aliases (1.24) | modern-lang | DEEPEN-25 | gate | 66 |
| 40 | `errors.AsType[T]` generic unwrapping (1.26) | modern-lang | DEEPEN-10 | gate | 65 |
| 41 | `new(expr)` initialized allocation (1.26) | modern-lang | NEW | gate | 63 |
| 42 | Self-referential generic type params (1.26, CRTP) | modern-lang | DEEPEN-20 | gate | 62 |
| 43 | go.mod `ignore` directive + monorepo layout | build-tooling | NEW | gate | 61 |
| 44 | Testing attributes `T.Attr`/`B.Attr` + `T.Output` (1.25) | testing | DEEPEN-12 | gate | 60 |
| 45 | `crypto/hpke` (RFC 9180 hybrid public-key encryption, 1.26) | security-crypto | NEW | gate | 60 |
| 46 | io_uring from Go (batched async syscalls) | systems | NEW | bar | 58 |
| 47 | mmap-backed storage engine | systems | NEW | bar | 57 |
| 48 | Load shedding + backpressure under overload | resilience | DEEPEN-30 | gate | 56 |
| 49 | templ + HTMX type-safe SSR | web-frontend | NEW | bar | 52 |
| 50 | `//go:fix inline` + revamped `go fix` modernization | build-tooling | NEW | gate | 50 |

## Recommended NEW chapters (the "best" picks, clustered)

Highest net-new value, cohesive, and mostly engine-verifiable (`gate`) first:

- **Chapter 48 — Modern Go (1.24-1.26 language & stdlib)** — gate-heavy, lowest
  risk: #1,2,3,6,7,8,11,12,38,39,40,41,42,43,44,50. ~14-16 lessons.
- **Chapter 49 — Application Security, Crypto & Supply Chain** — mixed gate/bar:
  #4,5,9,10,16,17,18,45. ~9-10 lessons.
- **Chapter 50 — Messaging & Event-Driven Backends** — bar-mode (external):
  #14*,19,20,21,22,23 + app-side outbox/inbox idempotency. ~9-10 lessons.
- **Chapter 51 — RPC & API Design** — bar: #14,30,31 + versioning/buf. ~7 lessons.
- **Chapter 52 — AI/LLM Backends in Go** — bar: #15,25,26 + RAG/streaming/tools. ~8 lessons.
- **Chapter 53 — Wasm & Extensibility** — mixed: #27,28 + plugin/embedding. ~6 lessons.
- **Chapter 54 — Cloud-Native Platform: Containers, Kubernetes & Multi-Cloud** —
  bar: Docker Engine SDK, kubebuilder/controller-runtime operators, KEDA,
  Helm/GitOps, gocloud.dev portable blob/config/secrets (more-cloud), and Redis
  (distributed cache, redsync locks, rate limiting, Streams). ~10 lessons. Added
  per the user's ask for Redis + Kubernetes/Docker + more cloud coverage.
- (Observability/runtime items #13,35,37 best DEEPEN ch26/30/31/35; resilience
  #34,48 DEEPEN ch30; tooling #32,33 DEEPEN ch12.)

## Status (2026-06-26)

SCAFFOLDED: all 7 chapters (48-54, 68 lesson stubs) created with numbered H1
stubs, chained `What's Next`, per-chapter `.worker-order-NN.md` briefs, and
`go.md` index rows 584-651. ch47's final lesson now links forward to 48-01.
Content generation via the engine is PENDING (scaffold-first, per the user).

Recommendation: generate **48 + 49 first** (most net-new, most `gate`-verifiable,
least external-dependency flakiness), then 50/51/52/53/54 as a `bar`-mode wave.

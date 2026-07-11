# Go Challenges and Exercises

> 583 hands-on Go lessons across 47 sections, from tooling and syntax to systems capstones.
> The curriculum uses executable artifacts, Bloom-aligned objectives, visible guidance, and Go toolchain verification.

**Requirements**: Go 1.26+ installed (`go version`), terminal, text editor or IDE with Go support.

**Convention**: Each lesson is a self-contained Go project. Run `gofmt -w .`, `go test ./...`, and the lesson-specific verification command before considering it complete.

**Curriculum Controls**: See [`_meta/ADAPTER.md`](_meta/ADAPTER.md), [`_meta/RUBRIC.md`](_meta/RUBRIC.md), [`_meta/PREREQUISITES.md`](_meta/PREREQUISITES.md), and [`_meta/CONCEPT-REGISTRY.md`](_meta/CONCEPT-REGISTRY.md).

## 01 - Environment and Tooling

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 1 | [Go Toolchain Through a Real CLI](01-environment-and-tooling/01-your-first-go-program/00-concepts.md) | Advanced | Analyze |
| 2 | [Go Modules and Dependencies](01-environment-and-tooling/02-go-modules-and-dependencies/00-concepts.md) | Basic | Understand |
| 3 | [Go Workspace and Project Layout](01-environment-and-tooling/03-go-workspace-and-project-layout/00-concepts.md) | Basic | Understand |
| 4 | [Go Tool Commands](01-environment-and-tooling/04-go-tool-commands/00-concepts.md) | Basic | Understand |
| 5 | [Go Install and Third-Party Packages](01-environment-and-tooling/05-go-install-and-third-party-packages/00-concepts.md) | Basic | Understand |
| 6 | [Linting with golangci-lint](01-environment-and-tooling/06-linting-with-golangci-lint/00-concepts.md) | Basic | Understand |
| 7 | [Debugging with Delve](01-environment-and-tooling/07-debugging-with-delve/00-concepts.md) | Basic | Understand |
| 8 | [Cross-Compilation and Build Tags](01-environment-and-tooling/08-cross-compilation-and-build-tags/00-concepts.md) | Intermediate | Apply |

## 02 - Variables, Types, and Constants

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 9 | [Variable Declaration and Short Assignment](02-variables-types-and-constants/01-variable-declaration-and-short-assignment/00-concepts.md) | Basic | Understand |
| 10 | [Zero Values and Default Initialization](02-variables-types-and-constants/02-zero-values-and-default-initialization/00-concepts.md) | Basic | Understand |
| 11 | [Basic Types](02-variables-types-and-constants/03-basic-types/00-concepts.md) | Basic | Understand |
| 12 | [Constants and Iota](02-variables-types-and-constants/04-constants-and-iota/00-concepts.md) | Basic | Understand |
| 13 | [Type Conversions and Type Assertions](02-variables-types-and-constants/05-type-conversions-and-type-assertions/00-concepts.md) | Basic | Understand |
| 14 | [Type Aliases vs Type Definitions](02-variables-types-and-constants/06-type-aliases-vs-type-definitions/00-concepts.md) | Basic | Understand |
| 15 | [Numeric Precision and Overflow](02-variables-types-and-constants/07-numeric-precision-and-overflow/00-concepts.md) | Intermediate | Apply |
| 16 | [Untyped Constants and Constant Expressions](02-variables-types-and-constants/08-untyped-constants-and-constant-expressions/00-concepts.md) | Intermediate | Apply |
| 17 | [Blank Identifier and Shadowing](02-variables-types-and-constants/09-blank-identifier-and-shadowing/00-concepts.md) | Basic | Understand |
| 18 | [Type Inference Deep Dive](02-variables-types-and-constants/10-type-inference-deep-dive/00-concepts.md) | Intermediate | Apply |

## 03 - Control Flow

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 19 | [If/Else and Init Statements](03-control-flow/01-if-else-and-init-statements/00-concepts.md) | Basic | Understand |
| 20 | [For Loops](03-control-flow/02-for-loops/00-concepts.md) | Basic | Understand |
| 21 | [Switch Statements](03-control-flow/03-switch-statements/00-concepts.md) | Basic | Understand |
| 22 | [Type Switch](03-control-flow/04-type-switch/00-concepts.md) | Basic | Understand |
| 23 | [Range Over Collections](03-control-flow/05-range-over-collections/00-concepts.md) | Basic | Understand |
| 24 | [Labels, Break, Continue, and Goto](03-control-flow/06-labels-break-continue-goto/00-concepts.md) | Intermediate | Apply |
| 25 | [Defer Semantics and Ordering](03-control-flow/07-defer-semantics-and-ordering/00-concepts.md) | Intermediate | Apply |
| 26 | [Panic and Recover](03-control-flow/08-panic-and-recover/00-concepts.md) | Intermediate | Apply |
| 27 | [Range Over Integers and Functions](03-control-flow/09-range-over-integers-and-functions/00-concepts.md) | Intermediate | Apply |
| 28 | [Control Flow Debugging Challenge](03-control-flow/10-control-flow-debugging-challenge/00-concepts.md) | Advanced | Analyze |

## 04 - Functions

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 29 | [Function Declaration and Multiple Return Values](04-functions/01-function-declaration-and-multiple-return-values/00-concepts.md) | Basic | Understand |
| 30 | [Named Return Values](04-functions/02-named-return-values/00-concepts.md) | Basic | Understand |
| 31 | [Variadic Functions](04-functions/03-variadic-functions/00-concepts.md) | Basic | Understand |
| 32 | [First-Class Functions and Closures](04-functions/04-first-class-functions-and-closures/00-concepts.md) | Basic | Understand |
| 33 | [Anonymous Functions](04-functions/05-anonymous-functions/00-concepts.md) | Basic | Understand |
| 34 | [Function Types and Callbacks](04-functions/06-function-types-and-callbacks/00-concepts.md) | Intermediate | Apply |
| 35 | [Recursive Functions and Stack Depth](04-functions/07-recursive-functions-and-stack-depth/00-concepts.md) | Intermediate | Apply |
| 36 | [Init Functions and Package Initialization](04-functions/08-init-functions-and-package-initialization/00-concepts.md) | Intermediate | Apply |
| 37 | [Closure Gotchas - Loop Variable Capture](04-functions/09-closure-gotchas-loop-variable-capture/00-concepts.md) | Intermediate | Apply |
| 38 | [Higher-Order Functions](04-functions/10-higher-order-functions/00-concepts.md) | Intermediate | Apply |
| 39 | [Defer Stacking and Resource Cleanup](04-functions/11-defer-stacking-and-resource-cleanup/00-concepts.md) | Intermediate | Apply |
| 40 | [Functional Options Pattern](04-functions/12-functional-options-pattern/00-concepts.md) | Advanced | Analyze |

## 05 - Strings, Runes, and Unicode

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 41 | [String Basics](05-strings-runes-and-unicode/01-string-basics/00-concepts.md) | Basic | Understand |
| 42 | [Byte Slices vs Strings](05-strings-runes-and-unicode/02-byte-slices-vs-strings/00-concepts.md) | Basic | Understand |
| 43 | [Runes and Unicode Code Points](05-strings-runes-and-unicode/03-runes-and-unicode-code-points/00-concepts.md) | Basic | Understand |
| 44 | [String Iteration: Bytes vs Runes](05-strings-runes-and-unicode/04-string-iteration-bytes-vs-runes/00-concepts.md) | Basic | Understand |
| 45 | [Strings Package](05-strings-runes-and-unicode/05-strings-package/00-concepts.md) | Intermediate | Apply |
| 46 | [String Formatting with fmt](05-strings-runes-and-unicode/06-string-formatting-with-fmt/00-concepts.md) | Intermediate | Apply |
| 47 | [Regular Expressions](05-strings-runes-and-unicode/07-regular-expressions/00-concepts.md) | Intermediate | Apply |
| 48 | [Unicode Normalization and Collation](05-strings-runes-and-unicode/08-unicode-normalization-and-collation/00-concepts.md) | Advanced | Analyze |
| 49 | [Strings Builder Performance](05-strings-runes-and-unicode/09-strings-builder-performance/00-concepts.md) | Advanced | Analyze |
| 50 | [Building a Text Processing Pipeline](05-strings-runes-and-unicode/10-building-a-text-processing-pipeline/00-concepts.md) | Advanced | Analyze |

## 06 - Collections: Arrays, Slices, and Maps

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 51 | [Arrays: Fixed Size and Value Semantics](06-collections-arrays-slices-and-maps/01-arrays-fixed-size-value-semantics/00-concepts.md) | Basic | Understand |
| 52 | [Slices: Creation, Append, and Capacity](06-collections-arrays-slices-and-maps/02-slices-creation-append-capacity/00-concepts.md) | Basic | Understand |
| 53 | [Slice Expressions and Sub-Slicing](06-collections-arrays-slices-and-maps/03-slice-expressions-and-sub-slicing/00-concepts.md) | Basic | Understand |
| 54 | [Maps: Creation, Access, and Iteration](06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/00-concepts.md) | Basic | Understand |
| 55 | [Nil Slices vs Empty Slices](06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/00-concepts.md) | Basic | Understand |
| 56 | [Copy and Full Slice Expression](06-collections-arrays-slices-and-maps/06-copy-and-full-slice-expression/00-concepts.md) | Intermediate | Apply |
| 57 | [Slice Internals](06-collections-arrays-slices-and-maps/07-slice-internals/00-concepts.md) | Intermediate | Apply |
| 58 | [Map Internals and Iteration Order](06-collections-arrays-slices-and-maps/08-map-internals-and-iteration-order/00-concepts.md) | Intermediate | Apply |
| 59 | [Slices Package](06-collections-arrays-slices-and-maps/09-slices-package/00-concepts.md) | Intermediate | Apply |
| 60 | [Maps Package](06-collections-arrays-slices-and-maps/10-maps-package/00-concepts.md) | Intermediate | Apply |
| 61 | [Slice Memory Leaks](06-collections-arrays-slices-and-maps/11-slice-memory-leaks/00-concepts.md) | Advanced | Analyze |
| 62 | [Sorted Collections and Binary Search](06-collections-arrays-slices-and-maps/12-sorted-collections-binary-search/00-concepts.md) | Advanced | Analyze |
| 63 | [Implementing a Ring Buffer](06-collections-arrays-slices-and-maps/13-implementing-a-ring-buffer/00-concepts.md) | Advanced | Analyze |
| 64 | [Custom Map-Based Data Structure](06-collections-arrays-slices-and-maps/14-custom-map-based-data-structure/00-concepts.md) | Insane | Create |

## 07 - Structs and Methods

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 65 | [Struct Declaration and Initialization](07-structs-and-methods/01-struct-declaration-and-initialization/00-concepts.md) | Basic | Understand |
| 66 | [Struct Tags and JSON Encoding](07-structs-and-methods/02-struct-tags-and-json-encoding/00-concepts.md) | Basic | Understand |
| 67 | [Methods: Value vs Pointer Receivers](07-structs-and-methods/03-methods-value-vs-pointer-receivers/00-concepts.md) | Basic | Understand |
| 68 | [Anonymous Structs and Embedding](07-structs-and-methods/04-anonymous-structs-and-embedding/00-concepts.md) | Basic | Understand |
| 69 | [Struct Comparison and Equality](07-structs-and-methods/05-struct-comparison-and-equality/00-concepts.md) | Intermediate | Apply |
| 70 | [Constructor Functions and Validation](07-structs-and-methods/06-constructor-functions-and-validation/00-concepts.md) | Intermediate | Apply |
| 71 | [Method Sets and Addressability](07-structs-and-methods/07-method-sets-and-addressability/00-concepts.md) | Intermediate | Apply |
| 72 | [Embedding for Composition](07-structs-and-methods/08-embedding-for-composition/00-concepts.md) | Intermediate | Apply |
| 73 | [Struct Memory Layout and Padding](07-structs-and-methods/09-struct-memory-layout-and-padding/00-concepts.md) | Advanced | Analyze |
| 74 | [Implementing Stringer](07-structs-and-methods/10-implementing-stringer/00-concepts.md) | Intermediate | Apply |
| 75 | [Builder Pattern for Complex Structs](07-structs-and-methods/11-builder-pattern-for-complex-structs/00-concepts.md) | Advanced | Analyze |
| 76 | [Designing a Domain Model](07-structs-and-methods/12-designing-a-domain-model/00-concepts.md) | Advanced | Analyze |

## 08 - Interfaces

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 77 | [Implicit Interface Satisfaction](08-interfaces/01-implicit-interface-satisfaction/00-concepts.md) | Basic | Understand |
| 78 | [Empty Interface and any](08-interfaces/02-empty-interface-and-any/00-concepts.md) | Basic | Understand |
| 79 | [Type Assertions and Type Switches](08-interfaces/03-type-assertions-and-type-switches/00-concepts.md) | Basic | Understand |
| 80 | [Common Standard Library Interfaces](08-interfaces/04-common-standard-library-interfaces/00-concepts.md) | Basic | Understand |
| 81 | [Interface Composition and Embedding](08-interfaces/05-interface-composition-and-embedding/00-concepts.md) | Intermediate | Apply |
| 82 | [Interface Segregation](08-interfaces/06-interface-segregation/00-concepts.md) | Intermediate | Apply |
| 83 | [Nil Interface Values](08-interfaces/07-nil-interface-values/00-concepts.md) | Intermediate | Apply |
| 84 | [Accept Interfaces, Return Structs](08-interfaces/08-accept-interfaces-return-structs/00-concepts.md) | Intermediate | Apply |
| 85 | [Interface Internals](08-interfaces/09-interface-internals/00-concepts.md) | Advanced | Analyze |
| 86 | [Dependency Injection with Interfaces](08-interfaces/10-dependency-injection-with-interfaces/00-concepts.md) | Advanced | Analyze |
| 87 | [Mock Interfaces for Testing](08-interfaces/11-mock-interfaces-for-testing/00-concepts.md) | Advanced | Analyze |
| 88 | [Interface Pollution Anti-Patterns](08-interfaces/12-interface-pollution-anti-patterns/00-concepts.md) | Advanced | Analyze |
| 89 | [Designing a Plugin System with Interfaces](08-interfaces/13-designing-a-plugin-system/00-concepts.md) | Insane | Create |
| 90 | [Interface-Based Middleware Chain](08-interfaces/14-interface-based-middleware-chain/00-concepts.md) | Insane | Create |

## 09 - Pointers

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 91 | [Pointer Basics: Address and Dereference](09-pointers/01-pointer-basics/00-concepts.md) | Basic | Understand |
| 92 | [Pointers and Function Parameters](09-pointers/02-pointers-and-function-parameters/00-concepts.md) | Basic | Understand |
| 93 | [new() vs &T{}](09-pointers/03-new-vs-composite-literal/00-concepts.md) | Basic | Understand |
| 94 | [Nil Pointers and Guard Checks](09-pointers/04-nil-pointers-and-guard-checks/00-concepts.md) | Basic | Understand |
| 95 | [Pointers to Structs: Auto-Dereferencing](09-pointers/05-pointers-to-structs/00-concepts.md) | Intermediate | Apply |
| 96 | [Pointer Receivers and Interface Satisfaction](09-pointers/06-pointer-receivers-and-interfaces/00-concepts.md) | Intermediate | Apply |
| 97 | [Escape Analysis: Stack vs Heap](09-pointers/07-escape-analysis/00-concepts.md) | Advanced | Analyze |
| 98 | [Pointers in Slices and Maps](09-pointers/08-pointers-in-slices-and-maps/00-concepts.md) | Intermediate | Apply |
| 99 | [Pointer Aliasing and Data Races](09-pointers/09-pointer-aliasing-and-data-races/00-concepts.md) | Advanced | Analyze |
| 100 | [Designing Pointer-Safe APIs](09-pointers/10-designing-pointer-safe-apis/00-concepts.md) | Advanced | Analyze |

## 10 - Error Handling

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 101 | [Error Interface and Basic Patterns](10-error-handling/01-error-interface-and-basic-patterns/00-concepts.md) | Basic | Understand |
| 102 | [fmt.Errorf and Error Wrapping](10-error-handling/02-fmt-errorf-and-error-wrapping/00-concepts.md) | Basic | Understand |
| 103 | [errors.Is and errors.As](10-error-handling/03-errors-is-and-errors-as/00-concepts.md) | Basic | Understand |
| 104 | [Custom Error Types](10-error-handling/04-custom-error-types/00-concepts.md) | Basic | Understand |
| 105 | [Sentinel Errors](10-error-handling/05-sentinel-errors/00-concepts.md) | Intermediate | Apply |
| 106 | [Error Wrapping Chains](10-error-handling/06-error-wrapping-chains/00-concepts.md) | Intermediate | Apply |
| 107 | [Multiple Error Returns](10-error-handling/07-multiple-error-returns/00-concepts.md) | Intermediate | Apply |
| 108 | [Panic vs Error](10-error-handling/08-panic-vs-error/00-concepts.md) | Intermediate | Apply |
| 109 | [Error Handling in Goroutines](10-error-handling/09-error-handling-in-goroutines/00-concepts.md) | Advanced | Analyze |
| 110 | [Error Handling Middleware](10-error-handling/10-error-handling-middleware/00-concepts.md) | Advanced | Analyze |
| 111 | [Structured Error Types](10-error-handling/11-structured-error-types/00-concepts.md) | Advanced | Analyze |
| 112 | [Retry Patterns with Backoff](10-error-handling/12-retry-patterns-with-backoff/00-concepts.md) | Advanced | Analyze |
| 113 | [Designing an Error Hierarchy](10-error-handling/13-designing-an-error-hierarchy/00-concepts.md) | Insane | Create |
| 114 | [Error Observability](10-error-handling/14-error-observability/00-concepts.md) | Insane | Create |

## 11 - Packages and Modules

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 115 | [Package Declaration and Imports](11-packages-and-modules/01-package-declaration-and-imports/00-concepts.md) | Basic | Understand |
| 116 | [Exported vs Unexported](11-packages-and-modules/02-exported-vs-unexported/00-concepts.md) | Basic | Understand |
| 117 | [Internal Packages](11-packages-and-modules/03-internal-packages/00-concepts.md) | Intermediate | Apply |
| 118 | [Go Module Versioning](11-packages-and-modules/04-go-module-versioning/00-concepts.md) | Intermediate | Apply |
| 119 | [Multi-Module Workspaces](11-packages-and-modules/05-multi-module-workspaces/00-concepts.md) | Intermediate | Apply |
| 120 | [Dependency Management](11-packages-and-modules/06-dependency-management/00-concepts.md) | Intermediate | Apply |
| 121 | [Module Proxies and GOPROXY](11-packages-and-modules/07-module-proxies-and-goproxy/00-concepts.md) | Advanced | Analyze |
| 122 | [Vendor Directory](11-packages-and-modules/08-vendor-directory/00-concepts.md) | Advanced | Analyze |
| 123 | [Designing a Public Go Module](11-packages-and-modules/09-designing-a-public-go-module/00-concepts.md) | Advanced | Analyze |
| 124 | [Monorepo Module Strategy](11-packages-and-modules/10-monorepo-module-strategy/00-concepts.md) | Insane | Create |

## 12 - Testing Ecosystem

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 125 | [Your First Test](12-testing-ecosystem/01-your-first-test/00-concepts.md) | Basic | Understand |
| 126 | [Table-Driven Tests](12-testing-ecosystem/02-table-driven-tests/00-concepts.md) | Basic | Understand |
| 127 | [Test Helpers](12-testing-ecosystem/03-test-helpers/00-concepts.md) | Intermediate | Apply |
| 128 | [Subtests and t.Run](12-testing-ecosystem/04-subtests-and-t-run/00-concepts.md) | Intermediate | Apply |
| 129 | [Benchmarks](12-testing-ecosystem/05-benchmarks/00-concepts.md) | Intermediate | Apply |
| 130 | [Fuzz Testing](12-testing-ecosystem/06-fuzz-testing/00-concepts.md) | Intermediate | Apply |
| 131 | [Test Fixtures and testdata](12-testing-ecosystem/07-test-fixtures-and-testdata/00-concepts.md) | Intermediate | Apply |
| 132 | [Mocking with Interfaces](12-testing-ecosystem/08-mocking-with-interfaces/00-concepts.md) | Intermediate | Apply |
| 133 | [httptest](12-testing-ecosystem/09-httptest/00-concepts.md) | Intermediate | Apply |
| 134 | [Testing Readers with iotest](12-testing-ecosystem/10-testing-readers-with-iotest/00-concepts.md) | Intermediate | Apply |
| 135 | [Testing Filesystems with fstest](12-testing-ecosystem/11-testing-filesystems-with-fstest/00-concepts.md) | Intermediate | Apply |
| 136 | [t.Cleanup Patterns](12-testing-ecosystem/12-t-cleanup-patterns/00-concepts.md) | Intermediate | Apply |
| 137 | [Build Tags for Test Separation](12-testing-ecosystem/13-build-tags-for-test-separation/00-concepts.md) | Intermediate | Apply |
| 138 | [Parallel Tests](12-testing-ecosystem/14-parallel-tests/00-concepts.md) | Intermediate | Apply |
| 139 | [Testable Examples](12-testing-ecosystem/15-testable-examples/00-concepts.md) | Intermediate | Apply |
| 140 | [Testing Time-Dependent Code](12-testing-ecosystem/16-testing-time-dependent-code/00-concepts.md) | Intermediate | Apply |
| 141 | [Testing with Environment Variables](12-testing-ecosystem/17-testing-with-environment-variables/00-concepts.md) | Intermediate | Apply |
| 142 | [Integration Tests with Build Tags](12-testing-ecosystem/18-integration-tests-with-build-tags/00-concepts.md) | Advanced | Analyze |
| 143 | [Golden File Testing](12-testing-ecosystem/19-golden-file-testing/00-concepts.md) | Advanced | Analyze |
| 144 | [Test Coverage Analysis](12-testing-ecosystem/20-test-coverage-analysis/00-concepts.md) | Advanced | Analyze |
| 145 | [Race Detector and Concurrent Test Safety](12-testing-ecosystem/21-race-detector/00-concepts.md) | Advanced | Analyze |
| 146 | [TestMain Setup and Teardown](12-testing-ecosystem/22-testmain-setup-teardown/00-concepts.md) | Advanced | Analyze |
| 147 | [Snapshot/Approval Testing](12-testing-ecosystem/23-snapshot-approval-testing/00-concepts.md) | Advanced | Analyze |
| 148 | [Property-Based Testing with rapid](12-testing-ecosystem/24-property-based-testing/00-concepts.md) | Insane | Create |
| 149 | [Building a Test Suite for a Production Service](12-testing-ecosystem/25-building-a-test-suite/00-concepts.md) | Insane | Create |

## 13 - Goroutines and Channels

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 150 | [Your First Goroutine](13-goroutines-and-channels/01-your-first-goroutine/00-concepts.md) | Basic | Understand |
| 151 | [Channel Basics](13-goroutines-and-channels/02-channel-basics/00-concepts.md) | Basic | Understand |
| 152 | [Buffered vs Unbuffered Channels](13-goroutines-and-channels/03-buffered-vs-unbuffered-channels/00-concepts.md) | Basic | Understand |
| 153 | [Channel Direction](13-goroutines-and-channels/04-channel-direction/00-concepts.md) | Basic | Understand |
| 154 | [WaitGroup](13-goroutines-and-channels/05-waitgroup/00-concepts.md) | Basic | Understand |
| 155 | [Ranging Over Channels](13-goroutines-and-channels/06-ranging-over-channels/00-concepts.md) | Intermediate | Apply |
| 156 | [Done Channel Pattern](13-goroutines-and-channels/07-done-channel-pattern/00-concepts.md) | Intermediate | Apply |
| 157 | [Goroutine Leak Detection](13-goroutines-and-channels/08-goroutine-leak-detection/00-concepts.md) | Intermediate | Apply |
| 158 | [Channel of Channels](13-goroutines-and-channels/09-channel-of-channels/00-concepts.md) | Intermediate | Apply |
| 159 | [Signaling with Closed Channels](13-goroutines-and-channels/10-signaling-with-closed-channels/00-concepts.md) | Intermediate | Apply |
| 160 | [Goroutine Lifecycle Management](13-goroutines-and-channels/11-goroutine-lifecycle-management/00-concepts.md) | Advanced | Analyze |
| 161 | [Channel Patterns: Semaphore and Barrier](13-goroutines-and-channels/12-channel-patterns-semaphore-barrier/00-concepts.md) | Advanced | Analyze |
| 162 | [Goroutine Pools](13-goroutines-and-channels/13-goroutine-pools/00-concepts.md) | Advanced | Analyze |
| 163 | [Deadlock Detection and Prevention](13-goroutines-and-channels/14-deadlock-detection-and-prevention/00-concepts.md) | Advanced | Analyze |
| 164 | [Building a Concurrent Task Scheduler](13-goroutines-and-channels/15-building-a-concurrent-task-scheduler/00-concepts.md) | Insane | Create |
| 165 | [Goroutine Debugging Under Load](13-goroutines-and-channels/16-goroutine-debugging-under-load/00-concepts.md) | Insane | Create |

## 14 - Select and Context

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 166 | [Select Statement Basics](14-select-and-context/01-select-statement-basics/00-concepts.md) | Basic | Understand |
| 167 | [Select with Default](14-select-and-context/02-select-with-default/00-concepts.md) | Basic | Understand |
| 168 | [Timeout with Select](14-select-and-context/03-timeout-with-select/00-concepts.md) | Intermediate | Apply |
| 169 | [Context WithCancel](14-select-and-context/04-context-withcancel/00-concepts.md) | Intermediate | Apply |
| 170 | [Context WithTimeout and WithDeadline](14-select-and-context/05-context-withtimeout-withdeadline/00-concepts.md) | Intermediate | Apply |
| 171 | [Context WithValue](14-select-and-context/06-context-withvalue/00-concepts.md) | Intermediate | Apply |
| 172 | [Context Propagation](14-select-and-context/07-context-propagation/00-concepts.md) | Intermediate | Apply |
| 173 | [Select Priority and Starvation](14-select-and-context/08-select-priority-and-starvation/00-concepts.md) | Advanced | Analyze |
| 174 | [Context in HTTP Servers and Clients](14-select-and-context/09-context-in-http-servers-clients/00-concepts.md) | Advanced | Analyze |
| 175 | [Context-Aware Database Queries](14-select-and-context/10-context-aware-database-queries/00-concepts.md) | Advanced | Analyze |
| 176 | [Graceful Shutdown with Context](14-select-and-context/11-graceful-shutdown-with-context/00-concepts.md) | Advanced | Analyze |
| 177 | [Multi-Stage Pipeline Cancellation](14-select-and-context/12-multi-stage-pipeline-cancellation/00-concepts.md) | Advanced | Analyze |
| 178 | [Context Leak Detection](14-select-and-context/13-context-leak-detection/00-concepts.md) | Insane | Create |
| 179 | [Building a Context-Aware Service Framework](14-select-and-context/14-building-a-context-aware-service-framework/00-concepts.md) | Insane | Create |

## 15 - Sync Primitives

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 180 | [sync.Mutex](15-sync-primitives/01-sync-mutex/00-concepts.md) | Basic | Understand |
| 181 | [sync.RWMutex](15-sync-primitives/02-sync-rwmutex/00-concepts.md) | Intermediate | Apply |
| 182 | [sync.Once](15-sync-primitives/03-sync-once/00-concepts.md) | Intermediate | Apply |
| 183 | [sync.Map](15-sync-primitives/04-sync-map/00-concepts.md) | Intermediate | Apply |
| 184 | [sync.Pool](15-sync-primitives/05-sync-pool/00-concepts.md) | Intermediate | Apply |
| 185 | [sync.Cond](15-sync-primitives/06-sync-cond/00-concepts.md) | Advanced | Analyze |
| 186 | [atomic Package](15-sync-primitives/07-atomic-package/00-concepts.md) | Advanced | Analyze |
| 187 | [atomic.Value Config Hot Reload](15-sync-primitives/08-atomic-value-config-hot-reload/00-concepts.md) | Advanced | Analyze |
| 188 | [Lock Ordering and Deadlock Prevention](15-sync-primitives/09-lock-ordering-deadlock-prevention/00-concepts.md) | Advanced | Analyze |
| 189 | [Mutex vs Channel](15-sync-primitives/10-mutex-vs-channel/00-concepts.md) | Advanced | Analyze |
| 190 | [Lock-Free Data Structures](15-sync-primitives/11-lock-free-data-structures/00-concepts.md) | Insane | Create |
| 191 | [sync.OnceValue and OnceFunc](15-sync-primitives/12-sync-oncevalue-oncefunc/00-concepts.md) | Intermediate | Apply |
| 192 | [Building a Thread-Safe Cache](15-sync-primitives/13-building-a-thread-safe-cache/00-concepts.md) | Insane | Create |
| 193 | [Contention Profiling](15-sync-primitives/14-contention-profiling/00-concepts.md) | Insane | Create |

## 16 - Concurrency Patterns

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 194 | [Pipeline Pattern](16-concurrency-patterns/01-pipeline-pattern/01-pipeline-pattern.md) | Intermediate | Apply |
| 195 | [Fan-Out Pattern](16-concurrency-patterns/02-fan-out-pattern/02-fan-out-pattern.md) | Intermediate | Apply |
| 196 | [Fan-In Pattern](16-concurrency-patterns/03-fan-in-pattern/03-fan-in-pattern.md) | Intermediate | Apply |
| 197 | [Worker Pool Pattern](16-concurrency-patterns/04-worker-pool-pattern/04-worker-pool-pattern.md) | Intermediate | Apply |
| 198 | [Generator Pattern](16-concurrency-patterns/05-generator-pattern/05-generator-pattern.md) | Intermediate | Apply |
| 199 | [errgroup Basic Usage](16-concurrency-patterns/06-errgroup-basic-usage/06-errgroup-basic-usage.md) | Intermediate | Apply |
| 200 | [errgroup with Context](16-concurrency-patterns/07-errgroup-with-context/07-errgroup-with-context.md) | Intermediate | Apply |
| 201 | [time.Ticker Periodic Goroutines](16-concurrency-patterns/08-time-ticker-periodic-goroutines/08-time-ticker-periodic-goroutines.md) | Intermediate | Apply |
| 202 | [Or-Channel Pattern](16-concurrency-patterns/09-or-channel-pattern/09-or-channel-pattern.md) | Advanced | Analyze |
| 203 | [Or-Done Channel Pattern](16-concurrency-patterns/10-or-done-channel-pattern/10-or-done-channel-pattern.md) | Advanced | Analyze |
| 204 | [Tee Channel Pattern](16-concurrency-patterns/11-tee-channel-pattern/11-tee-channel-pattern.md) | Advanced | Analyze |
| 205 | [Bridge Channel Pattern](16-concurrency-patterns/12-bridge-channel-pattern/12-bridge-channel-pattern.md) | Advanced | Analyze |
| 206 | [Rate Limiter with Token Bucket](16-concurrency-patterns/13-rate-limiter-token-bucket/13-rate-limiter-token-bucket.md) | Advanced | Analyze |
| 207 | [Circuit Breaker Pattern](16-concurrency-patterns/14-circuit-breaker-pattern/14-circuit-breaker-pattern.md) | Advanced | Analyze |
| 208 | [Bounded Parallelism](16-concurrency-patterns/15-bounded-parallelism/15-bounded-parallelism.md) | Advanced | Analyze |
| 209 | [Pub/Sub with Channels](16-concurrency-patterns/16-pub-sub-with-channels/16-pub-sub-with-channels.md) | Advanced | Analyze |
| 210 | [Error Group Parallel Error Handling](16-concurrency-patterns/17-error-group-parallel-error-handling/17-error-group-parallel-error-handling.md) | Advanced | Analyze |
| 211 | [Bounded Worker Pool with Adaptive Sizing](16-concurrency-patterns/18-bounded-worker-pool-adaptive-sizing/18-bounded-worker-pool-adaptive-sizing.md) | Advanced | Analyze |
| 212 | [Pipeline with Per-Stage Metrics](16-concurrency-patterns/19-pipeline-with-per-stage-metrics/19-pipeline-with-per-stage-metrics.md) | Advanced | Analyze |
| 213 | [Batch Processing with Partial Failure](16-concurrency-patterns/20-batch-processing-partial-failure/20-batch-processing-partial-failure.md) | Advanced | Analyze |
| 214 | [Graceful Goroutine Draining](16-concurrency-patterns/21-graceful-goroutine-draining/21-graceful-goroutine-draining.md) | Advanced | Analyze |
| 215 | [Channel-Based State Machine](16-concurrency-patterns/22-channel-based-state-machine/22-channel-based-state-machine.md) | Advanced | Analyze |
| 216 | [Request Coalescing with Singleflight](16-concurrency-patterns/23-request-coalescing-singleflight/23-request-coalescing-singleflight.md) | Advanced | Analyze |
| 217 | [Streaming Pipeline with Backpressure](16-concurrency-patterns/24-streaming-pipeline-backpressure/24-streaming-pipeline-backpressure.md) | Insane | Create |
| 218 | [Actor Model in Go](16-concurrency-patterns/25-actor-model-in-go/25-actor-model-in-go.md) | Insane | Create |
| 219 | [CSP vs Actor Model](16-concurrency-patterns/26-csp-vs-actor/26-csp-vs-actor.md) | Insane | Create |
| 220 | [Building a Concurrent Web Crawler](16-concurrency-patterns/27-building-a-concurrent-web-crawler/27-building-a-concurrent-web-crawler.md) | Insane | Create |
| 221 | [Fan-Out with Priority Queues](16-concurrency-patterns/28-fan-out-with-priority-queues/28-fan-out-with-priority-queues.md) | Insane | Create |

## 17 - HTTP Programming

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 222 | [HTTP Server with net/http](17-http-programming/01-http-server-with-net-http/01-http-server-with-net-http.md) | Basic | Understand |
| 223 | [HTTP Client](17-http-programming/02-http-client/02-http-client.md) | Basic | Understand |
| 224 | [ServeMux Routing and Patterns](17-http-programming/03-servemux-routing-and-patterns/03-servemux-routing-and-patterns.md) | Intermediate | Apply |
| 225 | [Middleware Chains](17-http-programming/04-middleware-chains/04-middleware-chains.md) | Intermediate | Apply |
| 226 | [Request Body Parsing and Validation](17-http-programming/05-request-body-parsing-and-validation/05-request-body-parsing-and-validation.md) | Intermediate | Apply |
| 227 | [HTTP Client Timeouts](17-http-programming/06-http-client-timeouts/06-http-client-timeouts.md) | Intermediate | Apply |
| 228 | [Cookie and Session Management](17-http-programming/07-cookie-and-session-management/07-cookie-and-session-management.md) | Intermediate | Apply |
| 229 | [File Upload and Multipart Forms](17-http-programming/08-file-upload-and-multipart-forms/08-file-upload-and-multipart-forms.md) | Intermediate | Apply |
| 230 | [Server-Sent Events](17-http-programming/09-server-sent-events/09-server-sent-events.md) | Advanced | Analyze |
| 231 | [WebSocket Server](17-http-programming/10-websocket-server/10-websocket-server.md) | Advanced | Analyze |
| 232 | [HTTP/2 Server Push](17-http-programming/11-http2-server-push/11-http2-server-push.md) | Advanced | Analyze |
| 233 | [Reverse Proxy and Load Balancer](17-http-programming/12-reverse-proxy-and-load-balancer/12-reverse-proxy-and-load-balancer.md) | Advanced | Analyze |
| 234 | [Building a REST API](17-http-programming/13-building-a-rest-api/13-building-a-rest-api.md) | Insane | Create |
| 235 | [HTTP Client: Retry, Circuit Breaker, and Tracing](17-http-programming/14-http-client-retry-circuit-breaker-tracing/14-http-client-retry-circuit-breaker.md) | Insane | Create |

## 18 - Encoding: JSON, XML, and Protobuf

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 236 | [JSON Marshal and Unmarshal](18-encoding-json-xml-protobuf/01-json-marshal-unmarshal/01-json-marshal-unmarshal.md) | Basic | Understand |
| 237 | [Struct Tags for JSON](18-encoding-json-xml-protobuf/02-struct-tags-for-json/02-struct-tags-for-json.md) | Basic | Understand |
| 238 | [Custom JSON Marshaler](18-encoding-json-xml-protobuf/03-custom-json-marshaler/03-custom-json-marshaler.md) | Intermediate | Apply |
| 239 | [Streaming JSON](18-encoding-json-xml-protobuf/04-streaming-json/04-streaming-json.md) | Intermediate | Apply |
| 240 | [Handling Unknown JSON Fields](18-encoding-json-xml-protobuf/05-handling-unknown-json-fields/05-handling-unknown-json-fields.md) | Intermediate | Apply |
| 241 | [JSON Patch and Merge](18-encoding-json-xml-protobuf/06-json-patch-merge/06-json-patch-merge.md) | Intermediate | Apply |
| 242 | [XML Encoding and Decoding](18-encoding-json-xml-protobuf/07-xml-encoding-decoding/07-xml-encoding-decoding.md) | Intermediate | Apply |
| 243 | [Protocol Buffers](18-encoding-json-xml-protobuf/08-protocol-buffers/08-protocol-buffers.md) | Advanced | Analyze |
| 244 | [gRPC Service](18-encoding-json-xml-protobuf/09-grpc-service/09-grpc-service.md) | Advanced | Analyze |
| 245 | [Binary Encoding](18-encoding-json-xml-protobuf/10-binary-encoding/10-binary-encoding.md) | Advanced | Analyze |
| 246 | [Custom Encoding Format](18-encoding-json-xml-protobuf/11-custom-encoding-format/11-custom-encoding-format.md) | Insane | Create |
| 247 | [Performance -- JSON vs Protobuf vs MessagePack](18-encoding-json-xml-protobuf/12-performance-json-protobuf-msgpack/12-performance-json-protobuf-msgpack.md) | Insane | Create |

## 19 - I/O and Filesystem

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 248 | [Reading and Writing Files](19-io-and-filesystem/01-reading-and-writing-files/01-reading-and-writing-files.md) | Basic | Understand |
| 249 | [io.Reader and io.Writer Composition](19-io-and-filesystem/02-io-reader-writer-composition/02-io-reader-writer-composition.md) | Basic | Understand |
| 250 | [Buffered I/O with bufio](19-io-and-filesystem/03-buffered-io-with-bufio/03-buffered-io-with-bufio.md) | Intermediate | Apply |
| 251 | [io.Copy, TeeReader, and MultiWriter](19-io-and-filesystem/04-io-copy-teereader-multiwriter/04-io-copy-teereader-multiwriter.md) | Intermediate | Apply |
| 252 | [Walking Directory Trees](19-io-and-filesystem/05-walking-directory-trees/05-walking-directory-trees.md) | Intermediate | Apply |
| 253 | [The embed Directive](19-io-and-filesystem/06-embed-directive/06-embed-directive.md) | Intermediate | Apply |
| 254 | [Temporary Files and Directories](19-io-and-filesystem/07-temporary-files-directories/07-temporary-files-directories.md) | Intermediate | Apply |
| 255 | [CSV Reading and Writing](19-io-and-filesystem/08-csv-reading-writing/08-csv-reading-writing.md) | Intermediate | Apply |
| 256 | [YAML Parsing](19-io-and-filesystem/09-yaml-parsing/09-yaml-parsing.md) | Intermediate | Apply |
| 257 | [TOML Config Files](19-io-and-filesystem/10-toml-config-files/10-toml-config-files.md) | Intermediate | Apply |
| 258 | [stdin/stdout Piping](19-io-and-filesystem/11-stdin-stdout-piping/11-stdin-stdout-piping.md) | Intermediate | Apply |
| 259 | [Archive Formats -- tar and zip](19-io-and-filesystem/12-archive-tar-zip/12-archive-tar-zip.md) | Intermediate | Apply |
| 260 | [io/fs Virtual Filesystems](19-io-and-filesystem/13-io-fs-virtual-filesystems/13-io-fs-virtual-filesystems.md) | Advanced | Analyze |
| 261 | [Pipe-Based I/O](19-io-and-filesystem/14-pipe-based-io/14-pipe-based-io.md) | Advanced | Analyze |
| 262 | [Memory-Mapped Files](19-io-and-filesystem/15-memory-mapped-files/15-memory-mapped-files.md) | Advanced | Analyze |
| 263 | [Implementing a Custom io.Reader](19-io-and-filesystem/16-implementing-custom-io-reader/16-implementing-custom-io-reader.md) | Advanced | Analyze |
| 264 | [Structured Logging with Rotation](19-io-and-filesystem/17-structured-logging-rotation/17-structured-logging-rotation.md) | Advanced | Analyze |
| 265 | [Building a File Watcher](19-io-and-filesystem/18-building-a-file-watcher/18-building-a-file-watcher.md) | Insane | Create |

## 20 - Generics

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 266 | [Type Parameters and Constraints](20-generics/01-type-parameters-and-constraints/01-type-parameters-and-constraints.md) | Basic | Understand |
| 267 | [Generic Functions](20-generics/02-generic-functions/02-generic-functions.md) | Basic | Understand |
| 268 | [Comparable and Ordered](20-generics/03-comparable-and-ordered/03-comparable-and-ordered.md) | Intermediate | Apply |
| 269 | [Generic Data Structures](20-generics/04-generic-data-structures/04-generic-data-structures.md) | Intermediate | Apply |
| 270 | [Interface Constraints with Methods](20-generics/05-interface-constraints-with-methods/05-interface-constraints-with-methods.md) | Intermediate | Apply |
| 271 | [Union Type Constraints](20-generics/06-union-type-constraints/06-union-type-constraints.md) | Intermediate | Apply |
| 272 | [Type Inference and Constraint Inference](20-generics/07-type-inference-and-constraint-inference/07-type-inference-and-constraint-inference.md) | Intermediate | Apply |
| 273 | [Generic Tree Structures](20-generics/08-generic-tree-structures/08-generic-tree-structures.md) | Advanced | Analyze |
| 274 | [Generic Iterator Patterns](20-generics/09-generic-iterator-patterns/09-generic-iterator-patterns.md) | Advanced | Analyze |
| 275 | [Generic Repository Pattern](20-generics/10-generic-repository-pattern/10-generic-repository-pattern.md) | Advanced | Analyze |
| 276 | [Generics vs Interfaces](20-generics/11-generics-vs-interfaces/11-generics-vs-interfaces.md) | Advanced | Analyze |
| 277 | [Type Constraint Composition](20-generics/12-type-constraint-composition/12-type-constraint-composition.md) | Advanced | Analyze |
| 278 | [Generic Middleware and Decorator](20-generics/13-generic-middleware-and-decorator/13-generic-middleware-and-decorator.md) | Insane | Create |
| 279 | [Building a Type-Safe Event Bus](20-generics/14-building-a-type-safe-event-bus/14-building-a-type-safe-event-bus.md) | Insane | Create |

## 21 - Structured Logging with slog

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 280 | [Slog Basics](21-structured-logging-with-slog/01-slog-basics/01-slog-basics.md) | Basic | Understand |
| 281 | [Log Levels and Filtering](21-structured-logging-with-slog/02-log-levels-and-filtering/02-log-levels-and-filtering.md) | Basic | Understand |
| 282 | [JSON Handler vs Text Handler](21-structured-logging-with-slog/03-json-handler-vs-text-handler/03-json-handler-vs-text-handler.md) | Intermediate | Apply |
| 283 | [Groups and Nested Attributes](21-structured-logging-with-slog/04-groups-and-nested-attributes/04-groups-and-nested-attributes.md) | Intermediate | Apply |
| 284 | [Slog with Logger Enrichment](21-structured-logging-with-slog/05-slog-with-for-logger-enrichment/05-slog-with-logger-enrichment.md) | Intermediate | Apply |
| 285 | [Custom Slog Handler](21-structured-logging-with-slog/06-custom-slog-handler/06-custom-slog-handler.md) | Advanced | Analyze |
| 286 | [Context-Aware Logging](21-structured-logging-with-slog/07-context-aware-logging/07-context-aware-logging.md) | Advanced | Analyze |
| 287 | [Log Sampling for High Throughput](21-structured-logging-with-slog/08-log-sampling/08-log-sampling.md) | Advanced | Analyze |
| 288 | [Replacing Global Logger Patterns](21-structured-logging-with-slog/09-replacing-global-logger-patterns/09-replacing-global-logger-patterns.md) | Advanced | Analyze |

## 22 - Database Patterns

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 289 | [Database/SQL Basics](22-database-patterns/01-database-sql-basics/01-database-sql-basics.md) | Intermediate | Apply |
| 290 | [Row Scanning and Struct Mapping](22-database-patterns/02-row-scanning-and-struct-mapping/02-row-scanning-and-struct-mapping.md) | Intermediate | Apply |
| 291 | [Connection Pool Configuration](22-database-patterns/03-connection-pool-configuration/03-connection-pool-configuration.md) | Intermediate | Apply |
| 292 | [Prepared Statements](22-database-patterns/04-prepared-statements/04-prepared-statements.md) | Intermediate | Apply |
| 293 | [Transactions](22-database-patterns/05-transactions/05-transactions.md) | Intermediate | Apply |
| 294 | [Null Handling](22-database-patterns/06-null-handling/06-null-handling.md) | Intermediate | Apply |
| 295 | [Migration Patterns](22-database-patterns/07-migration-patterns/07-migration-patterns.md) | Advanced | Analyze |
| 296 | [sqlc Type-Safe SQL](22-database-patterns/08-sqlc-type-safe-sql/08-sqlc-type-safe-sql.md) | Advanced | Analyze |
| 297 | [Context-Aware Queries](22-database-patterns/09-context-aware-queries/09-context-aware-queries.md) | Advanced | Analyze |
| 298 | [Testing with In-Memory SQLite](22-database-patterns/10-testing-with-in-memory-sqlite/10-testing-with-in-memory-sqlite.md) | Advanced | Analyze |

## 23 - CLI Applications

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 299 | [Flag Package Basics](23-cli-applications/01-flag-package-basics/01-flag-package-basics.md) | Intermediate | Apply |
| 300 | [Custom Flag Types](23-cli-applications/02-custom-flag-types/02-custom-flag-types.md) | Intermediate | Apply |
| 301 | [Subcommands with FlagSet](23-cli-applications/03-subcommands-with-flagset/03-subcommands-with-flagset.md) | Intermediate | Apply |
| 302 | [Cobra Commands, Flags, and Args](23-cli-applications/04-cobra-commands-flags-args/04-cobra-commands-flags-args.md) | Intermediate | Apply |
| 303 | [Interactive Prompts](23-cli-applications/05-interactive-prompts/05-interactive-prompts.md) | Intermediate | Apply |
| 304 | [Progress Bars and Spinners](23-cli-applications/06-progress-bars-and-spinners/06-progress-bars-and-spinners.md) | Intermediate | Apply |
| 305 | [Output Formatting](23-cli-applications/07-output-formatting/07-output-formatting.md) | Intermediate | Apply |
| 306 | [Config Loading](23-cli-applications/08-config-loading/08-config-loading.md) | Advanced | Analyze |
| 307 | [Shell Completion Generation](23-cli-applications/09-shell-completion-generation/09-shell-completion-generation.md) | Advanced | Analyze |
| 308 | [Building a Complete CLI Tool](23-cli-applications/10-building-a-complete-cli-tool/10-building-a-complete-cli-tool.md) | Insane | Create |

## 24 - Design Patterns in Go

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 309 | [Functional Options](24-design-patterns-in-go/01-functional-options-deep-dive/00-concepts.md) | Intermediate | Apply |
| 310 | [Builder Pattern](24-design-patterns-in-go/02-builder-pattern/00-concepts.md) | Intermediate | Apply |
| 311 | [Strategy Pattern](24-design-patterns-in-go/03-strategy-pattern-via-interfaces/00-concepts.md) | Intermediate | Apply |
| 312 | [Dependency Injection](24-design-patterns-in-go/04-dependency-injection/00-concepts.md) | Intermediate | Apply |
| 313 | [Repository Pattern](24-design-patterns-in-go/05-repository-pattern/00-concepts.md) | Intermediate | Apply |
| 314 | [Service Layer Pattern](24-design-patterns-in-go/06-service-layer-pattern/00-concepts.md) | Advanced | Analyze |
| 315 | [Adapter Pattern](24-design-patterns-in-go/07-adapter-pattern/00-concepts.md) | Advanced | Analyze |
| 316 | [Middleware/Decorator Pattern](24-design-patterns-in-go/08-middleware-decorator-pattern/00-concepts.md) | Advanced | Analyze |
| 317 | [Observer Pattern with Channels](24-design-patterns-in-go/09-observer-pattern-with-channels/00-concepts.md) | Advanced | Analyze |

## 25 - Iterators and Modern Go

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 318 | [Range Over Integers](25-iterators-and-modern-go/01-range-over-integers/00-concepts.md) | Basic | Understand |
| 319 | [Loopvar Semantic Change](25-iterators-and-modern-go/02-loopvar-semantic-change/00-concepts.md) | Intermediate | Apply |
| 320 | [Range Over Func -- Push Iterators](25-iterators-and-modern-go/03-range-over-func-push-iterators/00-concepts.md) | Intermediate | Apply |
| 321 | [Range Over Func -- Pull Iterators](25-iterators-and-modern-go/04-range-over-func-pull-iterators/00-concepts.md) | Intermediate | Apply |
| 322 | [Designing Iterator APIs](25-iterators-and-modern-go/05-designing-iterator-apis/00-concepts.md) | Advanced | Analyze |
| 323 | [Composing Iterators](25-iterators-and-modern-go/06-composing-iterators/00-concepts.md) | Advanced | Analyze |
| 324 | [iter Package Usage](25-iterators-and-modern-go/07-iter-package-usage/00-concepts.md) | Advanced | Analyze |
| 325 | [Standard Library Iterators](25-iterators-and-modern-go/08-standard-library-iterators/00-concepts.md) | Intermediate | Apply |

## 26 - Memory Model and Optimization

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 326 | [Happens-Before Relationships](26-memory-model-and-optimization/01-happens-before-relationships/01-happens-before-relationships.md) | Intermediate | Apply |
| 327 | [CPU Profiling with pprof](26-memory-model-and-optimization/02-cpu-profiling-with-pprof/02-cpu-profiling-with-pprof.md) | Intermediate | Apply |
| 328 | [Memory Profiling](26-memory-model-and-optimization/03-memory-profiling/03-memory-profiling.md) | Intermediate | Apply |
| 329 | [Benchmarking Methodology](26-memory-model-and-optimization/04-benchmarking-methodology/04-benchmarking-methodology.md) | Advanced | Analyze |
| 330 | [Escape Analysis](26-memory-model-and-optimization/05-escape-analysis/05-escape-analysis.md) | Advanced | Analyze |
| 331 | [Struct Field Ordering and Cache Lines](26-memory-model-and-optimization/06-struct-field-ordering-cache-lines/06-struct-field-ordering-cache-lines.md) | Advanced | Analyze |
| 332 | [String Interning](26-memory-model-and-optimization/07-string-interning/07-string-interning.md) | Advanced | Analyze |
| 333 | [sync.Pool Tuning](26-memory-model-and-optimization/08-sync-pool-tuning/08-sync-pool-tuning.md) | Advanced | Analyze |
| 334 | [Trace Tool and Goroutine Scheduling](26-memory-model-and-optimization/09-trace-tool-goroutine-scheduling/09-trace-tool-goroutine-scheduling.md) | Advanced | Analyze |
| 335 | [Memory Ballast and GOGC Tuning](26-memory-model-and-optimization/10-memory-ballast-gogc-tuning/10-memory-ballast-gogc-tuning.md) | Advanced | Analyze |
| 336 | [False Sharing and Cache Contention](26-memory-model-and-optimization/11-false-sharing-cache-contention/11-false-sharing-cache-contention.md) | Insane | Create |
| 337 | [Zero-Allocation Patterns](26-memory-model-and-optimization/12-zero-allocation-patterns/12-zero-allocation-patterns.md) | Insane | Create |
| 338 | [Performance Regression Testing](26-memory-model-and-optimization/13-performance-regression-testing/13-performance-regression-testing.md) | Insane | Create |
| 339 | [Optimizing a Real-World Hot Path](26-memory-model-and-optimization/14-optimizing-a-real-world-hot-path/14-optimizing-a-real-world-hot-path.md) | Insane | Create |

## 27 - Reflection

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 340 | [reflect.TypeOf and reflect.ValueOf](27-reflection/01-reflect-typeof-valueof/01-reflect-typeof-valueof.md) | Intermediate | Apply |
| 341 | [Inspecting Struct Fields and Tags](27-reflection/02-inspecting-struct-fields-tags/02-inspecting-struct-fields-tags.md) | Intermediate | Apply |
| 342 | [Dynamic Method Invocation](27-reflection/03-dynamic-method-invocation/03-dynamic-method-invocation.md) | Advanced | Analyze |
| 343 | [Setting Values with Reflect](27-reflection/04-setting-values-with-reflect/04-setting-values-with-reflect.md) | Advanced | Analyze |
| 344 | [Building a Struct Validator](27-reflection/05-building-a-struct-validator/05-building-a-struct-validator.md) | Advanced | Analyze |
| 345 | [DeepEqual and Custom Comparison](27-reflection/06-deepequal-and-custom-comparison/06-deepequal-and-custom-comparison.md) | Advanced | Analyze |
| 346 | [Reflection Performance Costs](27-reflection/07-reflection-performance-costs/07-reflection-performance-costs.md) | Advanced | Analyze |
| 347 | [Building a Simple ORM](27-reflection/08-building-a-simple-orm/08-building-a-simple-orm.md) | Insane | Create |
| 348 | [Code Generation vs Reflection](27-reflection/09-code-generation-vs-reflection/09-code-generation-vs-reflection.md) | Insane | Create |
| 349 | [Building a Configuration Loader](27-reflection/10-building-a-configuration-loader/10-building-a-configuration-loader.md) | Insane | Create |

## 28 - unsafe and cgo

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 350 | [unsafe.Pointer and uintptr](28-unsafe-and-cgo/01-unsafe-pointer-and-uintptr/01-unsafe-pointer-and-uintptr.md) | Advanced | Analyze |
| 351 | [unsafe.Sizeof, Alignof, and Offsetof](28-unsafe-and-cgo/02-unsafe-sizeof-alignof-offsetof/02-unsafe-sizeof-alignof-offsetof.md) | Advanced | Analyze |
| 352 | [Type Punning](28-unsafe-and-cgo/03-type-punning/03-type-punning.md) | Advanced | Analyze |
| 353 | [cgo Basics](28-unsafe-and-cgo/04-cgo-basics/04-cgo-basics.md) | Advanced | Analyze |
| 354 | [Passing Data Between Go and C](28-unsafe-and-cgo/05-passing-data-go-and-c/05-passing-data-go-and-c.md) | Advanced | Analyze |
| 355 | [cgo Performance Overhead](28-unsafe-and-cgo/06-cgo-performance-overhead/06-cgo-performance-overhead.md) | Advanced | Analyze |
| 356 | [unsafe.Slice and unsafe.String](28-unsafe-and-cgo/07-unsafe-slice-and-string/07-unsafe-slice-and-string.md) | Advanced | Analyze |
| 357 | [Wrapping a C Library](28-unsafe-and-cgo/08-wrapping-a-c-library/08-wrapping-a-c-library.md) | Insane | Create |
| 358 | [Zero-Copy Deserialization](28-unsafe-and-cgo/09-zero-copy-deserialization/09-zero-copy-deserialization.md) | Insane | Create |
| 359 | [Memory-Mapped Data Store with unsafe](28-unsafe-and-cgo/10-memory-mapped-data-store/10-memory-mapped-data-store.md) | Insane | Create |

## 29 - Code Generation and Build System

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 360 | [Exercise 29.1: go generate Basics](29-code-generation-and-build-system/01-go-generate-basics/01-go-generate-basics.md) | Intermediate | Apply |
| 361 | [Exercise 29.2: Stringer](29-code-generation-and-build-system/02-stringer/02-stringer.md) | Intermediate | Apply |
| 362 | [Exercise 29.3: Writing a Custom Code Generator](29-code-generation-and-build-system/03-writing-a-custom-code-generator/03-writing-a-custom-code-generator.md) | Advanced | Analyze |
| 363 | [Exercise 29.4: AST Parsing](29-code-generation-and-build-system/04-ast-parsing/04-ast-parsing.md) | Advanced | Analyze |
| 364 | [Exercise 29.5: Template-Based Code Generation](29-code-generation-and-build-system/05-template-based-code-generation/05-template-based-code-generation.md) | Advanced | Analyze |
| 365 | [Exercise 29.6: Build Constraints and File Suffixes](29-code-generation-and-build-system/06-build-constraints-and-file-suffixes/06-build-constraints-and-file-suffixes.md) | Intermediate | Apply |
| 366 | [Exercise 29.7: Link-Time Variable Injection](29-code-generation-and-build-system/07-link-time-variable-injection/07-link-time-variable-injection.md) | Intermediate | Apply |
| 367 | [Exercise 29.8: Plugin System](29-code-generation-and-build-system/08-plugin-system/08-plugin-system.md) | Advanced | Analyze |
| 368 | [Exercise 29.9: Building a CLI Code Generator](29-code-generation-and-build-system/09-building-a-cli-code-generator/09-building-a-cli-code-generator.md) | Insane | Create |
| 369 | [Exercise 29.10: AST Rewriting Tool](29-code-generation-and-build-system/10-ast-rewriting-tool/10-ast-rewriting-tool.md) | Insane | Create |

## 30 - Production Patterns

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 370 | [Exercise 30.1: Graceful Shutdown](30-production-patterns/01-graceful-shutdown/01-graceful-shutdown.md) | Advanced | Analyze |
| 371 | [Exercise 30.2: Layered Configuration](30-production-patterns/02-configuration-layered/02-configuration-layered.md) | Advanced | Analyze |
| 372 | [Exercise 30.3: Feature Flags](30-production-patterns/03-feature-flags/03-feature-flags.md) | Advanced | Analyze |
| 373 | [Exercise 30.4: Health Endpoints](30-production-patterns/04-health-endpoints/04-health-endpoints.md) | Advanced | Analyze |
| 374 | [Exercise 30.5: Request ID Propagation](30-production-patterns/05-request-id-propagation/05-request-id-propagation.md) | Advanced | Analyze |
| 375 | [Exercise 30.6: Structured Error Responses](30-production-patterns/06-structured-error-responses/06-structured-error-responses.md) | Advanced | Analyze |
| 376 | [Exercise 30.7: OpenTelemetry Instrumentation](30-production-patterns/07-opentelemetry-instrumentation/07-opentelemetry-instrumentation.md) | Advanced | Analyze |
| 377 | [Exercise 30.8: Distributed Tracing Context](30-production-patterns/08-distributed-tracing-context/08-distributed-tracing-context.md) | Advanced | Analyze |
| 378 | [Exercise 30.9: Circuit Breaker with Half-Open State](30-production-patterns/09-circuit-breaker-half-open/09-circuit-breaker-half-open.md) | Advanced | Analyze |
| 379 | [Exercise 30.10: Retry with Exponential Backoff and Jitter](30-production-patterns/10-retry-exponential-backoff-jitter/10-retry-exponential-backoff-jitter.md) | Advanced | Analyze |
| 380 | [Exercise 30.11: Timeout Budgets](30-production-patterns/11-timeout-budgets/11-timeout-budgets.md) | Advanced | Analyze |
| 381 | [Exercise 30.12: Connection Pool Health Monitoring](30-production-patterns/12-connection-pool-health-monitoring/12-connection-pool-health-monitoring.md) | Advanced | Analyze |
| 382 | [Exercise 30.13: Panic Recovery in Production](30-production-patterns/13-panic-recovery-in-production/13-panic-recovery-in-production.md) | Advanced | Analyze |
| 383 | [Exercise 30.14: Blue-Green Deployment Patterns](30-production-patterns/14-blue-green-deployment-patterns/14-blue-green-deployment-patterns.md) | Advanced | Analyze |

## 31 - Cloud Native Go

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 384 | [Lambda Handler Patterns](31-cloud-native-go/01-lambda-handler-patterns/01-lambda-handler-patterns.md) | Advanced | Analyze |
| 385 | [Lambda Cold Start Optimization](31-cloud-native-go/02-lambda-cold-start-optimization/02-lambda-cold-start-optimization.md) | Advanced | Analyze |
| 386 | [SQS Message Handler](31-cloud-native-go/03-sqs-message-handler/03-sqs-message-handler.md) | Advanced | Analyze |
| 387 | [EventBridge Event Routing](31-cloud-native-go/04-eventbridge-event-routing/04-eventbridge-event-routing.md) | Advanced | Analyze |
| 388 | [S3 Event Processing](31-cloud-native-go/05-s3-event-processing/05-s3-event-processing.md) | Advanced | Analyze |
| 389 | [Kubernetes client-go](31-cloud-native-go/06-kubernetes-client-go/06-kubernetes-client-go.md) | Advanced | Analyze |
| 390 | [Kubernetes Controller](31-cloud-native-go/07-kubernetes-controller/07-kubernetes-controller.md) | Advanced | Analyze |
| 391 | [Terraform Provider Skeleton](31-cloud-native-go/08-terraform-provider-skeleton/08-terraform-provider-skeleton.md) | Insane | Create |
| 392 | [Container Health Checks](31-cloud-native-go/09-container-health-checks/09-container-health-checks.md) | Advanced | Analyze |
| 393 | [Prometheus Metrics Exposition](31-cloud-native-go/10-prometheus-metrics-exposition/10-prometheus-metrics-exposition.md) | Advanced | Analyze |
| 394 | [OpenTelemetry Collector Integration](31-cloud-native-go/11-opentelemetry-collector-integration/11-opentelemetry-collector-integration.md) | Advanced | Analyze |

## 32 - Concurrency Debugging and Testing

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 395 | [Race Condition Reproduction](32-concurrency-debugging-and-testing/01-race-condition-reproduction/01-race-condition-reproduction.md) | Advanced | Analyze |
| 396 | [Goroutine Leak Detection with goleak](32-concurrency-debugging-and-testing/02-goroutine-leak-detection-goleak/02-goroutine-leak-detection-goleak.md) | Advanced | Analyze |
| 397 | [Testing Concurrent Code](32-concurrency-debugging-and-testing/03-testing-concurrent-code/03-testing-concurrent-code.md) | Advanced | Analyze |
| 398 | [Deadlock Detection Strategies](32-concurrency-debugging-and-testing/04-deadlock-detection-strategies/04-deadlock-detection-strategies.md) | Advanced | Analyze |
| 399 | [Contention Analysis](32-concurrency-debugging-and-testing/05-contention-analysis/05-contention-analysis.md) | Advanced | Analyze |
| 400 | [Goroutine Dump Analysis](32-concurrency-debugging-and-testing/06-goroutine-dump-analysis/06-goroutine-dump-analysis.md) | Advanced | Analyze |
| 401 | [Concurrent Test Isolation](32-concurrency-debugging-and-testing/07-concurrent-test-isolation/07-concurrent-test-isolation.md) | Advanced | Analyze |
| 402 | [Chaos Testing Concurrent Code](32-concurrency-debugging-and-testing/08-chaos-testing-concurrent-code/08-chaos-testing-concurrent-code.md) | Advanced | Analyze |

## 33 - TCP, UDP, and Networking

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 403 | [TCP Server and Client](33-tcp-udp-and-networking/01-tcp-server-and-client/01-tcp-server-and-client.md) | Intermediate | Apply |
| 404 | [UDP Server and Client](33-tcp-udp-and-networking/02-udp-server-and-client/02-udp-server-and-client.md) | Intermediate | Apply |
| 405 | [Concurrent TCP Server](33-tcp-udp-and-networking/03-concurrent-tcp-server/03-concurrent-tcp-server.md) | Intermediate | Apply |
| 406 | [Connection Timeouts and Deadlines](33-tcp-udp-and-networking/04-connection-timeouts-and-deadlines/04-connection-timeouts-and-deadlines.md) | Advanced | Analyze |
| 407 | [TCP Keep-Alive](33-tcp-udp-and-networking/05-tcp-keep-alive/05-tcp-keep-alive.md) | Advanced | Analyze |
| 408 | [Building a Line-Based Protocol](33-tcp-udp-and-networking/06-building-a-line-based-protocol/06-building-a-line-based-protocol.md) | Advanced | Analyze |
| 409 | [Connection Pooling Implementation](33-tcp-udp-and-networking/07-connection-pooling-implementation/07-connection-pooling-implementation.md) | Advanced | Analyze |
| 410 | [TLS Server and Client](33-tcp-udp-and-networking/08-tls-server-and-client/08-tls-server-and-client.md) | Advanced | Analyze |
| 411 | [Mutual TLS Authentication](33-tcp-udp-and-networking/09-mutual-tls-authentication/09-mutual-tls-authentication.md) | Advanced | Analyze |
| 412 | [DNS Resolver and Custom Dialer](33-tcp-udp-and-networking/10-dns-resolver-and-custom-dialer/10-dns-resolver-and-custom-dialer.md) | Advanced | Analyze |
| 413 | [HTTP Keep-Alive Analysis](33-tcp-udp-and-networking/11-http-keep-alive-analysis/11-http-keep-alive-analysis.md) | Advanced | Analyze |
| 414 | [HTTP Client Instrumentation](33-tcp-udp-and-networking/12-http-client-instrumentation/12-http-client-instrumentation.md) | Advanced | Analyze |
| 415 | [gRPC Streaming](33-tcp-udp-and-networking/13-grpc-streaming/13-grpc-streaming.md) | Advanced | Analyze |
| 416 | [gRPC Interceptors](33-tcp-udp-and-networking/14-grpc-interceptors/14-grpc-interceptors.md) | Advanced | Analyze |
| 417 | [Custom HTTP Transport](33-tcp-udp-and-networking/15-custom-http-transport/15-custom-http-transport.md) | Advanced | Analyze |
| 418 | [Reverse Proxy with Header Manipulation](33-tcp-udp-and-networking/16-reverse-proxy-header-manipulation/16-reverse-proxy-header-manipulation.md) | Advanced | Analyze |
| 419 | [WebSocket Binary Frames](33-tcp-udp-and-networking/17-websocket-binary-frames/17-websocket-binary-frames.md) | Advanced | Analyze |
| 420 | [Connection Draining](33-tcp-udp-and-networking/18-connection-draining/18-connection-draining.md) | Advanced | Analyze |
| 421 | [Building a SOCKS5 Proxy](33-tcp-udp-and-networking/19-building-a-socks5-proxy/19-building-a-socks5-proxy.md) | Insane | Create |
| 422 | [Custom Wire Protocol](33-tcp-udp-and-networking/20-custom-wire-protocol/20-custom-wire-protocol.md) | Insane | Create |
| 423 | [TCP Load Balancer](33-tcp-udp-and-networking/21-tcp-load-balancer/21-tcp-load-balancer.md) | Insane | Create |
| 424 | [Building a Port Scanner](33-tcp-udp-and-networking/22-building-a-port-scanner/22-building-a-port-scanner.md) | Insane | Create |
| 425 | [DNS Recursive Resolver](33-tcp-udp-and-networking/23-dns-recursive-resolver/23-dns-recursive-resolver.md) | Insane | Create |
| 426 | [QUIC Transport Protocol](33-tcp-udp-and-networking/24-quic-transport-protocol/24-quic-transport-protocol.md) | Insane | Create |
| 427 | [HTTP/3 over QUIC](33-tcp-udp-and-networking/25-http3-over-quic/25-http3-over-quic.md) | Insane | Create |
| 428 | [VPN Tunnel Implementation](33-tcp-udp-and-networking/26-vpn-tunnel-implementation/26-vpn-tunnel-implementation.md) | Insane | Create |
| 429 | [NAT Traversal with STUN/TURN](33-tcp-udp-and-networking/27-nat-traversal-stun-turn/27-nat-traversal-stun-turn.md) | Insane | Create |
| 430 | [Packet Sniffer with BPF](33-tcp-udp-and-networking/28-packet-sniffer-bpf/28-packet-sniffer-bpf.md) | Insane | Create |

## 34 - Runtime Scheduler

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 431 | [GMP Model](34-runtime-scheduler/01-gmp-model/01-gmp-model.md) | Advanced | Analyze |
| 432 | [GOMAXPROCS and Processor Binding](34-runtime-scheduler/02-gomaxprocs-processor-binding/02-gomaxprocs-processor-binding.md) | Advanced | Analyze |
| 433 | [Work Stealing](34-runtime-scheduler/03-work-stealing/03-work-stealing.md) | Advanced | Analyze |
| 434 | [Cooperative vs Preemptive Scheduling](34-runtime-scheduler/04-cooperative-vs-preemptive/04-cooperative-vs-preemptive.md) | Advanced | Analyze |
| 435 | [runtime.Gosched](34-runtime-scheduler/05-runtime-gosched/05-runtime-gosched.md) | Advanced | Analyze |
| 436 | [Goroutine Stack Growth](34-runtime-scheduler/06-goroutine-stack-growth/06-goroutine-stack-growth.md) | Advanced | Analyze |
| 437 | [Observing the Scheduler with GODEBUG](34-runtime-scheduler/07-observing-scheduler-godebug/07-observing-scheduler-godebug.md) | Insane | Create |
| 438 | [Scheduler Latency Tracing](34-runtime-scheduler/08-scheduler-latency-trace/08-scheduler-latency-trace.md) | Insane | Create |
| 439 | [CPU Pinning and NUMA](34-runtime-scheduler/09-cpu-pinning-numa/09-cpu-pinning-numa.md) | Insane | Create |
| 440 | [Scheduler-Friendly Algorithms](34-runtime-scheduler/10-scheduler-friendly-algorithms/10-scheduler-friendly-algorithms.md) | Insane | Create |

## 35 - Runtime Garbage Collector

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 441 | [Tri-Color Mark and Sweep](35-runtime-garbage-collector/01-tri-color-mark-and-sweep/01-tri-color-mark-and-sweep.md) | Advanced | Analyze |
| 442 | [GC Phases](35-runtime-garbage-collector/02-gc-phases/02-gc-phases.md) | Advanced | Analyze |
| 443 | [GOGC and GOMEMLIMIT Tuning](35-runtime-garbage-collector/03-gogc-and-gomemlimit/03-gogc-and-gomemlimit.md) | Advanced | Analyze |
| 444 | [Write Barriers and GC Invariants](35-runtime-garbage-collector/04-write-barriers/04-write-barriers.md) | Advanced | Analyze |
| 445 | [Observing GC with GODEBUG](35-runtime-garbage-collector/05-observing-gc-godebug/05-observing-gc-godebug.md) | Advanced | Analyze |
| 446 | [GC Pacer and Target Heap](35-runtime-garbage-collector/06-gc-pacer/06-gc-pacer.md) | Insane | Create |
| 447 | [Soft Memory Limit](35-runtime-garbage-collector/07-soft-memory-limit/07-soft-memory-limit.md) | Insane | Create |
| 448 | [GC Impact on Tail Latency](35-runtime-garbage-collector/08-gc-impact-tail-latency/08-gc-impact-tail-latency.md) | Insane | Create |
| 449 | [Reducing GC Pressure](35-runtime-garbage-collector/09-reducing-gc-pressure/09-reducing-gc-pressure.md) | Insane | Create |
| 450 | [Arena Allocation Patterns](35-runtime-garbage-collector/10-arena-allocation-patterns/10-arena-allocation-patterns.md) | Insane | Create |

## 36 - Runtime, Compiler, and Assembly

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 451 | [Reading SSA Output](36-runtime-compiler-and-assembly/01-reading-ssa-output/01-reading-ssa-output.md) | Advanced | Analyze |
| 452 | [Compiler Optimization Passes](36-runtime-compiler-and-assembly/02-compiler-optimization-passes/02-compiler-optimization-passes.md) | Advanced | Analyze |
| 453 | [Inlining Heuristics](36-runtime-compiler-and-assembly/03-inlining-heuristics/03-inlining-heuristics.md) | Advanced | Analyze |
| 454 | [Bounds Check Elimination](36-runtime-compiler-and-assembly/04-bounds-check-elimination/04-bounds-check-elimination.md) | Advanced | Analyze |
| 455 | [PGO: Profile-Guided Optimization](36-runtime-compiler-and-assembly/05-pgo-profile-guided-optimization/05-pgo-profile-guided-optimization.md) | Advanced | Analyze |
| 456 | [Compiler Devirtualization](36-runtime-compiler-and-assembly/06-compiler-devirtualization/06-compiler-devirtualization.md) | Advanced | Analyze |
| 457 | [Dead Code Elimination](36-runtime-compiler-and-assembly/07-dead-code-elimination/07-dead-code-elimination.md) | Advanced | Analyze |
| 458 | [runtime.SetFinalizer](36-runtime-compiler-and-assembly/08-runtime-setfinalizer/08-runtime-setfinalizer.md) | Advanced | Analyze |
| 459 | [Go Assembly: Plan9 Syntax](36-runtime-compiler-and-assembly/09-go-assembly-basics/09-go-assembly-basics.md) | Insane | Create |
| 460 | [Writing Assembly Functions](36-runtime-compiler-and-assembly/10-writing-assembly-functions/10-writing-assembly-functions.md) | Insane | Create |
| 461 | [SIMD with Assembly](36-runtime-compiler-and-assembly/11-simd-with-assembly/11-simd-with-assembly.md) | Insane | Create |
| 462 | [Analyzing Compiler Output](36-runtime-compiler-and-assembly/12-analyzing-compiler-output/12-analyzing-compiler-output.md) | Insane | Create |
| 463 | [Implementing a Custom Memory Allocator](36-runtime-compiler-and-assembly/13-implementing-a-custom-memory-allocator/13-implementing-a-custom-memory-allocator.md) | Insane | Create |
| 464 | [Writing a Goroutine-Aware Profiler](36-runtime-compiler-and-assembly/14-writing-a-goroutine-aware-profiler/14-writing-a-goroutine-aware-profiler.md) | Insane | Create |
| 465 | [Implementing a Green Thread Scheduler](36-runtime-compiler-and-assembly/15-implementing-a-green-thread-scheduler/15-implementing-a-green-thread-scheduler.md) | Insane | Create |

## 37 - Distributed Systems Fundamentals

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 466 | [Consistent Hashing Ring](37-distributed-systems-fundamentals/01-consistent-hashing-ring/01-consistent-hashing-ring.md) | Advanced | Analyze |
| 467 | [Implementing a Gossip Protocol](37-distributed-systems-fundamentals/02-implementing-a-gossip-protocol/02-implementing-a-gossip-protocol.md) | Advanced | Analyze |
| 468 | [Leader Election: Bully Algorithm](37-distributed-systems-fundamentals/03-leader-election-bully-algorithm/03-leader-election-bully-algorithm.md) | Advanced | Analyze |
| 469 | [Distributed Locking with Leases](37-distributed-systems-fundamentals/04-distributed-locking/04-distributed-locking.md) | Advanced | Analyze |
| 470 | [Vector Clocks and Causality](37-distributed-systems-fundamentals/05-vector-clocks/05-vector-clocks.md) | Advanced | Analyze |
| 471 | [Raft Leader Election](37-distributed-systems-fundamentals/06-raft-leader-election/06-raft-leader-election.md) | Insane | Create |
| 472 | [Raft Log Replication](37-distributed-systems-fundamentals/07-raft-log-replication/07-raft-log-replication.md) | Insane | Create |
| 473 | [Raft Snapshots](37-distributed-systems-fundamentals/08-raft-snapshots/08-raft-snapshots.md) | Insane | Create |
| 474 | [CRDTs: Conflict-Free Replicated Data Types](37-distributed-systems-fundamentals/09-crdts/09-crdts.md) | Insane | Create |
| 475 | [Merkle Tree](37-distributed-systems-fundamentals/10-merkle-tree/10-merkle-tree.md) | Insane | Create |
| 476 | [Service Discovery](37-distributed-systems-fundamentals/11-service-discovery/11-service-discovery.md) | Insane | Create |
| 477 | [Distributed Rate Limiter](37-distributed-systems-fundamentals/12-distributed-rate-limiter/12-distributed-rate-limiter.md) | Insane | Create |
| 478 | [Sharded Key-Value Store](37-distributed-systems-fundamentals/13-sharded-key-value-store/13-sharded-key-value-store.md) | Insane | Create |
| 479 | [Chaos Testing Framework](37-distributed-systems-fundamentals/14-chaos-testing-framework/14-chaos-testing-framework.md) | Insane | Create |
| 480 | [Paxos Consensus](37-distributed-systems-fundamentals/15-paxos-consensus/15-paxos-consensus.md) | Insane | Create |
| 481 | [Two-Phase Commit](37-distributed-systems-fundamentals/16-two-phase-commit/16-two-phase-commit.md) | Insane | Create |
| 482 | [Saga Orchestrator](37-distributed-systems-fundamentals/17-saga-orchestrator/17-saga-orchestrator.md) | Insane | Create |
| 483 | [Event Sourcing Engine](37-distributed-systems-fundamentals/18-event-sourcing-engine/18-event-sourcing-engine.md) | Insane | Create |
| 484 | [CQRS and Eventual Consistency](37-distributed-systems-fundamentals/19-cqrs-eventual-consistency/19-cqrs-eventual-consistency.md) | Insane | Create |
| 485 | [Distributed Transaction Coordinator](37-distributed-systems-fundamentals/20-distributed-transaction-coordinator/20-distributed-transaction-coordinator.md) | Insane | Create |
| 486 | [Anti-Entropy Protocol](37-distributed-systems-fundamentals/21-anti-entropy-protocol/21-anti-entropy-protocol.md) | Insane | Create |
| 487 | [Failure Detector: Phi Accrual](37-distributed-systems-fundamentals/22-failure-detector-phi-accrual/22-failure-detector-phi-accrual.md) | Insane | Create |
| 488 | [Quorum-Based Replication](37-distributed-systems-fundamentals/23-quorum-based-replication/23-quorum-based-replication.md) | Insane | Create |
| 489 | [Consistent Prefix Reads](37-distributed-systems-fundamentals/24-consistent-prefix-reads/24-consistent-prefix-reads.md) | Insane | Create |

## 38 - Capstone: Container Runtime

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 490 | [Linux Namespaces: UTS and PID](38-capstone-container-runtime/01-linux-namespaces-uts-pid/01-linux-namespaces-uts-pid.md) | Insane | Create |
| 491 | [Mount Namespace and Root Filesystem](38-capstone-container-runtime/02-mount-namespace-root-filesystem/02-mount-namespace-root-filesystem.md) | Insane | Create |
| 492 | [Network Namespace and Veth Pairs](38-capstone-container-runtime/03-network-namespace-veth/03-network-namespace-veth.md) | Insane | Create |
| 493 | [Cgroups v2: CPU and Memory Limits](38-capstone-container-runtime/04-cgroups-v2-cpu-memory/04-cgroups-v2-cpu-memory.md) | Insane | Create |
| 494 | [Overlay Filesystem](38-capstone-container-runtime/05-overlay-filesystem/05-overlay-filesystem.md) | Insane | Create |
| 495 | [OCI Image Pulling](38-capstone-container-runtime/06-oci-image-pulling/06-oci-image-pulling.md) | Insane | Create |
| 496 | [Container Lifecycle Management](38-capstone-container-runtime/07-container-lifecycle/07-container-lifecycle.md) | Insane | Create |
| 497 | [Exec into Running Container](38-capstone-container-runtime/08-exec-into-running-container/08-exec-into-running-container.md) | Insane | Create |
| 498 | [Container Networking: Bridge and NAT](38-capstone-container-runtime/09-container-networking-bridge-nat/09-container-networking-bridge-nat.md) | Insane | Create |
| 499 | [Full OCI Container Runtime](38-capstone-container-runtime/10-full-oci-container-runtime/10-full-oci-container-runtime.md) | Insane | Create |

## 39 - Capstone: Database Engine

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 500 | [Write-Ahead Log (WAL)](39-capstone-database-engine/01-write-ahead-log/00-concepts.md) | Insane | Create |
| 501 | [B+Tree Index](39-capstone-database-engine/02-btree-index/00-concepts.md) | Insane | Create |
| 502 | [Buffer Pool Manager](39-capstone-database-engine/03-buffer-pool-manager/00-concepts.md) | Insane | Create |
| 503 | [SQL Lexer and Tokenizer](39-capstone-database-engine/04-sql-lexer-tokenizer/00-concepts.md) | Insane | Create |
| 504 | [SQL Parser](39-capstone-database-engine/05-sql-parser/00-concepts.md) | Insane | Create |
| 505 | [Query Planner and Executor](39-capstone-database-engine/06-query-planner/00-concepts.md) | Insane | Create |
| 506 | [Multi-Version Concurrency Control (MVCC)](39-capstone-database-engine/07-mvcc/00-concepts.md) | Insane | Create |
| 507 | [Transaction Manager](39-capstone-database-engine/08-transaction-manager/00-concepts.md) | Insane | Create |
| 508 | [Network Protocol](39-capstone-database-engine/09-network-protocol/00-concepts.md) | Insane | Create |
| 509 | [Full Embedded Database Engine](39-capstone-database-engine/10-full-embedded-database/00-concepts.md) | Insane | Create |

## 40 - Capstone: Language Interpreter

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 510 | [Lexer and Tokenizer for a Programming Language](40-capstone-language-interpreter/01-lexer-tokenizer/00-concepts.md) | Insane | Create |
| 511 | [Pratt Parser for Expression Parsing](40-capstone-language-interpreter/02-pratt-parser/00-concepts.md) | Insane | Create |
| 512 | [AST Representation and Manipulation](40-capstone-language-interpreter/03-ast-representation/00-concepts.md) | Insane | Create |
| 513 | [Tree-Walking Evaluator](40-capstone-language-interpreter/04-tree-walking-evaluator/00-concepts.md) | Insane | Create |
| 514 | [Built-in Functions and Standard Library](40-capstone-language-interpreter/05-builtin-functions/00-concepts.md) | Insane | Create |
| 515 | [Closures and First-Class Functions](40-capstone-language-interpreter/06-closures-first-class-functions/00-concepts.md) | Insane | Create |
| 516 | [REPL with Line Editing](40-capstone-language-interpreter/07-repl-line-editing/00-concepts.md) | Insane | Create |
| 517 | [Full Monkey Language Interpreter](40-capstone-language-interpreter/08-full-interpreter-monkey/00-concepts.md) | Insane | Create |

## 41 - Capstone: Message Queue

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 518 | [In-Memory Topic and Subscription System](41-capstone-message-queue/01-in-memory-topic-subscription/00-concepts.md) | Insane | Create |
| 519 | [Persistent Message Storage](41-capstone-message-queue/02-persistent-message-storage/00-concepts.md) | Insane | Create |
| 520 | [Consumer Groups and Offset Tracking](41-capstone-message-queue/03-consumer-groups-offset-tracking/00-concepts.md) | Insane | Create |
| 521 | [Producer API with Batching](41-capstone-message-queue/04-producer-api-batching/00-concepts.md) | Insane | Create |
| 522 | [Consumer API with Backpressure](41-capstone-message-queue/05-consumer-api-backpressure/00-concepts.md) | Insane | Create |
| 523 | [Message Retention and Log Compaction](41-capstone-message-queue/06-message-retention-compaction/00-concepts.md) | Insane | Create |
| 524 | [TCP Protocol and Client Library](41-capstone-message-queue/07-tcp-protocol-client/00-concepts.md) | Insane | Create |
| 525 | [Full Message Queue System](41-capstone-message-queue/08-full-message-queue/00-concepts.md) | Insane | Create |

## 42 - Capstone: Service Mesh Data Plane

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 526 | [L4 TCP Proxy](42-capstone-service-mesh-data-plane/01-l4-tcp-proxy/00-concepts.md) | Insane | Create |
| 527 | [L7 HTTP Proxy](42-capstone-service-mesh-data-plane/02-l7-http-proxy/00-concepts.md) | Insane | Create |
| 528 | [mTLS Termination](42-capstone-service-mesh-data-plane/03-mtls-termination/00-concepts.md) | Insane | Create |
| 529 | [Load Balancing](42-capstone-service-mesh-data-plane/04-load-balancing/00-concepts.md) | Insane | Create |
| 530 | [Health Checking](42-capstone-service-mesh-data-plane/05-health-checking/00-concepts.md) | Insane | Create |
| 531 | [Traffic Management](42-capstone-service-mesh-data-plane/06-traffic-management/00-concepts.md) | Insane | Create |
| 532 | [Rate Limiting](42-capstone-service-mesh-data-plane/07-rate-limiting/00-concepts.md) | Insane | Create |
| 533 | [Observability Metrics](42-capstone-service-mesh-data-plane/08-observability/00-concepts.md) | Insane | Create |
| 534 | [Control Plane gRPC](42-capstone-service-mesh-data-plane/09-control-plane-grpc/00-concepts.md) | Insane | Create |
| 535 | [Full Data Plane](42-capstone-service-mesh-data-plane/10-full-data-plane/00-concepts.md) | Insane | Create |

## 43 - Capstone: Stream Processing Engine

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 536 | [Source Connectors](43-capstone-stream-processing-engine/01-source-connectors/00-concepts.md) | Insane | Create |
| 537 | [Operators: Map, Filter, FlatMap](43-capstone-stream-processing-engine/02-operators-map-filter-flatmap/00-concepts.md) | Insane | Create |
| 538 | [Windowing](43-capstone-stream-processing-engine/03-windowing/00-concepts.md) | Insane | Create |
| 539 | [Watermarks and Late Data](43-capstone-stream-processing-engine/04-watermarks-late-data/00-concepts.md) | Insane | Create |
| 540 | [Checkpointing](43-capstone-stream-processing-engine/05-checkpointing/00-concepts.md) | Insane | Create |
| 541 | [Parallel Execution](43-capstone-stream-processing-engine/06-parallel-execution/00-concepts.md) | Insane | Create |
| 542 | [Sink Connectors](43-capstone-stream-processing-engine/07-sink-connectors/00-concepts.md) | Insane | Create |
| 543 | [Full Stream Engine](43-capstone-stream-processing-engine/08-full-stream-engine/00-concepts.md) | Insane | Create |

## 44 - Capstone: HTTP/2 Implementation

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 544 | [Frame Parsing](44-capstone-http2-implementation/01-frame-parsing/00-concepts.md) | Insane | Create |
| 545 | [HPACK Header Compression](44-capstone-http2-implementation/02-hpack-header-compression/00-concepts.md) | Insane | Create |
| 546 | [Stream Multiplexing](44-capstone-http2-implementation/03-stream-multiplexing/00-concepts.md) | Insane | Create |
| 547 | [Server Push](44-capstone-http2-implementation/04-server-push/00-concepts.md) | Insane | Create |
| 548 | [Connection and Error Handling](44-capstone-http2-implementation/05-connection-error-handling/00-concepts.md) | Insane | Create |
| 549 | [Full HTTP/2 Server](44-capstone-http2-implementation/06-full-http2-server/00-concepts.md) | Insane | Create |

## 45 - Capstone: Distributed Key-Value Store

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 550 | [Partitioned Storage Engine](45-capstone-distributed-key-value-store/01-partitioned-storage/01-partitioned-storage.md) | Insane | Create |
| 551 | [Replication and Tunable Consistency](45-capstone-distributed-key-value-store/02-replication-consistency/02-replication-consistency.md) | Insane | Create |
| 552 | [Anti-Entropy with Merkle Trees](45-capstone-distributed-key-value-store/03-anti-entropy-merkle-trees/03-anti-entropy-merkle-trees.md) | Insane | Create |
| 553 | [Hinted Handoff](45-capstone-distributed-key-value-store/04-hinted-handoff/04-hinted-handoff.md) | Insane | Create |
| 554 | [Read Repair](45-capstone-distributed-key-value-store/05-read-repair/05-read-repair.md) | Insane | Create |
| 555 | [Membership Protocol](45-capstone-distributed-key-value-store/06-membership-protocol/06-membership-protocol.md) | Insane | Create |
| 556 | [Client Protocol](45-capstone-distributed-key-value-store/07-client-protocol/07-client-protocol.md) | Insane | Create |
| 557 | [Full Distributed Key-Value Store](45-capstone-distributed-key-value-store/08-full-distributed-kv/08-full-distributed-kv.md) | Insane | Create |

## 46 - Capstone: Concurrency Deep Dive

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 558 | [Lock-Free MPMC Queue](46-capstone-concurrency-deep-dive/01-lock-free-mpmc-queue/01-lock-free-mpmc-queue.md) | Insane | Create |
| 559 | [Concurrent Skip List](46-capstone-concurrency-deep-dive/02-concurrent-skip-list/02-concurrent-skip-list.md) | Insane | Create |
| 560 | [Hazard Pointer Memory Reclamation](46-capstone-concurrency-deep-dive/03-hazard-pointer-reclamation/03-hazard-pointer-reclamation.md) | Insane | Create |
| 561 | [Epoch-Based Memory Reclamation](46-capstone-concurrency-deep-dive/04-epoch-based-reclamation/04-epoch-based-reclamation.md) | Insane | Create |
| 562 | [Work-Stealing Deque](46-capstone-concurrency-deep-dive/05-work-stealing-deque/05-work-stealing-deque.md) | Insane | Create |
| 563 | [Software Transactional Memory](46-capstone-concurrency-deep-dive/06-software-transactional-memory/06-software-transactional-memory.md) | Insane | Create |
| 564 | [Concurrent B-Tree](46-capstone-concurrency-deep-dive/07-concurrent-btree/07-concurrent-btree.md) | Insane | Create |
| 565 | [Async/Await on Channels](46-capstone-concurrency-deep-dive/08-async-await-on-channels/08-async-await-on-channels.md) | Insane | Create |
| 566 | [Coroutine Library](46-capstone-concurrency-deep-dive/09-coroutine-library/09-coroutine-library.md) | Insane | Create |
| 567 | [Wait-Free Stack](46-capstone-concurrency-deep-dive/10-wait-free-stack/10-wait-free-stack.md) | Insane | Create |
| 568 | [Double Buffering for Concurrent Read/Write](46-capstone-concurrency-deep-dive/11-double-buffering/11-double-buffering.md) | Insane | Create |
| 569 | [Lock-Free Ring Buffer](46-capstone-concurrency-deep-dive/12-ring-buffer-lock-free/12-ring-buffer-lock-free.md) | Insane | Create |
| 570 | [Lock-Free Hash Map](46-capstone-concurrency-deep-dive/13-lock-free-hash-map/13-lock-free-hash-map.md) | Insane | Create |

## 47 - Capstone: Systems and Kernel

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 571 | [Direct System Calls](47-capstone-systems-and-kernel/01-direct-syscalls/01-direct-syscalls.md) | Insane | Create |
| 572 | [eBPF Tracing Tool](47-capstone-systems-and-kernel/02-ebpf-tracing/02-ebpf-tracing.md) | Insane | Create |
| 573 | [Netlink Socket Interface](47-capstone-systems-and-kernel/03-netlink-socket/03-netlink-socket.md) | Insane | Create |
| 574 | [FUSE Filesystem](47-capstone-systems-and-kernel/04-fuse-filesystem/04-fuse-filesystem.md) | Insane | Create |
| 575 | [io_uring Integration](47-capstone-systems-and-kernel/05-io-uring-integration/05-io-uring-integration.md) | Insane | Create |
| 576 | [Seccomp Filter Engine](47-capstone-systems-and-kernel/06-seccomp-filter/06-seccomp-filter.md) | Insane | Create |
| 577 | [ptrace Syscall Tracer](47-capstone-systems-and-kernel/07-ptrace-syscall-tracer/07-ptrace-syscall-tracer.md) | Insane | Create |
| 578 | [Raw Socket Packet Capture](47-capstone-systems-and-kernel/08-raw-socket-packet-capture/08-raw-socket-packet-capture.md) | Insane | Create |
| 579 | [Custom Network Protocol Stack](47-capstone-systems-and-kernel/09-custom-network-protocol-stack/09-custom-network-protocol-stack.md) | Insane | Create |
| 580 | [Go Language Server (LSP)](47-capstone-systems-and-kernel/10-go-language-server-lsp/10-go-language-server-lsp.md) | Insane | Create |
| 581 | [Dead Code Elimination Tool](47-capstone-systems-and-kernel/11-dead-code-elimination-tool/11-dead-code-elimination-tool.md) | Insane | Create |
| 582 | [Go-to-WebAssembly Compiler](47-capstone-systems-and-kernel/12-go-to-wasm-compiler/12-go-to-wasm-compiler.md) | Insane | Create |
| 583 | [Interactive Debugger with ptrace](47-capstone-systems-and-kernel/13-interactive-debugger-ptrace/13-interactive-debugger-ptrace.md) | Insane | Create |
## 48 - Modern Go (Language and Stdlib)

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 584 | [Directory-Confined Filesystem with os.Root](48-modern-go-language-and-stdlib/01-os-root-directory-sandboxing/00-concepts.md) | Advanced | Apply |
| 585 | [Deterministic Concurrency with testing/synctest](48-modern-go-language-and-stdlib/02-testing-synctest-deterministic-concurrency/00-concepts.md) | Advanced | Apply |
| 586 | [Value Interning with the unique Package](48-modern-go-language-and-stdlib/03-unique-value-interning/00-concepts.md) | Advanced | Apply |
| 587 | [Weak Pointers and runtime.AddCleanup](48-modern-go-language-and-stdlib/04-weak-pointers-and-runtime-cleanup/00-concepts.md) | Advanced | Apply |
| 588 | [Spawning Goroutines with sync.WaitGroup.Go](48-modern-go-language-and-stdlib/05-sync-waitgroup-go/00-concepts.md) | Advanced | Apply |
| 589 | [Generic Type Aliases](48-modern-go-language-and-stdlib/06-generic-type-aliases/00-concepts.md) | Advanced | Apply |
| 590 | [Tool Dependencies with go.mod tool Directives](48-modern-go-language-and-stdlib/07-go-tool-directives/00-concepts.md) | Advanced | Apply |
| 591 | [Excluding Directories with the go.mod ignore Directive](48-modern-go-language-and-stdlib/08-go-mod-ignore-and-monorepo/00-concepts.md) | Advanced | Apply |
| 592 | [Structured Test Metadata with T.Attr and T.Output](48-modern-go-language-and-stdlib/09-testing-attributes-and-output/00-concepts.md) | Advanced | Apply |
| 593 | [Container-Aware GOMAXPROCS](48-modern-go-language-and-stdlib/10-container-aware-gomaxprocs/00-concepts.md) | Advanced | Apply |
| 594 | [Swiss-Table Map Internals](48-modern-go-language-and-stdlib/11-swiss-table-map-internals/00-concepts.md) | Advanced | Apply |
| 595 | [Streaming JSON with encoding/json/v2 and jsontext](48-modern-go-language-and-stdlib/12-encoding-json-v2/00-concepts.md) | Advanced | Apply |
| 596 | [Production Tracing with runtime/trace FlightRecorder](48-modern-go-language-and-stdlib/13-flight-recorder-runtime-trace/00-concepts.md) | Advanced | Apply |
| 597 | [Generic Error Matching with errors.AsType](48-modern-go-language-and-stdlib/14-errors-astype-generic-matching/00-concepts.md) | Advanced | Apply |
| 598 | [Initialized Allocation with new(expr)](48-modern-go-language-and-stdlib/15-new-expr-initialized-allocation/00-concepts.md) | Advanced | Apply |
| 599 | [Code Modernization with go:fix and go fix](48-modern-go-language-and-stdlib/16-go-fix-inline-modernization/00-concepts.md) | Advanced | Apply |

## 49 - Application Security, Crypto and Supply Chain

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 600 | [Post-Quantum Hybrid TLS with crypto/mlkem](49-application-security-crypto-supplychain/01-post-quantum-hybrid-tls/00-concepts.md) | Advanced | Apply |
| 601 | [Authenticated Encryption with AES-GCM and ChaCha20-Poly1305](49-application-security-crypto-supplychain/02-aead-app-side-crypto/00-concepts.md) | Advanced | Apply |
| 602 | [Envelope Encryption: KEK and DEK](49-application-security-crypto-supplychain/03-envelope-encryption-kek-dek/00-concepts.md) | Advanced | Apply |
| 603 | [Password Hashing with Argon2 and bcrypt](49-application-security-crypto-supplychain/04-password-hashing-argon2/00-concepts.md) | Advanced | Apply |
| 604 | [Secure Tokens: PASETO vs JWT](49-application-security-crypto-supplychain/05-paseto-vs-jwt-tokens/00-concepts.md) | Advanced | Apply |
| 605 | [OAuth2 and OIDC Authentication Flows](49-application-security-crypto-supplychain/06-oauth2-oidc-flows/00-concepts.md) | Advanced | Apply |
| 606 | [Secrets Management with Vault](49-application-security-crypto-supplychain/07-secrets-management-vault/00-concepts.md) | Advanced | Apply |
| 607 | [FIPS 140-3 Mode and the Go Cryptographic Module](49-application-security-crypto-supplychain/08-fips-140-3-mode/00-concepts.md) | Advanced | Apply |
| 608 | [Vulnerability Scanning with govulncheck](49-application-security-crypto-supplychain/09-govulncheck-in-ci/00-concepts.md) | Advanced | Apply |
| 609 | [Supply-Chain Security: SLSA Provenance and SBOMs](49-application-security-crypto-supplychain/10-supply-chain-slsa-sbom/00-concepts.md) | Advanced | Apply |
| 610 | [Keyless Artifact Signing with Sigstore and cosign](49-application-security-crypto-supplychain/11-sigstore-cosign-signing/00-concepts.md) | Advanced | Apply |

## 50 - Messaging and Event-Driven Backends

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 611 | [Kafka Clients with franz-go](50-messaging-and-event-driven/01-kafka-with-franz-go/00-concepts.md) | Advanced | Apply |
| 612 | [Kafka Transactions and Exactly-Once Semantics](50-messaging-and-event-driven/02-kafka-exactly-once-transactions/00-concepts.md) | Advanced | Apply |
| 613 | [Durable Messaging with NATS JetStream](50-messaging-and-event-driven/03-nats-jetstream-persistence/00-concepts.md) | Advanced | Apply |
| 614 | [Redis Streams and Consumer Groups](50-messaging-and-event-driven/04-redis-streams-consumer-groups/00-concepts.md) | Advanced | Apply |
| 615 | [Event-Driven Pipelines with Watermill](50-messaging-and-event-driven/05-watermill-event-pipelines/00-concepts.md) | Advanced | Apply |
| 616 | [The Transactional Outbox Pattern](50-messaging-and-event-driven/06-transactional-outbox-pattern/00-concepts.md) | Advanced | Apply |
| 617 | [Idempotent Consumers and the Inbox Pattern](50-messaging-and-event-driven/07-idempotent-consumers-inbox/00-concepts.md) | Advanced | Apply |
| 618 | [Durable Execution with Temporal](50-messaging-and-event-driven/08-temporal-durable-execution/00-concepts.md) | Advanced | Apply |
| 619 | [Transactional Job Queues with River](50-messaging-and-event-driven/09-river-postgres-job-queue/00-concepts.md) | Advanced | Apply |
| 620 | [Dead-Letter Queues and Retry Topologies](50-messaging-and-event-driven/10-dead-letter-and-retry-topologies/00-concepts.md) | Advanced | Apply |

## 51 - RPC and API Design

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 621 | [Building Services with ConnectRPC](51-rpc-and-api-design/01-connectrpc-services/00-concepts.md) | Advanced | Apply |
| 622 | [REST/JSON Gateways with grpc-gateway](51-rpc-and-api-design/02-grpc-gateway-rest-json/00-concepts.md) | Advanced | Apply |
| 623 | [Protobuf Schemas and the buf Workflow](51-rpc-and-api-design/03-protobuf-buf-workflow/00-concepts.md) | Advanced | Apply |
| 624 | [Schema-First GraphQL with gqlgen](51-rpc-and-api-design/04-gqlgen-graphql-server/00-concepts.md) | Advanced | Apply |
| 625 | [Solving N+1 with GraphQL Dataloaders](51-rpc-and-api-design/05-graphql-dataloaders-n-plus-1/00-concepts.md) | Advanced | Apply |
| 626 | [API Versioning Strategies](51-rpc-and-api-design/06-api-versioning-strategies/00-concepts.md) | Advanced | Apply |
| 627 | [Choosing Between REST, gRPC, and Connect](51-rpc-and-api-design/07-rpc-style-tradeoffs/00-concepts.md) | Advanced | Apply |

## 52 - AI and LLM Backends in Go

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 628 | [Calling LLMs with the Anthropic and OpenAI Go SDKs](52-ai-llm-backends/01-llm-sdk-client/00-concepts.md) | Advanced | Apply |
| 629 | [Streaming Completions over SSE](52-ai-llm-backends/02-streaming-completions/00-concepts.md) | Advanced | Apply |
| 630 | [Tool and Function Calling](52-ai-llm-backends/03-tool-and-function-calling/00-concepts.md) | Advanced | Apply |
| 631 | [Embeddings and Vector Search with pgvector](52-ai-llm-backends/04-embeddings-and-pgvector/00-concepts.md) | Advanced | Apply |
| 632 | [Building a RAG Pipeline](52-ai-llm-backends/05-rag-pipeline/00-concepts.md) | Advanced | Apply |
| 633 | [Building MCP Servers in Go](52-ai-llm-backends/06-mcp-server-in-go/00-concepts.md) | Advanced | Apply |
| 634 | [Prompt Templating and Token Budgeting](52-ai-llm-backends/07-prompt-templating-token-budgeting/00-concepts.md) | Advanced | Apply |
| 635 | [LLM Resilience: Retries, Timeouts, and Caching](52-ai-llm-backends/08-llm-resilience-and-caching/00-concepts.md) | Advanced | Apply |

## 53 - WebAssembly and Extensibility

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 636 | [Embedding a Wasm Runtime with wazero](53-wasm-and-extensibility/01-wazero-host-runtime/00-concepts.md) | Advanced | Apply |
| 637 | [Host/Guest ABI and Linear Memory](53-wasm-and-extensibility/02-host-guest-abi-and-memory/00-concepts.md) | Advanced | Apply |
| 638 | [A Wasm-Based Plugin System](53-wasm-and-extensibility/03-wasm-plugin-system/00-concepts.md) | Advanced | Apply |
| 639 | [Compiling Guest Modules with TinyGo and WASI](53-wasm-and-extensibility/04-tinygo-wasi-guest-modules/00-concepts.md) | Advanced | Apply |
| 640 | [Process Plugins with hashicorp/go-plugin](53-wasm-and-extensibility/05-hashicorp-go-plugin/00-concepts.md) | Advanced | Apply |
| 641 | [Embedding a Scripting Engine](53-wasm-and-extensibility/06-embedding-a-scripting-engine/00-concepts.md) | Advanced | Apply |

## 54 - Cloud-Native Platform: Containers, Kubernetes and Multi-Cloud

| # | Lesson | Level | Bloom |
|---|--------|-------|-------|
| 642 | [Driving Docker with the Engine SDK](54-cloud-native-platform-and-orchestration/01-docker-engine-sdk/00-concepts.md) | Advanced | Apply |
| 643 | [Building a Kubernetes Operator with kubebuilder](54-cloud-native-platform-and-orchestration/02-kubebuilder-operator-crd/00-concepts.md) | Advanced | Apply |
| 644 | [Reconcile Loops with controller-runtime](54-cloud-native-platform-and-orchestration/03-controller-runtime-reconcile/00-concepts.md) | Advanced | Apply |
| 645 | [Event-Driven Autoscaling with KEDA](54-cloud-native-platform-and-orchestration/04-keda-event-driven-autoscaling/00-concepts.md) | Advanced | Apply |
| 646 | [Helm Packaging and GitOps Delivery](54-cloud-native-platform-and-orchestration/05-helm-and-gitops-patterns/00-concepts.md) | Advanced | Apply |
| 647 | [Portable Blob Storage with gocloud.dev](54-cloud-native-platform-and-orchestration/06-multi-cloud-blob-storage-gocloud/00-concepts.md) | Advanced | Apply |
| 648 | [Portable Config and Secrets with gocloud](54-cloud-native-platform-and-orchestration/07-cloud-config-and-secrets-portability/00-concepts.md) | Advanced | Apply |
| 649 | [Distributed Caching with Redis](54-cloud-native-platform-and-orchestration/08-redis-distributed-cache/00-concepts.md) | Advanced | Apply |
| 650 | [Distributed Locks with Redis and redsync](54-cloud-native-platform-and-orchestration/09-redis-distributed-locks-redsync/00-concepts.md) | Advanced | Apply |
| 651 | [Distributed Rate Limiting with Redis](54-cloud-native-platform-and-orchestration/10-redis-rate-limiting/00-concepts.md) | Advanced | Apply |

# Go Breakage Patterns — Taxonomy & Detection Guide

This reference covers every class of breaking change you're likely to encounter when
reviewing Go code diffs. For each pattern: what it is, how to detect it from a diff,
the risk level, and a safe fix.

Go's strict compiler means many breaking changes are **compile-time failures** rather
than silent runtime bugs — but some categories (interface satisfaction, value/pointer
receivers, goroutine safety) can still fail silently at runtime.

---

## Table of Contents

1. [Function & Method Signature Changes](#1-function--method-signature-changes)
2. [Interface Changes](#2-interface-changes)
3. [Struct Field Changes](#3-struct-field-changes)
4. [Export Status Changes](#4-export-status-changes)
5. [Type Definition & Alias Changes](#5-type-definition--alias-changes)
6. [Method Receiver Changes](#6-method-receiver-changes)
7. [Return Value Changes](#7-return-value-changes)
8. [Error Handling Contract Changes](#8-error-handling-contract-changes)
9. [Generic Type Parameter Changes](#9-generic-type-parameter-changes)
10. [Package & Import Path Changes](#10-package--import-path-changes)
11. [go.mod & Dependency Changes](#11-gomod--dependency-changes)
12. [Concurrency & Goroutine Safety Changes](#12-concurrency--goroutine-safety-changes)
13. [init() & Package-Level Variable Changes](#13-init--package-level-variable-changes)
14. [cgo & Build Tag Changes](#14-cgo--build-tag-changes)
15. [Reflect & Plugin / Dynamic Loading](#15-reflect--plugin--dynamic-loading)

---

## 1. Function & Method Signature Changes

### 1.1 Parameter removed
**Risk: 🔴 HIGH** (compile error)

```go
// Before
func Connect(host string, port int, timeout time.Duration) (*Conn, error)

// After
func Connect(host string, port int) (*Conn, error)  // timeout removed
```

All call sites passing `timeout` fail to compile immediately.
Fix: keep the parameter or add a `ConnectWithTimeout` variant.

---

### 1.2 New required parameter added
**Risk: 🔴 HIGH** (compile error)

```go
// After
func Connect(host string, port int, opts Options) (*Conn, error)
```

Every existing call site omitting `opts` fails to compile.
Fix: use variadic `...Option` or an options struct with zero-value defaults.

---

### 1.3 Variadic parameter added (safe)
**Risk: 🟢 LOW**

```go
// After
func Connect(host string, port int, opts ...Option) (*Conn, error)
```

Backward compatible — existing call sites pass zero opts automatically.
Caveat: code that uses `reflect` to inspect the function signature or assigns it to a
typed `func` variable will still break.

---

### 1.4 Parameter order changed
**Risk: 🔴 HIGH** (silent wrong values — no compile error)

```go
// Before: Connect(host, port)
// After:  Connect(port, host)   // swapped
```

The compiler accepts both `string, int` call sites but passes wrong values.
This is the most dangerous category — no error, wrong behaviour.

---

### 1.5 Parameter type changed (compatible underlying type)
**Risk: 🔴 HIGH** (compile error or silent truncation)

```go
// Before: func Save(id int64)
// After:  func Save(id int32)   // narrowing — data loss for large IDs
```

---

### 1.6 Named vs unnamed parameters changed
**Risk: 🟢 LOW** (names are not part of the Go calling convention)

Renaming parameters in the signature is safe at call sites but breaks callers
using `reflect.Type.In()` for name introspection.

---

## 2. Interface Changes

### 2.1 Method added to interface
**Risk: 🔴 HIGH** (compile error on all existing implementations)

```go
// Before
type Store interface {
    Get(id string) (Item, error)
    Set(id string, item Item) error
}

// After — Delete added
type Store interface {
    Get(id string) (Item, error)
    Set(id string, item Item) error
    Delete(id string) error   // NEW — every implementor must add this
}
```

Every type that satisfies `Store` now fails to compile until `Delete` is added.
This includes mock implementations in tests, which often break first.
Fix: introduce a new interface (`StoreV2`) and migrate gradually, or use embedding.

---

### 2.2 Method removed from interface
**Risk: 🟡 MEDIUM**

Existing implementations still satisfy the interface. However:
- Any caller that invokes the removed method **through the interface type** fails to compile.
- Type switches and assertions on the removed method break.

---

### 2.3 Method signature in interface changed
**Risk: 🔴 HIGH** (compile error on implementations AND callers)

Even a minor change like adding an error return breaks all implementations and callers.

---

### 2.4 Embedded interface added
**Risk: 🔴 HIGH**

```go
// After
type Handler interface {
    io.Closer        // newly embedded — all implementors must now Close()
    Handle(r *Request) Response
}
```

Every implementor must now satisfy `io.Closer` too.

---

### 2.5 Interface satisfied by value receiver, method changed to pointer receiver
**Risk: 🔴 HIGH** (silent interface satisfaction failure)

See §6 Receiver Changes. A value-type variable can no longer satisfy an interface
when the method is moved to a pointer receiver.

---

## 3. Struct Field Changes

### 3.1 Field removed
**Risk: 🔴 HIGH** (compile error on field access)

```go
// Before
type Config struct {
    Host     string
    Port     int
    Password string
}

// After — Password removed
type Config struct {
    Host string
    Port int
}
```

All `cfg.Password` accesses fail to compile. JSON/YAML unmarshalling that sets
`Password` silently ignores it (no error, just lost data).
Fix: deprecate with a comment first; keep the field for one release cycle.

---

### 3.2 Field added — keyed literals safe, positional literals break
**Risk: 🟡 MEDIUM**

```go
// After — Timeout added
type Config struct {
    Host     string
    Port     int
    Timeout  time.Duration  // new
}
```

- `Config{Host: "x", Port: 80}` — **safe**, zero value for Timeout
- `Config{"x", 80}` — **compile error**, positional literal missing field

The Go compiler rejects positional struct literals when field count changes
(only if the literal was fully positional). Find them with:

```bash
grep -rn 'Config{[^:}]' --include="*.go" .
```

---

### 3.3 Field type changed
**Risk: 🔴 HIGH** (compile error or silent data corruption)

Changing `int32` → `int64` is often safe. Changing `string` → `[]byte` breaks
all assignment and comparison sites.

---

### 3.4 Field renamed
**Risk: 🔴 HIGH** (compile error on access)

Old field name no longer exists; all access sites fail to compile.
JSON tag changes silently break serialised data (no compile error).

---

### 3.5 Field made unexported (uppercase → lowercase)
**Risk: 🔴 HIGH** for external packages; 🟡 MEDIUM within same package.

See §4 Export Status Changes.

---

### 3.6 Struct tag changed (json, db, yaml, etc.)
**Risk: 🟡 MEDIUM** (no compile error; serialisation/deserialisation breaks)

```go
// Before: `json:"user_id"`
// After:  `json:"userId"`
```

Existing serialised data (API responses, database rows) no longer maps correctly.
Clients decoding the old key get zero values silently.

---

### 3.7 `sync.Mutex` or `sync.RWMutex` field added
**Risk: 🟡 MEDIUM**

The struct can no longer be copied safely (copying a mutex is a data race).
Any `s2 := s1` or `func f(s MyStruct)` (pass by value) is now unsafe.
`go vet` will catch this with `copylocks`.

---

## 4. Export Status Changes

### 4.1 Exported → unexported
**Risk: 🔴 HIGH** (compile error in all external packages)

```go
// Before: func Validate(...)
// After:  func validate(...)   // first letter lowercased
```

External packages cannot access unexported identifiers. All callers outside the
package fail to compile.

---

### 4.2 Unexported → exported
**Risk: 🟡 MEDIUM**

No existing code breaks. However:
- The symbol is now part of the public API and carries stability obligations.
- Golint / staticcheck may flag it if it lacks documentation.
- It may shadow or conflict with exported names in embedding scenarios.

---

## 5. Type Definition & Alias Changes

### 5.1 Defined type → type alias
**Risk: 🟡 MEDIUM**

```go
// Before: type UserID int64  (defined type — not interchangeable with int64)
// After:  type UserID = int64 (alias — identical to int64)
```

Methods on `UserID` no longer work (aliases cannot have methods).
Type assertions and switches expecting `UserID` may change behaviour.

---

### 5.2 Type alias → defined type
**Risk: 🔴 HIGH**

```go
// Before: type Duration = time.Duration  (interchangeable)
// After:  type Duration time.Duration    (new type — requires explicit conversion)
```

All code that mixed `Duration` and `time.Duration` without conversion now fails
to compile.

---

### 5.3 Underlying type changed
**Risk: 🔴 HIGH**

```go
// Before: type Status int
// After:  type Status string
```

All `Status` constants (`const Active Status = 1`) become invalid. All comparisons,
marshalling, and switch statements break.

---

### 5.4 Type constraint changed (generics)
**Risk: 🔴 HIGH**

```go
// Before: type Set[T comparable] struct { ... }
// After:  type Set[T int | string] struct { ... }  // narrowed constraint
```

Callers instantiating `Set[float64]` or `Set[MyStruct]` fail to compile.

---

## 6. Method Receiver Changes

### 6.1 Value receiver → pointer receiver
**Risk: 🔴 HIGH**

```go
// Before
func (c Config) Validate() error { ... }  // value receiver — in T and *T method sets

// After
func (c *Config) Validate() error { ... } // pointer receiver — only in *T method set
```

- Variables of type `Config` (non-pointer) **no longer satisfy** interfaces requiring `Validate`.
- `var _ Validator = Config{}` — compile error.
- Silent interface satisfaction failures if the compiler doesn't catch the assignment.

---

### 6.2 Pointer receiver → value receiver
**Risk: 🟡 MEDIUM**

Generally safe at call sites. However:
- The method is now part of the value-type method set, which may cause unintended
  interface satisfaction if the method modifies state (it now operates on a copy).
- Performance may regress for large structs.

---

### 6.3 Receiver type renamed
**Risk: 🔴 HIGH**

The method no longer exists on the old type. All callers using the old type fail to compile.

---

## 7. Return Value Changes

### 7.1 Error return removed
**Risk: 🔴 HIGH** (compile error if callers use multi-assignment)

```go
// Before: result, err := f()
// After:  result := f()         // err removed
```

All `result, err := f()` call sites fail to compile.

---

### 7.2 Error return added
**Risk: 🔴 HIGH** (compile error if callers use single-assignment)

```go
// Before: result := f()
// After:  result, err := f()   // err added
```

All `result := f()` call sites fail to compile.

---

### 7.3 Return type changed (e.g. `*User` → `User`, `int` → `int64`)
**Risk: 🔴 HIGH** (compile error or silent truncation)

---

### 7.4 Named returns added or removed
**Risk: 🟢 LOW** for callers; 🟡 MEDIUM internally

Named returns change `defer` behaviour when combined with bare `return` statements —
a bare `return` returns the named values, which may differ if the function was later
modified to use named returns inconsistently.

---

## 8. Error Handling Contract Changes

### 8.1 Sentinel error renamed or removed
**Risk: 🔴 HIGH** (silent wrong behaviour — no compile error)

```go
// Before: var ErrNotFound = errors.New("not found")
// After:  var ErrItemNotFound = errors.New("item not found")
```

All `errors.Is(err, ErrNotFound)` checks silently return `false`. No compile error.

---

### 8.2 Error type changed (concrete type → wrapped)
**Risk: 🟡 MEDIUM**

```go
// Before: return &NotFoundError{ID: id}
// After:  return fmt.Errorf("item %d: %w", id, ErrNotFound)
```

Callers doing `var e *NotFoundError; errors.As(err, &e)` now get `false`.

---

### 8.3 `errors.Is` / `errors.As` chain broken
**Risk: 🟡 MEDIUM**

Wrapping changed; callers using `errors.Is` with a specific sentinel may miss errors
that are now wrapped differently.

---

### 8.4 Panic introduced where error was returned
**Risk: 🔴 HIGH**

Callers that expected an error return now get a panic instead. Cannot recover
without a `defer recover()`.

---

## 9. Generic Type Parameter Changes

### 9.1 Type constraint narrowed
**Risk: 🔴 HIGH** (compile error at instantiation sites)

Removing types from a constraint breaks all code that instantiated with those types.

---

### 9.2 Type parameter added to existing non-generic type
**Risk: 🔴 HIGH**

```go
// Before: type Cache struct { ... }
// After:  type Cache[K comparable, V any] struct { ... }
```

All `Cache{}` literal and `var c Cache` declarations now require type arguments.

---

### 9.3 Type parameter removed
**Risk: 🔴 HIGH**

All instantiation sites (`Cache[string, int]`) fail to compile.

---

## 10. Package & Import Path Changes

### 10.1 Package renamed
**Risk: 🔴 HIGH**

```go
// Before: package util
// After:  package utils
```

Import paths and all `util.Foo` references fail to compile.

---

### 10.2 File moved to different package
**Risk: 🔴 HIGH**

The symbol is no longer in its old import path. All importers must be updated.
Fix: add a compatibility shim in the old package:

```go
// old package: package auth
import "example.com/myapp/internal/auth/v2"
var Authenticate = authv2.Authenticate  // shim
```

---

### 10.3 Module path changed in go.mod
**Risk: 🔴 HIGH**

Every consumer of this module must update ALL import paths. Per semver, this
requires a new major version (`v2`, `v3`…).

---

### 10.4 Blank import removed (`import _ "pkg"`)
**Risk: 🟡 MEDIUM**

Blank imports register side effects (database drivers, codec registrations,
`init()` functions). Removing them silently removes those side effects.

---

## 11. go.mod & Dependency Changes

### 11.1 Dependency major version bumped
**Risk: 🔴 HIGH**

Major version bumps change import paths in Go modules (`v1` → `v2`). All import
statements referencing the old path must be updated.

---

### 11.2 Dependency removed
**Risk: 🔴 HIGH**

Code importing the removed package fails to compile.

---

### 11.3 Go version bumped
**Risk: 🟡 MEDIUM**

All contributors need the new toolchain. Language semantics may change:
- **1.22**: loop variable per-iteration capture (breaks code relying on sharing)
- **1.21**: `min`/`max` added to builtins (may shadow existing identifiers)
- **1.18**: generics added (new keywords may conflict with variable names)

---

### 11.4 `replace` directive added or removed
**Risk: 🟡 MEDIUM**

`replace` directives are not inherited by consuming modules. Adding one may mask
a real version issue; removing one may expose a previously hidden version conflict.

---

## 12. Concurrency & Goroutine Safety Changes

### 12.1 Mutex removed from a concurrently-accessed struct
**Risk: 🔴 HIGH** (data race — not caught by compiler; needs `-race`)

---

### 12.2 Method changed from goroutine-safe to unsafe (or vice versa)
**Risk: 🔴 HIGH** (data race — silent)

If the documentation previously guaranteed thread-safety and that guarantee is
removed, callers in concurrent contexts now have undefined behaviour.

---

### 12.3 Channel direction changed
**Risk: 🔴 HIGH** (compile error)

```go
// Before: func Produce() <-chan int   (receive-only channel)
// After:  func Produce() chan int     (bidirectional)
```

Code that assigned the result to a `<-chan int` variable now gets a compile error
(or vice versa — sending on a receive-only channel).

---

### 12.4 Channel buffering changed (buffered → unbuffered or vice versa)
**Risk: 🟡 MEDIUM** (deadlock risk, not a compile error)

Unbuffered channels synchronise sender and receiver. Buffered channels decouple
them. Changing this can introduce deadlocks or change throughput characteristics.

---

### 12.5 `sync.WaitGroup` / `sync.Once` field removed from struct
**Risk: 🟡 MEDIUM**

Code that relied on the group/once for lifecycle management no longer has that guarantee.

---

## 13. init() & Package-Level Variable Changes

### 13.1 `init()` function added
**Risk: 🟡 MEDIUM**

`init()` runs automatically on import. Adding one that performs I/O, panics on
missing config, or registers global state changes program startup behaviour.

---

### 13.2 `init()` function removed
**Risk: 🟡 MEDIUM**

Any initialisation that was performed (driver registration, flag registration,
global map population) is now skipped.

---

### 13.3 Package-level variable changed (value or type)
**Risk: 🟡 MEDIUM**

```go
// Before: var DefaultTimeout = 30 * time.Second
// After:  var DefaultTimeout = 5 * time.Second
```

Callers that read `DefaultTimeout` get the new value silently. If used as a
sentinel or default in config, behaviour changes without notice.

---

### 13.4 `iota` constant block reordered
**Risk: 🔴 HIGH** (silent wrong values — no compile error)

```go
// Before
const (
    StatusPending = iota  // 0
    StatusActive          // 1
    StatusDone            // 2
)

// After — new status inserted
const (
    StatusPending = iota  // 0
    StatusQueued          // 1 — inserted
    StatusActive          // 2 — was 1, now 2
    StatusDone            // 3 — was 2, now 3
)
```

All persisted values (database, wire format) now map to the wrong status.

---

## 14. cgo & Build Tag Changes

### 14.1 Build tag added/changed
**Risk: 🟡 MEDIUM**

```go
//go:build !windows
```

A file now excluded on a platform that previously included it, or vice versa.
CI that only tests one OS will not catch cross-platform breakage.

---

### 14.2 `import "C"` block changed
**Risk: 🔴 HIGH**

Changes to `#include`, `#cgo LDFLAGS`, or C function signatures break the cgo
binding. Requires C toolchain knowledge; flag all such changes for manual review.

---

### 14.3 File with `//go:generate` changed
**Risk: 🟡 MEDIUM**

The generator must be re-run. If CI doesn't run `go generate`, committed generated
files will be stale and inconsistent with the new generator.

---

## 15. Reflect & Plugin / Dynamic Loading

### 15.1 Struct field tag changed (used via `reflect.StructTag`)
**Risk: 🟡 MEDIUM**

ORM frameworks, JSON marshallers, and DI containers that read tags via reflect
will behave differently at runtime with no compile error.

---

### 15.2 Method set changed on a type used via reflect
**Risk: 🟡 MEDIUM**

Code using `reflect.Type.Method(i)` by index is fragile — method order is
alphabetical and changes when methods are added/removed.

---

### 15.3 Plugin (`plugin.Open`) symbol removed or renamed
**Risk: 🔴 HIGH** (runtime panic)

Go plugins load symbols by name at runtime. A removed or renamed exported symbol
causes `plugin.Lookup` to return an error (or panic if the error is ignored).

---

## Quick Risk Assignment Reference

| Change Type | Typical Risk | Compile Error? | Key Question |
|---|---|---|---|
| Param removed | 🔴 HIGH | Yes | — |
| Required param added | 🔴 HIGH | Yes | — |
| Param order changed | 🔴 HIGH | No (wrong values) | Any positional call sites? |
| Variadic param added | 🟢 LOW | No | Any reflect/func-type users? |
| Interface method added | 🔴 HIGH | Yes | How many implementors? |
| Interface method removed | 🟡 MEDIUM | Yes (callers) | Any callers invoke via interface? |
| Struct field removed | 🔴 HIGH | Yes | — |
| Struct field added | 🟡 MEDIUM | Positional literals only | Any `T{val1, val2}` call sites? |
| Struct tag changed | 🟡 MEDIUM | No | JSON/DB/YAML consumers? |
| Exported→unexported | 🔴 HIGH | Yes | — |
| Unexported→exported | 🟡 MEDIUM | No | Stability obligation added |
| Value→pointer receiver | 🔴 HIGH | Sometimes | Interface satisfaction? |
| Error return added | 🔴 HIGH | Yes | — |
| Sentinel error renamed | 🔴 HIGH | No | `errors.Is` callers? |
| iota reordered | 🔴 HIGH | No | Persisted/serialised? |
| Dependency major bump | 🔴 HIGH | Yes | Import paths updated? |
| go.mod Go version bump | 🟡 MEDIUM | No | Team toolchain ≥ new version? |
| Mutex removed | 🔴 HIGH | No | Concurrent access anywhere? |
| Body-only change | 🟢 LOW | No | Does contract hold? |

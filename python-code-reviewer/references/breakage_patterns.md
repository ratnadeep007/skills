# Python Breakage Patterns — Taxonomy & Detection Guide

This reference covers every class of breaking change you're likely to encounter when
reviewing Python code diffs. For each pattern: what it is, how to detect it from a
diff, what risk it carries, and what a safe fix looks like.

---

## Table of Contents

1. [Function & Method Signature Changes](#1-function--method-signature-changes)
2. [Return Value & Type Changes](#2-return-value--type-changes)
3. [Exception Contract Changes](#3-exception-contract-changes)
4. [Import & Module Structure Changes](#4-import--module-structure-changes)
5. [Class Hierarchy & OOP Changes](#5-class-hierarchy--oop-changes)
6. [Decorator Changes](#6-decorator-changes)
7. [Async / Sync Boundary Changes](#7-async--sync-boundary-changes)
8. [Global State & Singleton Changes](#8-global-state--singleton-changes)
9. [Type Annotation Changes (Runtime-Enforced)](#9-type-annotation-changes-runtime-enforced)
10. [Data Model Changes (ORM / Dataclasses / Pydantic)](#10-data-model-changes-orm--dataclasses--pydantic)
11. [Protocol & Duck-Typing Contract Changes](#11-protocol--duck-typing-contract-changes)
12. [Generator / Iterator Changes](#12-generator--iterator-changes)
13. [Context Manager Changes](#13-context-manager-changes)
14. [Configuration & Environment Changes](#14-configuration--environment-changes)
15. [Thread / Process Safety Changes](#15-thread--process-safety-changes)

---

## 1. Function & Method Signature Changes

### 1.1 Positional argument removed
**Risk: 🔴 HIGH**

```python
# Before
def process(data, mode, timeout):
    ...

# After
def process(data, mode):   # timeout removed
    ...
```

Detection: argument name disappears from `def` line in diff.
Impact: all callers passing `timeout` (positionally or by keyword) raise `TypeError`.
Fix: deprecate with a default first; remove after all callers are updated.

---

### 1.2 New required positional argument added
**Risk: 🔴 HIGH**

```python
# Before
def send(message):
    ...

# After
def send(message, channel):   # channel has no default
    ...
```

Detection: new arg name without `=` in `def` line.
Impact: all existing callers omitting `channel` raise `TypeError`.
Fix: add a default value or use `*` to make it keyword-only.

---

### 1.3 New optional argument (with default)
**Risk: 🟡 MEDIUM**

```python
# After
def send(message, channel="general"):
    ...
```

Safe for keyword callers. Breaks callers that:
- unpack a function's signature via `inspect.signature()`
- pass `**kwargs` and rely on the exact set of accepted keys
- use `*args` at the call site where the new arg occupies a new position

Detection: new `arg=default` at end of signature.

---

### 1.4 Argument order changed
**Risk: 🔴 HIGH**

```python
# Before
def connect(host, port, timeout):  ...
# After
def connect(host, timeout, port):  # port and timeout swapped
```

Detection: same args, different order in `def` line.
Impact: callers using positional args will silently pass wrong values (no error).
This is the most insidious breakage — no TypeError, wrong behaviour.

---

### 1.5 Positional-only argument made keyword-only (or vice versa)
**Risk: 🟡 MEDIUM**

```python
# Before
def f(x, y): ...
# After
def f(x, *, y): ...   # y is now keyword-only
```

Breaks callers like `f(1, 2)` — must become `f(1, y=2)`.

---

### 1.6 `*args` / `**kwargs` removed
**Risk: 🔴 HIGH**

Callers relying on variadic capture will silently drop arguments or raise `TypeError`.

---

### 1.7 Default value changed for existing arg
**Risk: 🟡 MEDIUM**

```python
# Before
def retry(n=3): ...
# After
def retry(n=0): ...   # default behaviour changes silently
```

Callers that rely on the default get different behaviour without any error.

---

## 2. Return Value & Type Changes

### 2.1 Function now returns `None` (previously returned a value)
**Risk: 🔴 HIGH**

```python
# Before
def get_user(id): return User.query.get(id)
# After
def get_user(id): User.query.get(id)   # forgot return — returns None
```

All callers that use the return value will get `None` and likely raise `AttributeError`
downstream. Often introduced accidentally.

---

### 2.2 Return type changed (e.g. dict → list, str → bytes)
**Risk: 🔴 HIGH**

Detection: different type constructors / return expressions in the diff.

---

### 2.3 Dictionary key set changed
**Risk: 🟡 MEDIUM**

```python
# Before: returns {"user_id": ..., "email": ...}
# After: returns {"id": ..., "email": ...}   # key renamed
```

Callers doing `result["user_id"]` get `KeyError`.

---

### 2.4 Falsy edge case change (None → [], "" → None)
**Risk: 🟡 MEDIUM**

Code using `if result:` may behave differently when `None` becomes `[]` or vice versa.

---

### 2.5 List → generator (or any non-reusable iterator)
**Risk: 🟡 MEDIUM**

Callers that consume the result twice (e.g. `len(result)` then iterate) will silently
get empty results on the second pass.

---

## 3. Exception Contract Changes

### 3.1 New exception type raised
**Risk: 🟡 MEDIUM**

```python
# Before: raises ValueError on bad input
# After: raises TypeError on bad input
```

Callers catching `ValueError` will no longer catch errors from this function.

---

### 3.2 Exception no longer raised (swallowed)
**Risk: 🟡 MEDIUM**

```python
# After
try:
    ...
except SomeError:
    pass   # silently swallowed
```

Callers relying on exceptions for control flow will break silently.

---

### 3.3 Exception now raised where it wasn't before
**Risk: 🔴 HIGH** (if callers have no try/except)

New `raise` in code paths that previously returned normally.

---

### 3.4 `finally` block change
**Risk: 🟡 MEDIUM**

Resource cleanup order changes; callers relying on cleanup (e.g. lock release,
file close) may see resource leaks or double-release.

---

## 4. Import & Module Structure Changes

### 4.1 Symbol moved to a different module
**Risk: 🔴 HIGH**

```python
# Before: from myapp.utils import helper
# After: helper moved to myapp.helpers.util
```

All `from myapp.utils import helper` statements raise `ImportError`.
Fix: add `from myapp.helpers.util import helper` back in `myapp/utils.py`.

---

### 4.2 Module renamed or deleted
**Risk: 🔴 HIGH**

Detection: `diff --git a/old_name.py b/new_name.py` with `rename` in diff header,
or only deletion with no corresponding addition.

---

### 4.3 `__all__` changed
**Risk: 🟡 MEDIUM**

`from module import *` callers will gain or lose symbols silently.

---

### 4.4 Circular import introduced
**Risk: 🔴 HIGH** (intermittent, hard to detect statically)

If A imports B and B now imports A, Python raises `ImportError` (or silently uses
a partially-initialised module). Check `import` lines in modified files.

---

### 4.5 Lazy import made eager (or vice versa)
**Risk: 🟡 MEDIUM**

Moves side effects (e.g. DB connection, config read) to a different time in startup.

---

## 5. Class Hierarchy & OOP Changes

### 5.1 Base class removed or changed
**Risk: 🔴 HIGH**

```python
# Before
class UserService(BaseService): ...
# After
class UserService:              ...  # BaseService removed
```

- Loses inherited methods / properties → `AttributeError` on callers
- `isinstance(obj, BaseService)` checks fail → type-guard logic breaks

---

### 5.2 New base class added (MRO change)
**Risk: 🟡 MEDIUM**

Method Resolution Order shifts. A method call may now resolve to a different
implementation. `super()` chains change. `isinstance()` now returns `True` for
a new type — may trigger unintended code paths.

---

### 5.3 `__init__` signature changed
**Risk: 🔴 HIGH** (same rules as §1 for functions)

All instantiation sites (`MyClass(...)`) are affected.

---

### 5.4 `__init__` calling `super().__init__()` removed
**Risk: 🔴 HIGH**

Parent class initialisation skipped — parent attributes will be missing.

---

### 5.5 `__slots__` added or changed
**Risk: 🔴 HIGH**

Adding `__slots__` prevents dynamic attribute assignment. Any code that does
`obj.new_attr = value` raises `AttributeError`.

---

### 5.6 `__eq__`, `__hash__`, `__lt__` changed
**Risk: 🟡 MEDIUM**

Objects used as dict keys or in sets may hash differently; sorting may change.
`==` comparisons in tests or conditions will behave differently.

---

### 5.7 Class variable → instance variable (or vice versa)
**Risk: 🟡 MEDIUM**

```python
# Before: MyClass.config shared across all instances
# After:  self.config per-instance — mutations no longer shared
```

---

## 6. Decorator Changes

### 6.1 `@property` added to a method
**Risk: 🔴 HIGH**

```python
# Before: obj.name()   ← called as method
# After:  obj.name     ← accessed as property (no parens)
```

All callers that call it as `obj.name()` get `TypeError: 'str' object is not callable`.

---

### 6.2 `@property` removed
**Risk: 🔴 HIGH**

Callers accessing `obj.name` (without parens) now get the function object, not
its return value.

---

### 6.3 `@staticmethod` ↔ `@classmethod` swap
**Risk: 🔴 HIGH**

- staticmethod → classmethod: first arg is now `cls` (passed automatically) → arg shift
- classmethod → staticmethod: `cls` no longer injected → first explicit arg gets `cls` value

---

### 6.4 `@classmethod` removed from constructor alternative
**Risk: 🔴 HIGH**

`MyClass.from_dict(...)` style constructors break if `@classmethod` is removed.

---

### 6.5 Caching decorator added (`@functools.lru_cache`, `@cache`)
**Risk: 🟡 MEDIUM**

Function is now cached — side-effectful callers that rely on re-evaluation get
stale results. Unhashable args now raise `TypeError`.

---

### 6.6 Validation / auth decorator removed
**Risk: 🔴 HIGH** (security / correctness)

`@login_required`, `@validate_input`, etc. — removing them silently removes
security checks. Flag these immediately.

---

## 7. Async / Sync Boundary Changes

### 7.1 Sync function → async
**Risk: 🔴 HIGH**

```python
# Before
result = fetch_data()
# After — callers must become async or use an event loop
result = await fetch_data()
```

Callers that don't `await` get a coroutine object, not the data. Silent bug.

---

### 7.2 Async function → sync
**Risk: 🔴 HIGH**

Callers using `await` will raise `TypeError: object NoneType can't be used in 'await' expression`.

---

### 7.3 Blocking call introduced in async function
**Risk: 🟡 MEDIUM**

`time.sleep()`, `requests.get()`, file I/O in `async def` blocks the event loop.
Not a crash, but a serious performance regression in async frameworks.

---

## 8. Global State & Singleton Changes

### 8.1 Module-level constant changed
**Risk: 🟡 MEDIUM**

```python
# Before: MAX_RETRIES = 3
# After:  MAX_RETRIES = 0
```

All code using `from module import MAX_RETRIES` gets a snapshot; `import module`
callers get the new value. Behaviour diverges.

---

### 8.2 Singleton pattern broken (constructor now creates new instance)
**Risk: 🔴 HIGH**

Code expecting one shared instance now creates multiple — state diverges.

---

### 8.3 Registry / factory mapping changed
**Risk: 🟡 MEDIUM**

`HANDLERS = {"json": JsonHandler, ...}` — removing or renaming a key causes
`KeyError` at dispatch time.

---

## 9. Type Annotation Changes (Runtime-Enforced)

### 9.1 Pydantic model field removed or renamed
**Risk: 🔴 HIGH**

`MyModel(removed_field=value)` raises `ValidationError`. All instantiation sites break.

---

### 9.2 Pydantic field type narrowed (Optional[str] → str)
**Risk: 🔴 HIGH**

Passing `None` where `str` is now required raises `ValidationError` at runtime.

---

### 9.3 Dataclass field `default` removed
**Risk: 🔴 HIGH**

Was optional in construction, now required. All call sites that omit it break.

---

### 9.4 TypedDict key removed or type changed
**Risk: 🟡 MEDIUM** (mostly static analysis, unless `mypy` is strict at runtime via plugins)

---

## 10. Data Model Changes (ORM / Dataclasses / Pydantic)

### 10.1 SQLAlchemy / Django ORM: column removed
**Risk: 🔴 HIGH**

Column access raises `AttributeError` (or worse, silently produces wrong query).
Requires a migration; if migration hasn't run, **every** request touching that model breaks.

---

### 10.2 Column `nullable=True` → `nullable=False`
**Risk: 🔴 HIGH**

Existing rows or code paths that write `None` will raise `IntegrityError`.

---

### 10.3 Relationship changed (lazy → eager, or removed)
**Risk: 🟡 MEDIUM**

Code that traverses `obj.related_objects` may now trigger N+1 queries or
`DetachedInstanceError`.

---

### 10.4 Migration not paired with model change
**Risk: 🔴 HIGH**

Model changed in code but no migration in diff → schema/code mismatch at deploy.
Check for `migrations/` or `alembic/versions/` files in the diff.

---

## 11. Protocol & Duck-Typing Contract Changes

### 11.1 Required method removed from a class used as a protocol
**Risk: 🔴 HIGH**

Code that calls `obj.do_thing()` on an instance of this class will get `AttributeError`.

---

### 11.2 Method signature change on a class that implements an interface
**Risk: 🔴 HIGH**

Protocol conformance broken — abstract method mismatch raises `TypeError` on
instantiation (if using `ABC`), or silent wrong-dispatch otherwise.

---

## 12. Generator / Iterator Changes

### 12.1 `return` values changed to `yield` (function → generator)
**Risk: 🔴 HIGH**

Callers expecting a list or single value now get a generator object.
`result[0]` raises `TypeError`.

---

### 12.2 `yield` removed (generator → regular function)
**Risk: 🔴 HIGH**

Callers iterating with `for x in func()` now iterate over a non-iterable (or a list
if the return was changed appropriately — check carefully).

---

### 12.3 `StopIteration` semantics changed
**Risk: 🟡 MEDIUM**

In Python 3.7+, `StopIteration` propagating out of a generator becomes `RuntimeError`.
Manually raising `StopIteration` inside a generator is now an error.

---

## 13. Context Manager Changes

### 13.1 `__enter__` return value changed
**Risk: 🟡 MEDIUM**

```python
with get_conn() as conn:
    conn.execute(...)   # if conn is now None, AttributeError
```

---

### 13.2 `__exit__` no longer suppresses exceptions
**Risk: 🟡 MEDIUM** (or potentially higher if calling code relies on exception suppression)

---

### 13.3 Context manager changed to non-context-manager
**Risk: 🔴 HIGH**

`with` usage raises `AttributeError: __enter__` if the object no longer supports
the context manager protocol.

---

## 14. Configuration & Environment Changes

### 14.1 Environment variable name changed
**Risk: 🔴 HIGH**

```python
# Before: os.environ["DB_HOST"]
# After:  os.environ["DATABASE_HOST"]
```

Old env var now silently returns `None` or raises `KeyError` depending on call style.

---

### 14.2 Config file key renamed
**Risk: 🔴 HIGH**

Similar to above — deployed environments won't have the new key name until
configuration is updated.

---

### 14.3 Default value for a config key changed
**Risk: 🟡 MEDIUM**

Environments that don't set the var explicitly get different behaviour after deploy.

---

## 15. Thread / Process Safety Changes

### 15.1 Lock / mutex removed
**Risk: 🔴 HIGH** (in concurrent code)

Race conditions introduced. May not manifest under low load.

---

### 15.2 Shared mutable state introduced
**Risk: 🟡 MEDIUM**

```python
# After: class-level mutable list shared across instances in multi-threaded use
cache = []
```

---

### 15.3 `threading.local()` usage added or removed
**Risk: 🟡 MEDIUM**

Per-thread state changes affect code that passes data between threads.

---

## Quick Risk Assignment Reference

| Change Type | Typical Risk | Key Question |
|---|---|---|
| Arg removed | 🔴 HIGH | Does any caller pass it? |
| Required arg added | 🔴 HIGH | Are all callers updated? |
| Optional arg added | 🟡 MEDIUM | Do callers use inspect/\*\*kwargs? |
| Arg order swapped | 🔴 HIGH | Any positional callers? |
| Return type changed | 🔴 HIGH | Is return value used? |
| Exception type changed | 🟡 MEDIUM | Do callers catch specifically? |
| Module renamed/moved | 🔴 HIGH | Are all import paths updated? |
| Base class changed | 🔴 HIGH | Any isinstance checks or super() chains? |
| `@property` added | 🔴 HIGH | Any `obj.method()` callers? |
| Sync → async | 🔴 HIGH | Are all await sites updated? |
| Body-only change | 🟢 LOW | Does behaviour contract hold? |
| New symbol added | 🟢 LOW | Does it shadow anything? |
| Constant value changed | 🟡 MEDIUM | Is it used as a sentinel/boundary? |
| ORM column removed | 🔴 HIGH | Is there a migration? |
| Pydantic field narrowed | 🔴 HIGH | Can existing data still validate? |

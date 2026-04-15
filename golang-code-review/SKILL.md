---
name: golang-code-review
description: >
  Deep Go code review skill that detects potential breakage from code changes by
  tracing call graphs, interface implementations, struct usages, and import paths.
  Use this skill whenever someone asks to review Go code changes, check for breaking
  changes, audit a git diff, find what could break after a refactor, trace the impact
  of a function, interface, or struct change, or validate a PR before merge. Also
  trigger when the user says things like "what could this break", "review my Go
  changes", "is this safe to merge", "check my diff", "find regressions", or pastes
  a diff or mentions git diff / HEAD / master in a Go project. Works in Claude Code,
  OpenCode, Cursor (GUI and CLI), and any coding agent that follows the SKILL.md
  convention.
---

# Go Code Review — Breakage & Impact Analyzer

This skill performs structured static analysis of Go code changes to surface potential
runtime and compile-time breakage **before** tests run or a PR merges. It combines
git diff extraction, Go declaration parsing, call-graph and interface-implementation
tracing, and a curated Go-specific breakage taxonomy to produce an actionable risk
report.

---

## Argument handling — check $ARGUMENTS first

When the skill is invoked, read `$ARGUMENTS` before doing anything else.

If `$ARGUMENTS` is `--help` or `-h`, print the following help message verbatim and
stop — do not run any analysis:

```
golang-code-review — Go breakage & impact analyzer

USAGE
  /golang-code-review [OPTIONS]

OPTIONS
  (none)              Auto-detect changes: uncommitted + committed vs. origin
                      default branch
  --base <ref>        Diff against a specific git ref
                        Examples: origin/main  HEAD~3  HEAD~1  abc1234
  --file <path>       Analyse a saved .diff file instead of running git
  --uncommitted       Only staged/unstaged working-tree changes (skip commits)
  --committed         Only commits not yet pushed (skip working tree)
  --build             Run  go build ./...  after analysis to confirm compile
  --vet               Run  go vet ./...    after analysis for static checks
  --output <path>     Write the JSON report to a file as well as printing it
  --quiet             Skip human-readable summary; emit JSON only
  --help, -h          Show this message

EXAMPLES
  /golang-code-review
      Review all changes on this branch vs. origin/master (auto-detected)

  /golang-code-review --base origin/main
      Diff everything between this branch and origin/main

  /golang-code-review --base HEAD~3
      Review only the last 3 commits

  /golang-code-review --file /tmp/pr.diff
      Analyse a diff file (e.g. exported from a GitHub PR)

  /golang-code-review --uncommitted
      Only check working-tree changes, ignore commits

  /golang-code-review --committed
      Only check commits not yet pushed

  /golang-code-review --base HEAD~1 --build --vet
      Review the last commit, then run go build + go vet

  /golang-code-review --base HEAD~1 --output /tmp/report.json
      Save JSON report to file

WHAT IT CHECKS
  • Function/method signature changes (params, return types, receiver)
  • Interface changes — adding a method breaks ALL existing implementations
  • Struct field changes (removed fields, type changes, json/db tag changes)
  • Export status changes (Exported ↔ unexported)
  • Type definition and alias changes
  • Method receiver changes (value vs pointer receiver)
  • Generic type constraint changes (Go 1.18+)
  • go.mod / go.sum dependency and module path changes
  • Package rename or removal
  • Callers, implementors, and embedding sites of every changed symbol

OUTPUT
  A risk report with 🔴 HIGH / 🟡 MEDIUM / 🟢 LOW findings, an impact map
  showing every caller, implementor, and struct-user, plus test coverage gaps.

REQUIREMENTS
  Python 3.8+, git in PATH. go build / go vet only needed for --build / --vet.
  No pip installs required.
```

If `$ARGUMENTS` contains any other flags, parse them as described in the
"Argument mapping" section below, then proceed to Step 1.

### Argument mapping

| User flag | Action |
|---|---|
| `--base <ref>` | Pass `--base-ref <ref>` to `analyze_diff.py` |
| `--file <path>` | Pass `--diff-file <path>` to `analyze_diff.py` |
| `--uncommitted` | Run `git diff --cached && git diff` only; feed via `--diff-file` |
| `--committed` | Use merge-base as `--base-ref`; skip working-tree diff |
| `--build` | After analysis, run `go build ./...` and append any errors to report |
| `--vet` | After analysis, run `go vet ./...` and append output to report |
| `--output <path>` | Pass `--output <path>` to both scripts |
| `--quiet` | Pass `--quiet` to both scripts |
| *(none)* | Run `analyze_diff.py --repo-root .` with no extra flags (auto-detect) |

---

## Quick-start decision tree

```
User pastes diff or asks to "review Go changes"
        │
        ▼
[Step 1] Determine what to diff (see §Diff Strategy)
        │
        ▼
[Step 2] Run scripts/analyze_diff.go to extract changed Go declarations
        │
        ▼
[Step 3] Run scripts/trace_callers.go to find downstream impact
        │
        ▼
[Step 4] (optional) Run go build ./... and go vet ./...
        │
        ▼
[Step 5] Apply breakage taxonomy (see references/breakage_patterns.md)
        │
        ▼
[Step 6] Produce Risk Report (see §Report Format)
```

---

## Step 1 — Diff Strategy

Choose the right git command based on the situation. Always `cd` to the repo root
(where `go.mod` lives) first.

### A. Uncommitted working-tree changes vs. master/main

```bash
DEFAULT_BRANCH=$(git remote show origin 2>/dev/null | grep "HEAD branch" | awk '{print $NF}')
DEFAULT_BRANCH=${DEFAULT_BRANCH:-main}

# Full unified diff (uncommitted, vs. remote default branch)
git diff origin/$DEFAULT_BRANCH...HEAD -- '*.go' '*.mod' '*.sum'

# Also include unstaged changes
git diff -- '*.go' '*.mod' '*.sum'
```

### B. Committed changes not yet pushed (HEAD vs. branch point)

```bash
BASE=$(git merge-base HEAD origin/$DEFAULT_BRANCH 2>/dev/null || \
       git merge-base HEAD $DEFAULT_BRANCH)

# Full diff from branch point to HEAD
git diff $BASE HEAD -- '*.go' '*.mod' '*.sum'

# List changed Go files only
git diff --name-only $BASE HEAD -- '*.go'

# Inspect a single file at any ref
git show HEAD:path/to/file.go
git show $BASE:path/to/file.go
```

### C. Specific commit or PR SHA

```bash
git show <SHA> -- '*.go'
git diff <SHA>^..<SHA> -- '*.go'
```

### D. Inline / pasted diff

```bash
cat > /tmp/review_input.diff << 'EOF'
<pasted diff here>
EOF
python scripts/analyze_diff.go --diff-file /tmp/review_input.diff --repo-root .
```

### Traversing to find files

```bash
# Find a Go file by name
find . -name "target.go" -not -path "*/vendor/*" -not -path "*/.git/*"

# Find a symbol definition across the repo
grep -rn "^func TargetFunc\|^type TargetType" --include="*.go" .

# Find all interface implementations (files that implement a method)
grep -rn "func .* MethodName(" --include="*.go" .

# Find all struct embeddings
grep -rn "StructName$\|StructName " --include="*.go" .
```

### go.mod changes — always check

```bash
git diff $BASE HEAD -- go.mod go.sum
```

Any change to `go.mod` (module path, Go version bump, dependency add/remove/replace)
should be flagged separately — see breakage_patterns.md §Module & Dependency Changes.

---

## Step 2 — Run analyze_diff.go

`scripts/analyze_diff.go` uses `go/ast` and `go/parser` from the Go standard
library — no third-party dependencies, no `go.mod` needed to run it.

```bash
# Show full help
go run <skill_dir>/scripts/analyze_diff.go --help
```

```
usage: analyze_diff [--repo-root REPO_ROOT] [--base-ref BASE_REF]
                    [--diff-file DIFF_FILE] [--output OUTPUT] [--quiet]

options:
  --repo-root   Path to repo root containing go.mod (default: .)
  --base-ref    Git ref to diff against (omit = auto-detect)
  --diff-file   Pre-existing .diff file; skips git entirely
  --output      Write JSON results to this path (also printed to stdout)
  --quiet       Suppress human-readable summary; emit JSON only
```

```bash
# Auto-detect all changes
go run <skill_dir>/scripts/analyze_diff.go --repo-root .

# Specific base
go run <skill_dir>/scripts/analyze_diff.go --repo-root . --base-ref origin/main

# Last 3 commits
go run <skill_dir>/scripts/analyze_diff.go --repo-root . --base-ref HEAD~3

# Pasted diff
go run <skill_dir>/scripts/analyze_diff.go --repo-root . --diff-file /tmp/pr.diff

# CI usage (JSON only, write to file)
go run <skill_dir>/scripts/analyze_diff.go \
    --repo-root . --output report.json --quiet
```

Or compile once for faster repeated use:

```bash
go build -o ~/.local/bin/go-review-diff <skill_dir>/scripts/analyze_diff.go
go-review-diff --repo-root . --base-ref HEAD~3
```

The script outputs structured JSON:

```json
{
  "summary": {
    "files_changed": 3,
    "declarations_changed": 8,
    "high_risk": 3,
    "medium_risk": 3,
    "low_risk": 2,
    "overall_risk": "high",
    "go_mod_changed": true
  },
  "changed_files": ["internal/auth/auth.go", "pkg/models/user.go"],
  "go_mod_changes": ["require github.com/foo/bar v1.2.0 → v1.3.0"],
  "changed_declarations": [
    {
      "file": "internal/auth/auth.go",
      "symbol": "Authenticate",
      "package": "auth",
      "kind": "function",
      "exported": true,
      "change_type": "signature_change",
      "old_signature": "func Authenticate(ctx context.Context, username, password string) error",
      "new_signature": "func Authenticate(ctx context.Context, username, password string, opts ...Option) error",
      "risk": "medium",
      "reason": "Variadic opts added — backward compatible at call sites, breaks callers using reflect or func-value assignment"
    }
  ]
}
```

---

## Step 3 — Run trace_callers.go

`scripts/trace_callers.go` walks the entire source tree using `go/ast` to find
callers, interface implementors, struct embedders, and type users.

```bash
# Show full help
go run <skill_dir>/scripts/trace_callers.go --help
```

```
usage: trace_callers [--repo-root REPO_ROOT] [--changes-json CHANGES_JSON]
                     [--output OUTPUT] [--quiet]

options:
  --repo-root      Path to repo root (default: .)
  --changes-json   JSON file from analyze_diff.go --output
                   (omit to read from stdin when piping)
  --output         Write impact map JSON to this path
  --quiet          JSON only, no human-readable output
```

```bash
# Pipe directly from analyze_diff (most common)
go run <skill_dir>/scripts/analyze_diff.go --repo-root . | \
    go run <skill_dir>/scripts/trace_callers.go --repo-root .

# Use a saved changes file
go run <skill_dir>/scripts/trace_callers.go \
    --repo-root . --changes-json /tmp/changes.json

# Full pipeline, both outputs written to files
go run <skill_dir>/scripts/analyze_diff.go \
    --repo-root . --output /tmp/changes.json --quiet && \
go run <skill_dir>/scripts/trace_callers.go \
    --repo-root . --changes-json /tmp/changes.json --output /tmp/impact.json
```

Or compile once for faster repeated use:

```bash
go build -o ~/.local/bin/go-review-trace <skill_dir>/scripts/trace_callers.go
```

---

## Step 4 — Optional: go build and go vet

Run these after the static analysis to catch anything the diff parser missed:

```bash
# Compile-check every package
go build ./...

# Static analysis (nil dereferences, unreachable code, printf format mismatches, etc.)
go vet ./...

# If staticcheck is installed (recommended)
staticcheck ./...

# Run tests to catch behavioural regressions
go test ./... -count=1
```

Append any compiler errors or vet warnings to the risk report as additional HIGH/MEDIUM
findings.

---

## Step 5 — Apply breakage taxonomy

Read `references/breakage_patterns.md` to classify each changed declaration. Key
Go-specific categories:

- **Signature changes** — params added/removed/reordered, return type changes
- **Interface changes** — method added (breaks all implementors), removed (safe)
- **Struct field changes** — field removed/renamed/type-changed; positional literal breakage
- **Export status** — Exported→unexported or vice versa
- **Receiver changes** — value↔pointer, type rename
- **Type changes** — alias vs definition, constraint changes for generics
- **go.mod changes** — module path, Go version, dependency version bumps
- **Concurrency contracts** — mutex removed, channel direction changed
- **Error handling** — sentinel errors renamed/removed, error type changes
- **init() and global state** — init order, package-level var changes

---

## Step 6 — Risk Report Format

```
# Go Code Review — Breakage Risk Report
**Repo:** <module path from go.mod>
**Base:** <base ref>  →  **Head:** <head ref or "working tree">
**Reviewed:** <timestamp>

---

## 🔴 HIGH RISK  (<N> issues)

### 1. `internal/auth/auth.go` — interface `Authenticator` — method added
- **Change:** `Validate(token string) bool` added to interface
- **Impact:** Every type that implements `Authenticator` now fails to compile
- **Implementors found:** `MockAuth` (auth_test.go:12), `JWTAuth` (jwt/jwt.go:34)
- **Fix:** Implement `Validate` on all existing implementors, or add it with a
  default in a new embedding struct

---

## 🟡 MEDIUM RISK  (<N> issues)
...

## 🟢 LOW RISK / INFO  (<N> issues)
...

---

## Impact Map

| Symbol | Package | Callers | Implementors | Struct Users | Risk |
|--------|---------|---------|--------------|--------------|------|
| `Authenticate` | auth | 6 | — | — | 🟡 MEDIUM |
| `Authenticator` | auth | — | 2 | 3 | 🔴 HIGH |
| `UserModel` | models | 4 | — | 7 | 🔴 HIGH |

---

## go.mod Changes
- `go 1.21` → `go 1.22` — confirm all team members have updated toolchain
- `require github.com/foo/bar v1.2.0 → v1.3.0` — review bar's changelog for
  breaking changes in that version range

---

## Test Coverage Gaps
- `Authenticate` has 6 callers but 0 test files call it directly
- `UserModel` struct changes not reflected in table-driven test fixtures

---

## Summary
<1-3 sentence plain-English verdict>
```

---

## Handling Go-specific edge cases

**Vendor directory:** Skip `vendor/` in all file traversals — changes there are
managed by `go mod vendor` and are not direct code changes.

**Generated files:** Files with `// Code generated` at the top are auto-generated.
Flag their changes as informational only — the real change is in the generator.

**Build tags:** A file with `//go:build !production` may not be compiled in all
environments. Note build tag constraints when reporting impact.

**cgo files:** Changes to `import "C"` blocks or `#cgo` directives require C
toolchain awareness. Flag these as HIGH and recommend manual review.

**Blank imports:** `import _ "pkg"` is for side-effects (init registration). Adding
or removing these changes program initialization — flag as MEDIUM.

---

## Reference files

- `references/breakage_patterns.md` — Full Go breakage taxonomy: 15 categories,
  50+ patterns with risk levels, detection heuristics, and fix guidance.
- `scripts/analyze_diff.go` — Git diff extractor + Go declaration change parser.
  Entry point for all reviews. No external deps.
- `scripts/trace_callers.go` — Reverse call/implementation graph builder. Finds
  callers, interface implementors, struct embedders, and type users across the repo.

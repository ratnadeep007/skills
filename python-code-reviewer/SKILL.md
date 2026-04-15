---
name: python-code-review
description: >
  Deep Python code review skill that detects potential breakage from code changes by
  tracing call graphs, import trees, and data flows. Use this skill whenever someone
  asks to review Python code changes, check for breaking changes, audit a git diff,
  find what could break after a refactor, trace the impact of a function or class
  change, or validate a PR before merge. Also trigger when the user says things like
  "what could this break", "review my changes", "is this safe to merge", "check my
  diff", "find regressions", or pastes a diff or mentions git diff / HEAD / master.
  Works in Claude Code, OpenCode, Cursor (GUI and CLI), and any coding agent that
  follows the SKILL.md convention.
---

# Python Code Review — Breakage & Impact Analyzer

This skill performs structured static analysis of Python code changes to surface
potential runtime breakage **before** tests run or a PR merges. It combines git diff
extraction, Python AST analysis, call-graph tracing, and a curated breakage taxonomy
to produce an actionable risk report.

---

## Argument handling — check $ARGUMENTS first

When the skill is invoked, read `$ARGUMENTS` before doing anything else.

If `$ARGUMENTS` is `--help` or `-h`, print the following help message verbatim and
stop — do not run any analysis:

```
python-code-review — Python breakage & impact analyzer

USAGE
  /python-code-review [OPTIONS]

OPTIONS
  (none)              Auto-detect changes: uncommitted + committed vs. origin default branch
  --base <ref>        Diff against a specific git ref
                        Examples: origin/main  HEAD~3  HEAD~1  abc1234
  --file <path>       Analyse a saved .diff file instead of running git
  --uncommitted       Only look at unstaged/staged changes (skip committed)
  --committed         Only look at commits not yet pushed (skip working tree)
  --output <path>     Write the JSON report to a file as well as printing it
  --quiet             Skip the human-readable summary; emit JSON only
  --help, -h          Show this message

EXAMPLES
  /python-code-review
      Review all changes on this branch vs. origin/master (auto-detected)

  /python-code-review --base origin/main
      Diff everything between this branch and origin/main

  /python-code-review --base HEAD~3
      Review only the last 3 commits

  /python-code-review --file /tmp/pr.diff
      Analyse a diff file (e.g. exported from a GitHub PR)

  /python-code-review --uncommitted
      Only check working-tree changes (staged + unstaged), ignore commits

  /python-code-review --committed
      Only check commits not yet pushed to the remote

  /python-code-review --base HEAD~1 --output /tmp/report.json
      Diff the last commit, save the JSON report to a file

WHAT IT CHECKS
  • Function/method signature changes (added/removed/reordered args)
  • Sync ↔ async boundary changes
  • Class hierarchy and decorator changes
  • Removed or renamed symbols and modules
  • Import path breakage
  • Callers and importers of every changed symbol (full repo scan)
  • Test coverage gaps for affected call sites

OUTPUT
  A risk report with 🔴 HIGH / 🟡 MEDIUM / 🟢 LOW findings, an impact map
  showing every caller and importer, and suggested test coverage gaps.

REQUIREMENTS
  Python 3.8+, git in PATH. No pip installs needed.
```

If `$ARGUMENTS` contains any other flags, parse them as described in the
"Argument mapping" section below before proceeding to Step 1.

### Argument mapping

| User flag | Script flag(s) to use |
|---|---|
| `--base <ref>` | `--base-ref <ref>` on `analyze_diff.py` |
| `--file <path>` | `--diff-file <path>` on `analyze_diff.py` |
| `--uncommitted` | Run `git diff --cached` + `git diff` only; pass result via `--diff-file` |
| `--committed` | Use merge-base as `--base-ref`; skip working-tree diff |
| `--output <path>` | `--output <path>` on both scripts |
| `--quiet` | `--quiet` on both scripts |
| *(none)* | Run `analyze_diff.py --repo-root .` with no extra flags (auto-detect) |

---

## Quick-start decision tree

```
User pastes diff or asks to "review changes"
        │
        ▼
[Step 1] Determine what to diff (see §Diff Strategy)
        │
        ▼
[Step 2] Run scripts/analyze_diff.py to extract changed symbols
        │
        ▼
[Step 3] Run scripts/trace_callers.py to find downstream impact
        │
        ▼
[Step 4] Apply breakage taxonomy (see references/breakage_patterns.md)
        │
        ▼
[Step 5] Produce Risk Report (see §Report Format)
```

---

## Step 1 — Diff Strategy

Choose the right git command based on the situation. Always `cd` to the repo root first.

### A. Uncommitted working-tree changes vs. master/main

```bash
# Detect default branch name
DEFAULT_BRANCH=$(git remote show origin 2>/dev/null | grep "HEAD branch" | awk '{print $NF}')
DEFAULT_BRANCH=${DEFAULT_BRANCH:-master}

# Full unified diff (uncommitted, vs. remote default branch)
git diff origin/$DEFAULT_BRANCH...HEAD -- '*.py'

# If there are also unstaged changes in the working tree:
git diff origin/$DEFAULT_BRANCH -- '*.py'
```

If `origin` is not available (local-only repo), fall back to:

```bash
git diff $DEFAULT_BRANCH -- '*.py'          # staged + committed delta
git diff -- '*.py'                           # unstaged working-tree changes
```

### B. Committed changes not yet pushed (HEAD vs. branch point)

```bash
# Find the merge-base between HEAD and master/main
BASE=$(git merge-base HEAD origin/$DEFAULT_BRANCH 2>/dev/null || git merge-base HEAD $DEFAULT_BRANCH)

# Full diff from branch point to HEAD
git diff $BASE HEAD -- '*.py'

# List only the changed Python files
git diff --name-only $BASE HEAD -- '*.py'

# Examine a single file's full history on this branch
git log --oneline $BASE..HEAD -- path/to/file.py
git show HEAD:path/to/file.py          # current committed version
git show $BASE:path/to/file.py         # version at branch point
```

### C. Specific commit or PR SHA

```bash
git show <SHA> -- '*.py'               # diff for one commit
git diff <SHA>^..<SHA> -- '*.py'       # equivalent, explicit
```

### D. Inline diff (user pasted a diff)

When the user pastes a raw diff directly into the conversation, skip git entirely.
Write the diff to `/tmp/review_input.diff` and pass it directly to `analyze_diff.py`:

```bash
cat > /tmp/review_input.diff << 'EOF'
<pasted diff here>
EOF
python scripts/analyze_diff.py --diff-file /tmp/review_input.diff --repo-root .
```

### Traversing to find files

If a diff references a file that doesn't exist at its stated path, search for it:

```bash
# Find by filename
find . -name "target_file.py" -not -path "*/\.*"

# Find by a symbol name that appears in the diff
grep -rn "def changed_function\|class ChangedClass" --include="*.py" .
```

---

## Step 2 — Run analyze_diff.py

`scripts/analyze_diff.py` parses the diff and extracts the **changed symbols**
(functions, classes, methods, module-level assignments) along with a structured
change record for each.

```bash
# Show full help
python <skill_dir>/scripts/analyze_diff.py --help
```

```
usage: analyze_diff.py [-h] [--repo-root REPO_ROOT] [--base-ref BASE_REF]
                       [--diff-file DIFF_FILE] [--output OUTPUT] [--quiet]

options:
  --repo-root   Path to git repo root (default: current directory)
  --base-ref    Git ref to diff against, e.g. origin/main, HEAD~3, <SHA>
                (omit to auto-detect: compares uncommitted + committed changes
                vs. the remote default branch)
  --diff-file   Path to a pre-existing .diff file (skips git entirely —
                useful when reviewing a pasted diff or PR export)
  --output      Write JSON results to this file path (also always printed
                to stdout so you can pipe to trace_callers.py)
  --quiet       Suppress the human-readable summary; emit JSON only
  -h, --help    Show this message and exit
```

```bash
# Auto-detect uncommitted + committed changes vs. origin/master
python <skill_dir>/scripts/analyze_diff.py --repo-root .

# Explicit base ref
python <skill_dir>/scripts/analyze_diff.py --repo-root . --base-ref origin/main

# Last 3 commits only
python <skill_dir>/scripts/analyze_diff.py --repo-root . --base-ref HEAD~3

# Specific commit SHA
python <skill_dir>/scripts/analyze_diff.py --repo-root . --base-ref abc1234^

# Inline / pasted diff
python <skill_dir>/scripts/analyze_diff.py --repo-root . --diff-file /tmp/my.diff

# Write JSON + suppress console output (for CI)
python <skill_dir>/scripts/analyze_diff.py --repo-root . --output report.json --quiet
```

The script outputs a JSON structure:

```json
{
  "summary": {
    "files_changed": 3,
    "symbols_changed": 7,
    "high_risk": 2,
    "medium_risk": 3,
    "low_risk": 2
  },
  "changed_files": ["src/auth.py", "src/models/user.py"],
  "changed_symbols": [
    {
      "file": "src/auth.py",
      "symbol": "authenticate",
      "kind": "function",
      "change_type": "signature_change",
      "old_signature": "authenticate(username, password)",
      "new_signature": "authenticate(username, password, mfa_token=None)",
      "risk": "medium",
      "reason": "New kwarg with default — backward compatible unless callers use positional args beyond position 2"
    }
  ]
}
```

---

## Step 3 — Run trace_callers.py

`scripts/trace_callers.py` walks the entire Python source tree and builds a
**reverse call graph**: for each changed symbol, it finds every file and line that
calls or imports it.

```bash
# Show full help
python <skill_dir>/scripts/trace_callers.py --help
```

```
usage: trace_callers.py [-h] [--repo-root REPO_ROOT]
                        [--changes-json CHANGES_JSON] [--stdin]
                        [--output OUTPUT] [--quiet]

options:
  --repo-root      Path to git repo root (default: current directory)
  --changes-json   JSON file produced by analyze_diff.py --output
                   (omit when piping from analyze_diff.py)
  --stdin          Read the changes JSON from stdin instead of a file
                   (set automatically when piping)
  --output         Write impact map JSON to this file path (also printed
                   to stdout)
  --quiet          Suppress the human-readable impact report; emit JSON only
  -h, --help       Show this message and exit
```

```bash
# Pipe directly from analyze_diff (most common)
python <skill_dir>/scripts/analyze_diff.py --repo-root . | \
    python <skill_dir>/scripts/trace_callers.py --repo-root . --stdin

# Use a saved changes file
python <skill_dir>/scripts/trace_callers.py \
    --repo-root . --changes-json /tmp/changes.json

# Full pipeline writing both outputs to files
python <skill_dir>/scripts/analyze_diff.py \
    --repo-root . --output /tmp/changes.json --quiet && \
python <skill_dir>/scripts/trace_callers.py \
    --repo-root . --changes-json /tmp/changes.json --output /tmp/impact.json
```

Output:

```json
{
  "impact_map": {
    "src/auth.py::authenticate": {
      "callers": [
        {"file": "src/views/login.py",  "line": 42, "call_expr": "authenticate(u, p)"},
        {"file": "tests/test_auth.py",  "line": 17, "call_expr": "authenticate('admin', 'x')"}
      ],
      "importers": ["src/views/login.py", "src/middleware/auth.py"],
      "risk_escalation": "callers use positional args — new mfa_token param is safe"
    }
  }
}
```

---

## Step 4 — Apply breakage taxonomy

Read `references/breakage_patterns.md` to match each changed symbol against the
curated set of Python-specific breakage categories. The patterns file contains:

- **Signature changes** (positional arg add/remove, default removal, kwarg-only enforcement)
- **Return type / shape changes** (None returns, dict key changes, list→generator)
- **Exception contract changes** (new raises, removed catches)
- **Import / module restructuring** (moved symbols, renamed modules)
- **Class hierarchy changes** (base class changes, MRO shifts, `__init__` signature)
- **Decorator changes** (property→method, classmethod/staticmethod swap)
- **Global state / singleton mutations**
- **Async/sync boundary changes**
- **Type annotation narrowing** (runtime-enforced via Pydantic/dataclasses)

For each changed symbol, determine which patterns apply, then set final risk.

---

## Step 5 — Risk Report Format

Always produce the report in this exact structure. Keep it tight — developers scan
reports, they don't read them linearly.

```
# Code Review — Breakage Risk Report
**Repo:** <repo name or path>
**Base:** <base ref>  →  **Head:** <head ref or "working tree">
**Reviewed:** <timestamp>

---

## 🔴 HIGH RISK  (<N> issues)

### 1. `src/auth.py` — `authenticate()` signature change
- **Change:** Removed `timeout` positional arg (was arg #3)
- **Impact:** 4 callers pass 3 positional args → will raise `TypeError` at runtime
- **Files affected:** `src/views/login.py:42`, `tests/test_auth.py:17`, ...
- **Fix:** Update all callers OR make `timeout` keyword-only with a default

---

## 🟡 MEDIUM RISK  (<N> issues)
...

## 🟢 LOW RISK / INFO  (<N> issues)
...

---

## Impact Map

| Symbol | Changed In | Callers | Importers | Risk |
|--------|-----------|---------|-----------|------|
| `authenticate` | src/auth.py | 4 | 2 | 🔴 HIGH |
| `UserModel` | src/models/user.py | 7 | 5 | 🟡 MEDIUM |

---

## Suggested Test Coverage Gaps
- `authenticate()` has 2 callers with NO test coverage
- `UserModel.__init__` change not reflected in fixture factories

---

## Summary
<1-3 sentence plain-English verdict: safe to merge / needs fixes / needs review>
```

---

## Handling ambiguous or large diffs

**Large diffs (>500 lines changed):**
Split into passes. First pass: identify all signature and interface changes (highest
breakage potential). Second pass: logic changes within existing interfaces. Third
pass: new additions (usually safe).

**Refactors (rename/move):**
Check both the old import path and the new one. Old importers will break unless
there's a compatibility shim (`from new.path import X` in the old module).

**Database model changes (SQLAlchemy / Django ORM):**
Flag any column removal, type narrowing, or nullable→non-nullable change as HIGH
risk — these require migrations and can cause runtime errors if migrations haven't
run.

**Dependency injection / registry patterns:**
If a class is registered in a factory or DI container, callers may not appear in
the grep — note this as a caveat in the report.

---

## Environment compatibility

This skill runs in any environment where Python 3.8+ and git are available. The
analysis scripts use only the standard library (`ast`, `os`, `re`, `subprocess`,
`json`, `argparse`) — no pip installs required.

**Claude Code / OpenCode / Cursor CLI:**
Run scripts directly via the terminal tool. Use `--repo-root $(git rev-parse --show-toplevel)` to auto-detect the repo root.

**Cursor GUI / IDE agents:**
Paste the diff into the conversation. The skill handles inline diffs via `--diff-file`.

**CI integration:**
```bash
# Drop into any CI step (GitHub Actions, GitLab CI, etc.)
python python-code-review/scripts/analyze_diff.py \
    --repo-root . \
    --base-ref origin/main \
    --output breakage_report.json
```

---

## Reference files

- `references/breakage_patterns.md` — Full taxonomy of Python breakage patterns with
  risk levels, detection heuristics, and real-world examples. Read this when you need
  to classify a subtle change or explain risk to the user.

- `scripts/analyze_diff.py` — Git diff extractor + AST symbol change parser.
  Self-contained, no external deps. Entry point for all reviews.

- `scripts/trace_callers.py` — AST-based reverse call graph builder. Finds all
  callers and importers of changed symbols across the full repo.

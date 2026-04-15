#!/usr/bin/env python3
"""
analyze_diff.py — Python Code Review: Diff Extractor + AST Symbol Change Parser

Extracts changed Python symbols from a git diff (or an inline .diff file) and
performs AST-level analysis to classify each change and assign an initial risk level.

Usage:
    # Auto-detect uncommitted changes vs. origin/master
    python analyze_diff.py --repo-root /path/to/repo

    # Compare HEAD against a specific base
    python analyze_diff.py --repo-root /path/to/repo --base-ref origin/main

    # Analyse the last N commits
    python analyze_diff.py --repo-root /path/to/repo --base-ref HEAD~3

    # Use a pre-existing diff file (e.g. from a paste)
    python analyze_diff.py --repo-root /path/to/repo --diff-file /tmp/my.diff

    # Write structured JSON results
    python analyze_diff.py --repo-root /path/to/repo --output /tmp/changes.json

Requires: Python 3.8+, git in PATH. No third-party packages.
"""

import argparse
import ast
import json
import os
import re
import subprocess
import sys
import textwrap
from dataclasses import dataclass, field, asdict
from pathlib import Path
from typing import Dict, List, Optional, Tuple


# ---------------------------------------------------------------------------
# Data structures
# ---------------------------------------------------------------------------

@dataclass
class SymbolChange:
    file: str
    symbol: str           # qualified name, e.g. "MyClass.my_method"
    kind: str             # function | method | class | assignment | decorator
    change_type: str      # see CHANGE_TYPES below
    old_signature: Optional[str] = None
    new_signature: Optional[str] = None
    old_bases: Optional[List[str]] = None
    new_bases: Optional[List[str]] = None
    removed_args: List[str] = field(default_factory=list)
    added_args: List[str] = field(default_factory=list)
    changed_defaults: List[str] = field(default_factory=list)
    kwonly_enforced: List[str] = field(default_factory=list)
    return_annotation_changed: bool = False
    async_changed: bool = False
    decorators_changed: bool = False
    risk: str = "low"     # high | medium | low
    reason: str = ""
    added_lines: List[int] = field(default_factory=list)
    removed_lines: List[int] = field(default_factory=list)


CHANGE_TYPES = {
    "added":              "New symbol (usually safe unless it shadows something)",
    "removed":            "Symbol deleted — all callers/importers will break",
    "signature_change":   "Function/method signature changed",
    "class_change":       "Class definition changed (bases, metaclass, decorators)",
    "body_change":        "Internal logic change only (interface unchanged)",
    "rename":             "Symbol appears to be renamed",
    "async_change":       "Sync↔async boundary changed",
    "decorator_change":   "Decorators changed — may alter runtime behaviour",
    "assignment_change":  "Module-level variable/constant changed",
}


# ---------------------------------------------------------------------------
# Git helpers
# ---------------------------------------------------------------------------

def run(cmd: List[str], cwd: str, capture=True) -> str:
    """Run a shell command and return stdout, or raise on error."""
    result = subprocess.run(
        cmd, cwd=cwd,
        stdout=subprocess.PIPE if capture else None,
        stderr=subprocess.PIPE,
        text=True,
    )
    if result.returncode != 0 and capture:
        # Non-zero exit is often informational (e.g. no diff) — return empty
        return ""
    return result.stdout or ""


def detect_default_branch(repo_root: str) -> str:
    """Return the remote default branch name, falling back to 'master'."""
    # Try remote HEAD
    out = run(["git", "remote", "show", "origin"], repo_root)
    m = re.search(r"HEAD branch:\s+(\S+)", out)
    if m:
        return m.group(1)
    # Try common names
    for branch in ("main", "master", "develop", "trunk"):
        out = run(["git", "rev-parse", "--verify", f"origin/{branch}"], repo_root)
        if out.strip():
            return branch
        out = run(["git", "rev-parse", "--verify", branch], repo_root)
        if out.strip():
            return branch
    return "master"


def get_diff_text(repo_root: str, base_ref: Optional[str], diff_file: Optional[str]) -> str:
    """Return the unified diff text to analyse."""
    if diff_file:
        return Path(diff_file).read_text(encoding="utf-8", errors="replace")

    if base_ref:
        # Explicit base — diff between base and HEAD (includes staged + committed)
        diff = run(["git", "diff", base_ref, "HEAD", "--", "*.py"], repo_root)
        if not diff:
            # Maybe HEAD IS the base (e.g. --base-ref HEAD~0); try working tree
            diff = run(["git", "diff", base_ref, "--", "*.py"], repo_root)
        return diff

    # Auto-detect
    default_branch = detect_default_branch(repo_root)
    origin_ref = f"origin/{default_branch}"

    # Check if origin/<branch> exists
    has_origin = bool(run(["git", "rev-parse", "--verify", origin_ref], repo_root).strip())
    base = origin_ref if has_origin else default_branch

    # Try merge-base diff (committed on this branch)
    merge_base = run(["git", "merge-base", "HEAD", base], repo_root).strip()
    diff_committed = ""
    if merge_base:
        diff_committed = run(["git", "diff", merge_base, "HEAD", "--", "*.py"], repo_root)

    # Staged changes
    diff_staged = run(["git", "diff", "--cached", "--", "*.py"], repo_root)

    # Unstaged / working-tree changes
    diff_unstaged = run(["git", "diff", "--", "*.py"], repo_root)

    return "\n".join(filter(None, [diff_committed, diff_staged, diff_unstaged]))


def get_file_content_at_ref(repo_root: str, filepath: str, ref: str) -> Optional[str]:
    """Return file content at a given git ref, or None if not found."""
    out = run(["git", "show", f"{ref}:{filepath}"], repo_root)
    return out if out else None


# ---------------------------------------------------------------------------
# Diff parsing
# ---------------------------------------------------------------------------

@dataclass
class FileDiff:
    path: str
    old_path: Optional[str]   # set on renames
    hunks: List[Dict]         # [{old_start, old_count, new_start, new_count, lines}]
    is_new_file: bool = False
    is_deleted: bool = False
    is_rename: bool = False


def parse_diff(diff_text: str) -> List[FileDiff]:
    """Parse a unified diff into FileDiff objects."""
    file_diffs: List[FileDiff] = []
    current: Optional[FileDiff] = None
    current_hunk: Optional[Dict] = None

    for line in diff_text.splitlines():
        if line.startswith("diff --git "):
            if current and current_hunk:
                current.hunks.append(current_hunk)
                current_hunk = None
            if current:
                file_diffs.append(current)
            # Extract path from "diff --git a/path b/path"
            m = re.match(r"diff --git a/(.*) b/(.*)", line)
            path = m.group(2) if m else line.split()[-1]
            current = FileDiff(path=path, old_path=None, hunks=[])

        elif line.startswith("new file"):
            if current:
                current.is_new_file = True
        elif line.startswith("deleted file"):
            if current:
                current.is_deleted = True
        elif line.startswith("rename from "):
            if current:
                current.old_path = line[len("rename from "):]
                current.is_rename = True
        elif line.startswith("rename to "):
            if current:
                current.path = line[len("rename to "):]

        elif line.startswith("--- ") or line.startswith("+++ "):
            pass  # skip file headers

        elif line.startswith("@@"):
            if current and current_hunk:
                current.hunks.append(current_hunk)
            # @@ -old_start,old_count +new_start,new_count @@
            m = re.match(r"@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@", line)
            if m and current:
                current_hunk = {
                    "old_start": int(m.group(1)),
                    "old_count": int(m.group(2) or 1),
                    "new_start": int(m.group(3)),
                    "new_count": int(m.group(4) or 1),
                    "lines": [],
                }

        elif current_hunk is not None:
            current_hunk["lines"].append(line)

    if current and current_hunk:
        current.hunks.append(current_hunk)
    if current:
        file_diffs.append(current)

    return file_diffs


def hunk_added_removed_lines(hunk: Dict) -> Tuple[List[int], List[int]]:
    """Return (added_line_numbers, removed_line_numbers) for a hunk."""
    added, removed = [], []
    old_lineno = hunk["old_start"]
    new_lineno = hunk["new_start"]
    for line in hunk["lines"]:
        if line.startswith("+"):
            added.append(new_lineno)
            new_lineno += 1
        elif line.startswith("-"):
            removed.append(old_lineno)
            old_lineno += 1
        else:
            old_lineno += 1
            new_lineno += 1
    return added, removed


# ---------------------------------------------------------------------------
# AST helpers
# ---------------------------------------------------------------------------

def safe_parse(source: str) -> Optional[ast.Module]:
    """Parse Python source to AST, returning None on syntax error."""
    try:
        return ast.parse(source)
    except SyntaxError:
        return None


def extract_signature(node: ast.FunctionDef) -> str:
    """Return a human-readable signature string."""
    args = node.args
    parts = []

    # positional-only (Python 3.8+)
    for i, a in enumerate(args.posonlyargs):
        default_idx = i - (len(args.posonlyargs) - len(args.defaults))
        default = f"={ast.unparse(args.defaults[default_idx])}" if default_idx >= 0 else ""
        parts.append(f"{a.arg}{default}")
    if args.posonlyargs:
        parts.append("/")

    # regular args
    offset = len(args.posonlyargs)
    for i, a in enumerate(args.args):
        di = (offset + i) - (len(args.posonlyargs) + len(args.args) - len(args.defaults))
        default = f"={ast.unparse(args.defaults[di])}" if di >= 0 else ""
        annotation = f": {ast.unparse(a.annotation)}" if a.annotation else ""
        parts.append(f"{a.arg}{annotation}{default}")

    if args.vararg:
        parts.append(f"*{args.vararg.arg}")
    elif args.kwonlyargs:
        parts.append("*")

    for i, a in enumerate(args.kwonlyargs):
        default = f"={ast.unparse(args.kw_defaults[i])}" if args.kw_defaults[i] else ""
        annotation = f": {ast.unparse(a.annotation)}" if a.annotation else ""
        parts.append(f"{a.arg}{annotation}{default}")

    if args.kwarg:
        parts.append(f"**{args.kwarg.arg}")

    ret = f" -> {ast.unparse(node.returns)}" if node.returns else ""
    is_async = "async " if isinstance(node, ast.AsyncFunctionDef) else ""
    return f"{is_async}def {node.name}({', '.join(parts)}){ret}"


def get_arg_names(func: ast.FunctionDef) -> List[str]:
    args = func.args
    names = (
        [a.arg for a in args.posonlyargs]
        + [a.arg for a in args.args]
        + ([args.vararg.arg] if args.vararg else [])
        + [a.arg for a in args.kwonlyargs]
        + ([args.kwarg.arg] if args.kwarg else [])
    )
    return names


def get_decorator_names(node) -> List[str]:
    decorators = []
    for d in node.decorator_list:
        decorators.append(ast.unparse(d))
    return decorators


def get_base_names(cls: ast.ClassDef) -> List[str]:
    return [ast.unparse(b) for b in cls.bases]


def symbols_in_tree(tree: ast.Module, prefix: str = "") -> Dict[str, ast.AST]:
    """Return {qualified_name: ast_node} for all top-level and class-nested symbols."""
    result = {}
    for node in ast.walk(tree):
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            result[node.name] = node
        elif isinstance(node, ast.ClassDef):
            result[node.name] = node
            for item in node.body:
                if isinstance(item, (ast.FunctionDef, ast.AsyncFunctionDef)):
                    result[f"{node.name}.{item.name}"] = item
        elif isinstance(node, ast.Assign):
            for t in node.targets:
                if isinstance(t, ast.Name) and t.id.isupper():
                    # Only track module-level UPPER_CASE constants
                    result[t.id] = node
    return result


# ---------------------------------------------------------------------------
# Change classification
# ---------------------------------------------------------------------------

def classify_function_change(
    old: ast.FunctionDef,
    new: ast.FunctionDef,
    filepath: str,
    symbol: str,
) -> SymbolChange:
    change = SymbolChange(
        file=filepath,
        symbol=symbol,
        kind="method" if "." in symbol else "function",
        change_type="body_change",
        old_signature=extract_signature(old),
        new_signature=extract_signature(new),
    )

    old_args = get_arg_names(old)
    new_args = get_arg_names(new)

    removed = [a for a in old_args if a not in new_args and a != "self" and a != "cls"]
    added = [a for a in new_args if a not in old_args and a != "self" and a != "cls"]

    change.removed_args = removed
    change.added_args = added

    # Async boundary change
    old_async = isinstance(old, ast.AsyncFunctionDef)
    new_async = isinstance(new, ast.AsyncFunctionDef)
    if old_async != new_async:
        change.async_changed = True
        change.change_type = "async_change"
        change.risk = "high"
        change.reason = (
            f"Function changed from {'async' if old_async else 'sync'} to "
            f"{'async' if new_async else 'sync'} — all await/call sites must be updated"
        )
        return change

    # Decorator changes
    old_decs = get_decorator_names(old)
    new_decs = get_decorator_names(new)
    if old_decs != new_decs:
        change.decorators_changed = True
        change.change_type = "decorator_change"
        removed_decs = [d for d in old_decs if d not in new_decs]
        added_decs = [d for d in new_decs if d not in old_decs]
        if any(d in ("property", "staticmethod", "classmethod") for d in removed_decs + added_decs):
            change.risk = "high"
            change.reason = (
                f"Critical decorator changed: removed={removed_decs}, added={added_decs}. "
                "property/staticmethod/classmethod changes alter call semantics."
            )
        else:
            change.risk = "medium"
            change.reason = f"Decorators changed: removed={removed_decs}, added={added_decs}"
        return change

    # Return annotation changed
    old_ret = ast.unparse(old.returns) if old.returns else None
    new_ret = ast.unparse(new.returns) if new.returns else None
    if old_ret != new_ret:
        change.return_annotation_changed = True

    if removed:
        change.change_type = "signature_change"
        change.risk = "high"
        change.reason = (
            f"Arguments removed: {removed}. All callers passing these args will raise TypeError."
        )
    elif added:
        # Determine if new args have defaults
        new_args_obj = new.args
        all_new_with_defaults = True
        new_arg_names_with_defaults = set()

        # Build the set of arg names that have defaults
        n_posonly = len(new_args_obj.posonlyargs)
        n_regular = len(new_args_obj.args)
        total_pos = n_posonly + n_regular
        n_defaults = len(new_args_obj.defaults)
        defaults_start = total_pos - n_defaults

        for i, a in enumerate(new_args_obj.posonlyargs + new_args_obj.args):
            if i >= defaults_start:
                new_arg_names_with_defaults.add(a.arg)

        for i, a in enumerate(new_args_obj.kwonlyargs):
            if new_args_obj.kw_defaults[i] is not None:
                new_arg_names_with_defaults.add(a.arg)

        for arg_name in added:
            if arg_name not in new_arg_names_with_defaults:
                all_new_with_defaults = False
                break

        change.change_type = "signature_change"
        if all_new_with_defaults:
            change.risk = "medium"
            change.reason = (
                f"New args with defaults added: {added}. "
                "Backward compatible unless callers use **kwargs unpacking or positional arg count checks."
            )
        else:
            change.risk = "high"
            change.reason = (
                f"New required args added (no defaults): {added}. "
                "All existing callers will raise TypeError."
            )
    else:
        # Signature text changed but arg names same — could be type annotations or default value changes
        old_sig = extract_signature(old)
        new_sig = extract_signature(new)
        if old_sig != new_sig:
            change.change_type = "signature_change"
            # Check for default value changes in existing args
            change.risk = "low"
            change.reason = "Signature changed (annotations or default values). Verify default value semantics."
        else:
            change.change_type = "body_change"
            change.risk = "low"
            change.reason = "Internal implementation changed. Interface stable."

    return change


def classify_class_change(
    old: ast.ClassDef,
    new: ast.ClassDef,
    filepath: str,
    symbol: str,
) -> SymbolChange:
    old_bases = get_base_names(old)
    new_bases = get_base_names(new)
    old_decs = get_decorator_names(old)
    new_decs = get_decorator_names(new)

    change = SymbolChange(
        file=filepath,
        symbol=symbol,
        kind="class",
        change_type="class_change",
        old_bases=old_bases,
        new_bases=new_bases,
    )

    if old_bases != new_bases:
        change.risk = "high"
        removed_bases = [b for b in old_bases if b not in new_bases]
        added_bases = [b for b in new_bases if b not in old_bases]
        change.reason = (
            f"Base classes changed: removed={removed_bases}, added={added_bases}. "
            "MRO shift may silently change inherited behaviour or break isinstance() checks."
        )
    elif old_decs != new_decs:
        change.decorators_changed = True
        change.risk = "medium"
        change.reason = f"Class decorators changed: {old_decs} → {new_decs}"
    else:
        change.risk = "low"
        change.reason = "Class structure changed internally. Base classes and decorators unchanged."

    return change


# ---------------------------------------------------------------------------
# Core analysis
# ---------------------------------------------------------------------------

def analyse_file_diff(
    file_diff: FileDiff,
    repo_root: str,
    base_ref: Optional[str],
) -> List[SymbolChange]:
    changes: List[SymbolChange] = []
    filepath = file_diff.path

    # --- Deleted file ---
    if file_diff.is_deleted:
        changes.append(SymbolChange(
            file=filepath,
            symbol="<entire module>",
            kind="module",
            change_type="removed",
            risk="high",
            reason="Entire module deleted. All importers will raise ImportError.",
        ))
        return changes

    # --- New file ---
    if file_diff.is_new_file:
        changes.append(SymbolChange(
            file=filepath,
            symbol="<entire module>",
            kind="module",
            change_type="added",
            risk="low",
            reason="New module. No existing callers to break (unless it shadows another module).",
        ))
        return changes

    # --- Rename ---
    if file_diff.is_rename and file_diff.old_path:
        changes.append(SymbolChange(
            file=filepath,
            symbol="<entire module>",
            kind="module",
            change_type="rename",
            risk="high",
            reason=(
                f"Module renamed: {file_diff.old_path} → {filepath}. "
                "All importers using the old path will raise ImportError "
                "unless a compatibility shim is in place."
            ),
        ))

    # Gather all added and removed line numbers
    all_added: List[int] = []
    all_removed: List[int] = []
    for hunk in file_diff.hunks:
        a, r = hunk_added_removed_lines(hunk)
        all_added.extend(a)
        all_removed.extend(r)

    # Try to get old and new source for AST comparison
    old_source = None
    new_source = None

    abs_path = Path(repo_root) / filepath
    if abs_path.exists():
        new_source = abs_path.read_text(encoding="utf-8", errors="replace")

    if base_ref:
        old_source = get_file_content_at_ref(repo_root, filepath, base_ref)
    else:
        # Reconstruct old source from diff (apply hunks in reverse)
        old_source = reconstruct_old_source(new_source, file_diff) if new_source else None

    old_tree = safe_parse(old_source) if old_source else None
    new_tree = safe_parse(new_source) if new_source else None

    if not old_tree and not new_tree:
        # Can't parse either — report changed lines only
        changes.append(SymbolChange(
            file=filepath,
            symbol="<unparseable>",
            kind="unknown",
            change_type="body_change",
            risk="medium",
            reason="File changed but could not be parsed as valid Python. Manual review required.",
            added_lines=all_added,
            removed_lines=all_removed,
        ))
        return changes

    old_symbols = symbols_in_tree(old_tree) if old_tree else {}
    new_symbols = symbols_in_tree(new_tree) if new_tree else {}

    # Determine which symbols are touched by the diff
    touched = set()
    if new_tree:
        for node in ast.walk(new_tree):
            if hasattr(node, "lineno") and node.lineno in all_added:
                if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
                    touched.add(node.name)
                elif isinstance(node, ast.ClassDef):
                    touched.add(node.name)
    if old_tree:
        for node in ast.walk(old_tree):
            if hasattr(node, "lineno") and node.lineno in all_removed:
                if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
                    touched.add(node.name)

    # If we can't determine touched symbols, fall back to all changed symbols
    if not touched:
        all_syms = set(old_symbols) | set(new_symbols)
        # Rough heuristic: any symbol whose line is within the hunk range
        hunk_ranges = []
        for hunk in file_diff.hunks:
            hunk_ranges.append((hunk["old_start"], hunk["old_start"] + hunk["old_count"]))
        for sym, node in {**old_symbols, **new_symbols}.items():
            if hasattr(node, "lineno"):
                for start, end in hunk_ranges:
                    if start <= node.lineno <= end:
                        touched.add(sym)

    # Symbols only in old (removed)
    for sym in set(old_symbols) - set(new_symbols):
        if sym in touched or not touched:
            changes.append(SymbolChange(
                file=filepath,
                symbol=sym,
                kind=_kind(old_symbols[sym]),
                change_type="removed",
                risk="high",
                reason=f"`{sym}` removed. All callers/importers will fail at runtime.",
                removed_lines=all_removed,
            ))

    # Symbols only in new (added)
    for sym in set(new_symbols) - set(old_symbols):
        if sym in touched or not touched:
            changes.append(SymbolChange(
                file=filepath,
                symbol=sym,
                kind=_kind(new_symbols[sym]),
                change_type="added",
                risk="low",
                reason=f"`{sym}` added. No existing callers to break.",
                added_lines=all_added,
            ))

    # Symbols in both (modified)
    for sym in set(old_symbols) & set(new_symbols):
        if sym not in touched and touched:
            continue
        old_node = old_symbols[sym]
        new_node = new_symbols[sym]

        if isinstance(old_node, (ast.FunctionDef, ast.AsyncFunctionDef)) and \
           isinstance(new_node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            ch = classify_function_change(old_node, new_node, filepath, sym)
            ch.added_lines = all_added
            ch.removed_lines = all_removed
            if ch.change_type != "body_change" or True:  # always include
                changes.append(ch)

        elif isinstance(old_node, ast.ClassDef) and isinstance(new_node, ast.ClassDef):
            ch = classify_class_change(old_node, new_node, filepath, sym)
            ch.added_lines = all_added
            ch.removed_lines = all_removed
            changes.append(ch)

        else:
            # Type changed (e.g. function → class)
            changes.append(SymbolChange(
                file=filepath,
                symbol=sym,
                kind=_kind(new_node),
                change_type="removed",
                risk="high",
                reason=f"`{sym}` changed kind: {_kind(old_node)} → {_kind(new_node)}. All existing usages will break.",
                added_lines=all_added,
                removed_lines=all_removed,
            ))

    return changes


def _kind(node: ast.AST) -> str:
    if isinstance(node, ast.AsyncFunctionDef):
        return "async_function"
    if isinstance(node, ast.FunctionDef):
        return "function"
    if isinstance(node, ast.ClassDef):
        return "class"
    if isinstance(node, ast.Assign):
        return "assignment"
    return "unknown"


def reconstruct_old_source(new_source: str, file_diff: FileDiff) -> str:
    """Roughly reconstruct old source by inverting the diff hunks."""
    lines = new_source.splitlines(keepends=True)
    # Process hunks in reverse order to not mess up line numbers
    for hunk in reversed(file_diff.hunks):
        new_start = hunk["new_start"] - 1  # 0-indexed
        new_count = hunk["new_count"]
        old_lines = [
            l[1:] + ("\n" if not l[1:].endswith("\n") else "")
            for l in hunk["lines"]
            if l.startswith("-") or (not l.startswith("+") and not l.startswith("\\"))
        ]
        lines[new_start:new_start + new_count] = old_lines
    return "".join(lines)


# ---------------------------------------------------------------------------
# Risk summary
# ---------------------------------------------------------------------------

def build_summary(changes: List[SymbolChange], changed_files: List[str]) -> Dict:
    high = [c for c in changes if c.risk == "high"]
    medium = [c for c in changes if c.risk == "medium"]
    low = [c for c in changes if c.risk == "low"]

    return {
        "files_changed": len(changed_files),
        "symbols_changed": len(changes),
        "high_risk": len(high),
        "medium_risk": len(medium),
        "low_risk": len(low),
        "overall_risk": "high" if high else ("medium" if medium else "low"),
    }


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    parser = argparse.ArgumentParser(
        description="Analyse a Python git diff for potential breakage.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=textwrap.dedent(__doc__),
    )
    parser.add_argument("--repo-root", default=".", help="Path to git repo root")
    parser.add_argument("--base-ref", default=None, help="Base git ref to diff against")
    parser.add_argument("--diff-file", default=None, help="Path to a pre-existing .diff file")
    parser.add_argument("--output", default=None, help="Write JSON results to this path")
    parser.add_argument("--quiet", action="store_true", help="Suppress human-readable output")
    args = parser.parse_args()

    repo_root = str(Path(args.repo_root).resolve())

    # --- Get diff ---
    diff_text = get_diff_text(repo_root, args.base_ref, args.diff_file)
    if not diff_text.strip():
        print("No Python changes detected.", file=sys.stderr)
        result = {
            "summary": {"files_changed": 0, "symbols_changed": 0,
                        "high_risk": 0, "medium_risk": 0, "low_risk": 0,
                        "overall_risk": "none"},
            "changed_files": [],
            "changed_symbols": [],
        }
        if args.output:
            Path(args.output).write_text(json.dumps(result, indent=2))
        print(json.dumps(result, indent=2))
        return

    # --- Parse diff ---
    file_diffs = parse_diff(diff_text)
    changed_files = [fd.path for fd in file_diffs]

    # --- Analyse each file ---
    all_changes: List[SymbolChange] = []
    for fd in file_diffs:
        if not fd.path.endswith(".py") and not (fd.old_path or "").endswith(".py"):
            continue
        try:
            changes = analyse_file_diff(fd, repo_root, args.base_ref)
            all_changes.extend(changes)
        except Exception as e:
            all_changes.append(SymbolChange(
                file=fd.path,
                symbol="<error>",
                kind="unknown",
                change_type="body_change",
                risk="medium",
                reason=f"Analysis error: {e}",
            ))

    # --- Build output ---
    result = {
        "summary": build_summary(all_changes, changed_files),
        "changed_files": changed_files,
        "changed_symbols": [asdict(c) for c in all_changes],
    }

    output_json = json.dumps(result, indent=2)

    if args.output:
        Path(args.output).write_text(output_json)

    if not args.quiet:
        # Human-readable summary
        s = result["summary"]
        print(f"\n{'='*60}")
        print(f"  Python Code Review — Change Analysis")
        print(f"{'='*60}")
        print(f"  Files changed   : {s['files_changed']}")
        print(f"  Symbols changed : {s['symbols_changed']}")
        print(f"  Overall risk    : {s['overall_risk'].upper()}")
        print(f"    🔴 High       : {s['high_risk']}")
        print(f"    🟡 Medium     : {s['medium_risk']}")
        print(f"    🟢 Low        : {s['low_risk']}")
        print(f"{'='*60}\n")

        for c in sorted(all_changes, key=lambda x: {"high": 0, "medium": 1, "low": 2}[x.risk]):
            icon = {"high": "🔴", "medium": "🟡", "low": "🟢"}[c.risk]
            print(f"{icon} [{c.risk.upper():6}] {c.file} :: {c.symbol}")
            print(f"         change : {c.change_type}")
            if c.old_signature or c.new_signature:
                print(f"         old    : {c.old_signature}")
                print(f"         new    : {c.new_signature}")
            print(f"         reason : {c.reason}")
            print()

    # Always print JSON so callers can pipe it
    print(output_json)


if __name__ == "__main__":
    main()

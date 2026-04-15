#!/usr/bin/env python3
"""
trace_callers.py — Python Code Review: Reverse Call Graph Builder

For each changed symbol produced by analyze_diff.py, this script walks the
entire Python source tree and finds:

  1. Direct callers    — files/lines that call the function/method
  2. Importers         — files that import the symbol or its module
  3. Subclasses        — classes that inherit from a changed class
  4. Attribute access  — lines that access a changed attribute/property

Usage:
    # Pipe from analyze_diff.py
    python analyze_diff.py --repo-root . | python trace_callers.py --repo-root .

    # Use a JSON file from a previous analyze_diff run
    python trace_callers.py --repo-root . --changes-json /tmp/changes.json

    # Write structured JSON output
    python trace_callers.py --repo-root . --changes-json /tmp/changes.json \
        --output /tmp/impact.json

Requires: Python 3.8+. No third-party packages.
"""

import argparse
import ast
import json
import os
import re
import sys
import textwrap
from dataclasses import dataclass, field, asdict
from pathlib import Path
from typing import Dict, List, Optional, Set, Tuple


# ---------------------------------------------------------------------------
# Data structures
# ---------------------------------------------------------------------------

@dataclass
class CallSite:
    file: str
    line: int
    col: int
    call_expr: str        # the source snippet
    call_type: str        # direct_call | import | subclass | attribute_access | decorator_use


@dataclass
class SymbolImpact:
    symbol: str           # e.g. "src/auth.py::authenticate"
    file: str
    risk: str             # inherited from analyze_diff
    call_sites: List[CallSite] = field(default_factory=list)
    importers: List[str] = field(default_factory=list)
    subclasses: List[str] = field(default_factory=list)
    risk_escalation: str = ""
    test_coverage: bool = False   # True if any caller is in a test file


# ---------------------------------------------------------------------------
# Source-tree walker
# ---------------------------------------------------------------------------

IGNORED_DIRS = {
    ".git", ".hg", ".svn", "__pycache__", ".mypy_cache", ".pytest_cache",
    ".tox", "node_modules", "venv", ".venv", "env", ".env",
    "dist", "build", "site-packages", ".eggs",
}

def iter_python_files(root: str):
    """Yield absolute paths to all .py files under root, skipping ignored dirs."""
    for dirpath, dirnames, filenames in os.walk(root):
        # Prune ignored directories in-place
        dirnames[:] = [d for d in dirnames if d not in IGNORED_DIRS]
        for fname in filenames:
            if fname.endswith(".py"):
                yield os.path.join(dirpath, fname)


def safe_parse(source: str) -> Optional[ast.Module]:
    try:
        return ast.parse(source, type_comments=False)
    except SyntaxError:
        return None


def read_source(filepath: str) -> Optional[str]:
    try:
        return Path(filepath).read_text(encoding="utf-8", errors="replace")
    except OSError:
        return None


def source_snippet(source_lines: List[str], lineno: int, maxlen: int = 120) -> str:
    """Return the source line at lineno (1-indexed), trimmed."""
    if 1 <= lineno <= len(source_lines):
        return source_lines[lineno - 1].strip()[:maxlen]
    return ""


# ---------------------------------------------------------------------------
# Import analysis
# ---------------------------------------------------------------------------

def module_path_from_file(filepath: str, repo_root: str) -> str:
    """Convert a file path to a dotted module path relative to repo root."""
    rel = os.path.relpath(filepath, repo_root)
    parts = Path(rel).with_suffix("").parts
    return ".".join(parts).lstrip(".")


def extract_imports(tree: ast.Module) -> List[Tuple[str, Optional[str]]]:
    """
    Return [(imported_name, alias_or_None)] for all import statements.
    Handles:
        import foo                    → ("foo", None)
        import foo.bar as fb          → ("foo.bar", "fb")
        from foo import bar           → ("foo.bar", None)
        from foo import bar as b      → ("foo.bar", "b")
        from . import bar             → (".<current>.bar", None)  [relative, simplified]
    """
    results = []
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            for alias in node.names:
                results.append((alias.name, alias.asname))
        elif isinstance(node, ast.ImportFrom):
            module = node.module or ""
            level_dots = "." * (node.level or 0)
            for alias in node.names:
                if alias.name == "*":
                    results.append((f"{level_dots}{module}.*", None))
                else:
                    full = f"{level_dots}{module}.{alias.name}" if module else f"{level_dots}{alias.name}"
                    results.append((full, alias.asname))
    return results


# ---------------------------------------------------------------------------
# Call-site detection
# ---------------------------------------------------------------------------

class CallFinder(ast.NodeVisitor):
    """Visit an AST and record all call sites for a given set of symbol names."""

    def __init__(self, target_names: Set[str], source_lines: List[str]):
        self.target_names = target_names
        self.source_lines = source_lines
        self.found: List[Tuple[int, int, str]] = []  # (line, col, snippet)

    def _check_name(self, name: str, node: ast.AST):
        if name in self.target_names:
            snippet = source_snippet(self.source_lines, node.lineno)
            self.found.append((node.lineno, getattr(node, "col_offset", 0), snippet))

    def visit_Call(self, node: ast.Call):
        # Direct function call:  foo(...)
        if isinstance(node.func, ast.Name):
            self._check_name(node.func.id, node)
        # Method call:  obj.foo(...)
        elif isinstance(node.func, ast.Attribute):
            self._check_name(node.func.attr, node)
        self.generic_visit(node)

    def visit_Attribute(self, node: ast.Attribute):
        # Attribute access (not inside a Call):  obj.foo
        self._check_name(node.attr, node)
        self.generic_visit(node)

    def visit_Decorator(self, node):
        # Decorators that reference the symbol
        if isinstance(node, ast.Name):
            self._check_name(node.id, node)
        elif isinstance(node, ast.Attribute):
            self._check_name(node.attr, node)


class SubclassFinder(ast.NodeVisitor):
    """Find class definitions that subclass any of the target class names."""

    def __init__(self, target_classes: Set[str], source_lines: List[str]):
        self.target_classes = target_classes
        self.source_lines = source_lines
        self.found: List[Tuple[str, int, str]] = []  # (class_name, line, snippet)

    def visit_ClassDef(self, node: ast.ClassDef):
        for base in node.bases:
            base_name = None
            if isinstance(base, ast.Name):
                base_name = base.id
            elif isinstance(base, ast.Attribute):
                base_name = base.attr
            if base_name and base_name in self.target_classes:
                snippet = source_snippet(self.source_lines, node.lineno)
                self.found.append((node.name, node.lineno, snippet))
        self.generic_visit(node)


# ---------------------------------------------------------------------------
# Core impact tracing
# ---------------------------------------------------------------------------

def trace_impacts(
    changes_json: Dict,
    repo_root: str,
) -> Dict[str, SymbolImpact]:
    """
    For each changed symbol, scan all Python files and build an impact map.
    Returns {symbol_key: SymbolImpact}.
    """
    changed_symbols = changes_json.get("changed_symbols", [])
    changed_files_set = set(changes_json.get("changed_files", []))

    # Build lookup: what symbol names do we care about?
    # Key = "filepath::symbol_name"
    # Value = SymbolChange dict
    impact_map: Dict[str, SymbolImpact] = {}
    symbol_names: Dict[str, List[str]] = {}  # symbol_name → [key, ...]

    for cs in changed_symbols:
        sym = cs["symbol"]
        filepath = cs["file"]
        key = f"{filepath}::{sym}"
        impact_map[key] = SymbolImpact(
            symbol=key,
            file=filepath,
            risk=cs.get("risk", "low"),
        )
        # Track both qualified name and short name
        short_name = sym.split(".")[-1] if "." in sym else sym
        for name in {sym, short_name}:
            if name and name != "<entire module>" and name != "<unparseable>":
                symbol_names.setdefault(name, []).append(key)

    if not symbol_names:
        return impact_map

    # Resolve module paths for changed files (for import matching)
    changed_modules: Dict[str, str] = {}  # module_dotpath → key
    for key, impact in impact_map.items():
        mod_path = module_path_from_file(
            str(Path(repo_root) / impact.file), repo_root
        )
        changed_modules[mod_path] = key
        # Also track parent module
        parent = ".".join(mod_path.split(".")[:-1])
        if parent:
            changed_modules.setdefault(parent, key)

    target_names_set = set(symbol_names.keys())
    target_classes = {
        name for name, keys in symbol_names.items()
        for key in keys
        if impact_map[key].risk in ("high", "medium")
    }

    # Walk all Python files
    for py_file in iter_python_files(repo_root):
        rel_path = os.path.relpath(py_file, repo_root)

        # Skip the changed files themselves (we already know they changed)
        if rel_path in changed_files_set:
            continue

        source = read_source(py_file)
        if not source:
            continue

        source_lines = source.splitlines()
        tree = safe_parse(source)
        if not tree:
            continue

        is_test_file = (
            "test" in rel_path.lower()
            or "spec" in rel_path.lower()
            or os.path.basename(py_file).startswith("test_")
        )

        # --- Check imports ---
        imports = extract_imports(tree)
        for imported_name, alias in imports:
            # Strip leading dots for relative imports
            clean = imported_name.lstrip(".")
            for mod_path, key in changed_modules.items():
                if clean == mod_path or clean.startswith(mod_path + "."):
                    if key in impact_map:
                        if rel_path not in impact_map[key].importers:
                            impact_map[key].importers.append(rel_path)
                        if is_test_file:
                            impact_map[key].test_coverage = True

        # --- Find call sites ---
        call_finder = CallFinder(target_names_set, source_lines)
        call_finder.visit(tree)
        for lineno, col, snippet in call_finder.found:
            # Map back to the changed symbol key(s)
            matched_keys = set()
            for token in _tokens_at_line(source_lines, lineno):
                if token in symbol_names:
                    matched_keys.update(symbol_names[token])
            if not matched_keys:
                # Fallback: match any symbol name appearing on this line
                for name, keys in symbol_names.items():
                    if re.search(r'\b' + re.escape(name) + r'\b', snippet):
                        matched_keys.update(keys)
            for key in matched_keys:
                if key in impact_map:
                    impact_map[key].call_sites.append(CallSite(
                        file=rel_path,
                        line=lineno,
                        col=col,
                        call_expr=snippet,
                        call_type="direct_call",
                    ))
                    if is_test_file:
                        impact_map[key].test_coverage = True

        # --- Find subclasses ---
        sub_finder = SubclassFinder(target_classes, source_lines)
        sub_finder.visit(tree)
        for class_name, lineno, snippet in sub_finder.found:
            # Match back to changed class keys
            for name in target_classes:
                if re.search(r'\b' + re.escape(name) + r'\b', snippet):
                    if name in symbol_names:
                        for key in symbol_names[name]:
                            if key in impact_map:
                                entry = f"{rel_path}:{lineno}::{class_name}"
                                if entry not in impact_map[key].subclasses:
                                    impact_map[key].subclasses.append(entry)

    # --- Risk escalation notes ---
    for key, impact in impact_map.items():
        notes = []
        n_callers = len(impact.call_sites)
        n_importers = len(impact.importers)
        n_subclasses = len(impact.subclasses)

        # Check if callers use positional args (rough heuristic)
        positional_callers = [
            cs for cs in impact.call_sites
            if re.search(r'\w+\s*\([^)]+\)', cs.call_expr)
            and "=" not in cs.call_expr
        ]

        if n_callers > 0 and impact.risk == "medium":
            if positional_callers:
                notes.append(
                    f"{len(positional_callers)} caller(s) appear to use positional args "
                    "— new parameters may break them if not keyword-only"
                )
                impact.risk = "high"

        if n_subclasses > 0 and impact.risk in ("medium", "high"):
            notes.append(
                f"{n_subclasses} subclass(es) found — base class changes cascade"
            )

        if not impact.test_coverage and n_callers > 0:
            notes.append(f"{n_callers} caller(s) found with NO test coverage detected")

        if n_callers == 0 and n_importers == 0 and impact.risk == "high":
            notes.append("No callers or importers found — may be dead code or dynamically loaded")

        impact.risk_escalation = "; ".join(notes) if notes else "No additional escalation."

    return impact_map


def _tokens_at_line(source_lines: List[str], lineno: int) -> List[str]:
    """Return identifier tokens from a line of source."""
    if 1 <= lineno <= len(source_lines):
        return re.findall(r'\b[a-zA-Z_]\w*\b', source_lines[lineno - 1])
    return []


# ---------------------------------------------------------------------------
# Report rendering
# ---------------------------------------------------------------------------

def render_report(impact_map: Dict[str, SymbolImpact], changes_json: Dict) -> str:
    lines = []
    s = changes_json.get("summary", {})
    lines.append(f"\n{'='*60}")
    lines.append("  Python Code Review — Impact & Caller Analysis")
    lines.append(f"{'='*60}")
    lines.append(f"  Changed symbols tracked : {len(impact_map)}")

    total_callers = sum(len(i.call_sites) for i in impact_map.values())
    total_importers = sum(len(i.importers) for i in impact_map.values())
    total_subclasses = sum(len(i.subclasses) for i in impact_map.values())

    lines.append(f"  Total call sites found  : {total_callers}")
    lines.append(f"  Total importers found   : {total_importers}")
    lines.append(f"  Total subclasses found  : {total_subclasses}")
    lines.append(f"{'='*60}\n")

    # Sort by risk
    sorted_impacts = sorted(
        impact_map.values(),
        key=lambda x: {"high": 0, "medium": 1, "low": 2}.get(x.risk, 3)
    )

    for impact in sorted_impacts:
        icon = {"high": "🔴", "medium": "🟡", "low": "🟢"}.get(impact.risk, "⚪")
        sym_short = impact.symbol.split("::")[-1]
        lines.append(f"{icon} [{impact.risk.upper():6}] {impact.symbol}")

        if impact.call_sites:
            lines.append(f"         callers ({len(impact.call_sites)}):")
            for cs in impact.call_sites[:10]:  # cap at 10 for readability
                lines.append(f"           • {cs.file}:{cs.line}  {cs.call_expr}")
            if len(impact.call_sites) > 10:
                lines.append(f"           … and {len(impact.call_sites)-10} more")

        if impact.importers:
            lines.append(f"         importers ({len(impact.importers)}): {', '.join(impact.importers[:5])}")

        if impact.subclasses:
            lines.append(f"         subclasses ({len(impact.subclasses)}): {', '.join(impact.subclasses[:5])}")

        cov = "✅ has test coverage" if impact.test_coverage else "⚠️  no test coverage detected"
        lines.append(f"         coverage   : {cov}")

        if impact.risk_escalation and impact.risk_escalation != "No additional escalation.":
            lines.append(f"         notes      : {impact.risk_escalation}")

        lines.append("")

    return "\n".join(lines)


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    parser = argparse.ArgumentParser(
        description="Trace callers and importers of changed Python symbols.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=textwrap.dedent(__doc__),
    )
    parser.add_argument("--repo-root", default=".", help="Path to git repo root")
    parser.add_argument(
        "--changes-json",
        default=None,
        help="JSON file from analyze_diff.py (omit to read from stdin)",
    )
    parser.add_argument("--stdin", action="store_true", help="Read changes JSON from stdin")
    parser.add_argument("--output", default=None, help="Write impact JSON to this path")
    parser.add_argument("--quiet", action="store_true", help="Suppress human-readable output")
    args = parser.parse_args()

    # --- Load changes ---
    if args.changes_json:
        with open(args.changes_json) as f:
            raw = f.read()
    elif args.stdin or not sys.stdin.isatty():
        raw = sys.stdin.read()
    else:
        print("Error: provide --changes-json or pipe from analyze_diff.py", file=sys.stderr)
        sys.exit(1)

    # analyze_diff.py prints human text THEN JSON — find the JSON block
    json_start = raw.find("{")
    if json_start == -1:
        print("Error: no JSON found in input", file=sys.stderr)
        sys.exit(1)
    changes_json = json.loads(raw[json_start:])

    repo_root = str(Path(args.repo_root).resolve())

    # --- Trace ---
    impact_map = trace_impacts(changes_json, repo_root)

    # --- Build output ---
    result = {
        "summary": {
            "symbols_tracked": len(impact_map),
            "total_call_sites": sum(len(i.call_sites) for i in impact_map.values()),
            "total_importers": sum(len(i.importers) for i in impact_map.values()),
            "total_subclasses": sum(len(i.subclasses) for i in impact_map.values()),
            "symbols_with_no_test_coverage": sum(
                1 for i in impact_map.values()
                if not i.test_coverage and (i.call_sites or i.importers)
            ),
        },
        "impact_map": {k: asdict(v) for k, v in impact_map.items()},
    }

    output_json = json.dumps(result, indent=2)

    if args.output:
        Path(args.output).write_text(output_json)

    if not args.quiet:
        print(render_report(impact_map, changes_json))

    print(output_json)


if __name__ == "__main__":
    main()

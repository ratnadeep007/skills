// analyze_diff.go — Go Code Review: Diff Extractor + AST Change Classifier
//
// Parses a git diff of Go source files, uses go/ast to extract and compare
// declarations between old and new versions, and classifies each change with
// a risk level. Outputs a human-readable summary and structured JSON.
//
// Usage:
//
//	go run analyze_diff.go [--repo-root .] [--base-ref origin/main] [--diff-file f.diff] [--output report.json] [--quiet]
//
// Or compile once and run as a binary:
//
//	go build -o analyze_diff analyze_diff.go
//	./analyze_diff --repo-root /path/to/repo --base-ref HEAD~3
//
// Requires: Go 1.18+, git in PATH. No external dependencies.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Data structures
// ---------------------------------------------------------------------------

// Declaration captures the parsed shape of a single Go top-level declaration.
type Declaration struct {
	Name         string   // e.g. "Authenticate", "User.Validate"
	Kind         string   // function | method | interface | struct | type_alias | type | const | var
	Signature    string   // full text of the declaration header
	Fields       []string // interface method signatures or struct field names+types
	Receiver     string   // for methods: receiver type name (without *)
	IsPtrReceiver bool
	IsExported   bool
	HasGenerics  bool
	Package      string
}

// DeclChange records a single changed declaration and its risk assessment.
type DeclChange struct {
	File          string   `json:"file"`
	Symbol        string   `json:"symbol"`
	Package       string   `json:"package"`
	Kind          string   `json:"kind"`
	Exported      bool     `json:"exported"`
	ChangeType    string   `json:"change_type"`
	OldSignature  string   `json:"old_signature,omitempty"`
	NewSignature  string   `json:"new_signature,omitempty"`
	RemovedFields []string `json:"removed_fields,omitempty"`
	AddedFields   []string `json:"added_fields,omitempty"`
	Risk          string   `json:"risk"`   // high | medium | low
	Reason        string   `json:"reason"`
}

// FileDiff is a parsed representation of one file's section in a unified diff.
type FileDiff struct {
	Path      string
	OldPath   string
	Hunks     []Hunk
	IsNew     bool
	IsDeleted bool
	IsRename  bool
}

// Hunk is one @@ block from a unified diff.
type Hunk struct {
	OldStart, OldCount int
	NewStart, NewCount int
	Lines              []string // raw diff lines including +/-/space prefix
}

// AnalysisResult is the top-level JSON output.
type AnalysisResult struct {
	Summary              Summary       `json:"summary"`
	ChangedFiles         []string      `json:"changed_files"`
	ChangedDeclarations  []DeclChange  `json:"changed_declarations"`
}

// Summary contains aggregate counts.
type Summary struct {
	FilesChanged         int    `json:"files_changed"`
	DeclarationsChanged  int    `json:"declarations_changed"`
	HighRisk             int    `json:"high_risk"`
	MediumRisk           int    `json:"medium_risk"`
	LowRisk              int    `json:"low_risk"`
	OverallRisk          string `json:"overall_risk"`
	GoModChanged         bool   `json:"go_mod_changed"`
}

// ---------------------------------------------------------------------------
// Git helpers
// ---------------------------------------------------------------------------

func runGit(repoRoot string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = repoRoot
	out, _ := cmd.Output()
	return string(out)
}

func detectDefaultBranch(repoRoot string) string {
	out := runGit(repoRoot, "remote", "show", "origin")
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "HEAD branch:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "HEAD branch:"))
		}
	}
	for _, name := range []string{"main", "master", "develop", "trunk"} {
		if runGit(repoRoot, "rev-parse", "--verify", "origin/"+name) != "" {
			return name
		}
		if runGit(repoRoot, "rev-parse", "--verify", name) != "" {
			return name
		}
	}
	return "main"
}

func getDiff(repoRoot, baseRef, diffFile string) string {
	if diffFile != "" {
		b, err := os.ReadFile(diffFile)
		if err != nil {
			fatalf("reading diff file: %v", err)
		}
		return string(b)
	}

	goPatterns := []string{"--", "*.go", "go.mod", "go.sum"}

	if baseRef != "" {
		if out := runGit(repoRoot, append([]string{"diff", baseRef, "HEAD"}, goPatterns...)...); out != "" {
			return out
		}
		return runGit(repoRoot, append([]string{"diff", baseRef}, goPatterns...)...)
	}

	defaultBranch := detectDefaultBranch(repoRoot)
	originRef := "origin/" + defaultBranch
	hasOrigin := strings.TrimSpace(runGit(repoRoot, "rev-parse", "--verify", originRef)) != ""
	base := originRef
	if !hasOrigin {
		base = defaultBranch
	}

	mergeBase := strings.TrimSpace(runGit(repoRoot, "merge-base", "HEAD", base))
	var parts []string
	if mergeBase != "" {
		parts = append(parts, runGit(repoRoot, append([]string{"diff", mergeBase, "HEAD"}, goPatterns...)...))
	}
	parts = append(parts,
		runGit(repoRoot, append([]string{"diff", "--cached"}, goPatterns...)...),
		runGit(repoRoot, append([]string{"diff"}, goPatterns...)...),
	)
	return strings.Join(filterNonEmpty(parts), "\n")
}

func getFileAtRef(repoRoot, path, ref string) string {
	return runGit(repoRoot, "show", ref+":"+path)
}

// ---------------------------------------------------------------------------
// Unified diff parser
// ---------------------------------------------------------------------------

var reHunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

func parseDiff(text string) []FileDiff {
	var files []FileDiff
	var cur *FileDiff
	var curHunk *Hunk

	flush := func() {
		if curHunk != nil && cur != nil {
			cur.Hunks = append(cur.Hunks, *curHunk)
			curHunk = nil
		}
	}

	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			if cur != nil {
				files = append(files, *cur)
			}
			// "diff --git a/path b/path"
			parts := strings.Fields(line)
			path := strings.TrimPrefix(parts[len(parts)-1], "b/")
			cur = &FileDiff{Path: path}

		case strings.HasPrefix(line, "new file"):
			if cur != nil {
				cur.IsNew = true
			}
		case strings.HasPrefix(line, "deleted file"):
			if cur != nil {
				cur.IsDeleted = true
			}
		case strings.HasPrefix(line, "rename from "):
			if cur != nil {
				cur.OldPath = strings.TrimPrefix(line, "rename from ")
				cur.IsRename = true
			}
		case strings.HasPrefix(line, "rename to "):
			if cur != nil {
				cur.Path = strings.TrimPrefix(line, "rename to ")
			}

		case strings.HasPrefix(line, "@@"):
			flush()
			if cur == nil {
				continue
			}
			m := reHunkHeader.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			h := Hunk{
				OldStart: atoi(m[1]), OldCount: atoidef(m[2], 1),
				NewStart: atoi(m[3]), NewCount: atoidef(m[4], 1),
			}
			curHunk = &h

		default:
			if curHunk != nil && !strings.HasPrefix(line, "---") && !strings.HasPrefix(line, "+++") {
				curHunk.Lines = append(curHunk.Lines, line)
			}
		}
	}
	flush()
	if cur != nil {
		files = append(files, *cur)
	}
	return files
}

// ---------------------------------------------------------------------------
// Go AST parsing — extract declarations from source
// ---------------------------------------------------------------------------

// extractDeclarations parses Go source and returns a map of qualified name →
// Declaration. Methods are keyed as "ReceiverType.MethodName".
func extractDeclarations(src []byte) map[string]Declaration {
	decls := make(map[string]Declaration)
	if len(src) == 0 {
		return decls
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil && f == nil {
		return decls
	}

	pkgName := ""
	if f.Name != nil {
		pkgName = f.Name.Name
	}

	for _, decl := range f.Decls {
		switch d := decl.(type) {

		case *ast.FuncDecl:
			name := d.Name.Name
			key := name

			recv := ""
			isPtrRecv := false
			if d.Recv != nil && len(d.Recv.List) > 0 {
				switch rt := d.Recv.List[0].Type.(type) {
				case *ast.StarExpr:
					if id, ok := rt.X.(*ast.Ident); ok {
						recv = id.Name
						isPtrRecv = true
					}
				case *ast.Ident:
					recv = rt.Name
				case *ast.IndexExpr: // generic receiver T[P]
					if id, ok := rt.X.(*ast.Ident); ok {
						recv = id.Name
					}
				case *ast.IndexListExpr: // generic receiver T[P, Q]
					if id, ok := rt.X.(*ast.Ident); ok {
						recv = id.Name
					}
				}
				if recv != "" {
					key = recv + "." + name
				}
			}

			sig := funcSignature(src, fset, d)
			hasGenerics := d.Type.TypeParams != nil && len(d.Type.TypeParams.List) > 0

			decls[key] = Declaration{
				Name:          key,
				Kind:          kindOf(recv),
				Signature:     sig,
				Receiver:      recv,
				IsPtrReceiver: isPtrRecv,
				IsExported:    ast.IsExported(name),
				HasGenerics:   hasGenerics,
				Package:       pkgName,
			}

		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					name := s.Name.Name
					hasGenerics := s.TypeParams != nil && len(s.TypeParams.List) > 0
					var kind string
					var fields []string
					var sig string

					switch tt := s.Type.(type) {
					case *ast.InterfaceType:
						kind = "interface"
						fields = interfaceMethods(tt, src, fset)
						sig = "type " + name + " interface{ " + strings.Join(fields, "; ") + " }"
					case *ast.StructType:
						kind = "struct"
						fields = structFields(tt, src, fset)
						sig = "type " + name + " struct{ " + strings.Join(fields, "; ") + " }"
					default:
						// Type alias or definition
						typeText := srcSlice(src, fset, s.Type.Pos(), s.Type.End())
						if s.Assign.IsValid() {
							kind = "type_alias"
							sig = "type " + name + " = " + typeText
						} else {
							kind = "type"
							sig = "type " + name + " " + typeText
						}
					}
					if hasGenerics {
						sig += " [generic]"
					}

					decls[name] = Declaration{
						Name:        name,
						Kind:        kind,
						Signature:   sig,
						Fields:      fields,
						IsExported:  ast.IsExported(name),
						HasGenerics: hasGenerics,
						Package:     pkgName,
					}

				case *ast.ValueSpec:
					// const or var — only track exported top-level ones
					kw := "var"
					if d.Tok.String() == "const" {
						kw = "const"
					}
					for _, name := range s.Names {
						if !ast.IsExported(name.Name) {
							continue
						}
						typeText := ""
						if s.Type != nil {
							typeText = " " + srcSlice(src, fset, s.Type.Pos(), s.Type.End())
						}
						decls[name.Name] = Declaration{
							Name:       name.Name,
							Kind:       kw,
							Signature:  kw + " " + name.Name + typeText,
							IsExported: true,
							Package:    pkgName,
						}
					}
				}
			}
		}
	}
	return decls
}

// funcSignature extracts the text of a function signature (everything up to
// the opening brace) from source bytes.
func funcSignature(src []byte, fset *token.FileSet, fn *ast.FuncDecl) string {
	start := fset.Position(fn.Pos()).Offset
	end := start
	if fn.Body != nil {
		end = fset.Position(fn.Body.Lbrace).Offset
	} else {
		end = fset.Position(fn.End()).Offset
	}
	if end > len(src) {
		end = len(src)
	}
	return strings.TrimSpace(string(src[start:end]))
}

// interfaceMethods returns sorted "MethodName(params) returns" strings.
func interfaceMethods(iface *ast.InterfaceType, src []byte, fset *token.FileSet) []string {
	var out []string
	for _, m := range iface.Methods.List {
		if len(m.Names) == 0 {
			// Embedded interface
			out = append(out, "~"+srcSlice(src, fset, m.Type.Pos(), m.Type.End()))
			continue
		}
		for _, name := range m.Names {
			typText := srcSlice(src, fset, m.Type.Pos(), m.Type.End())
			out = append(out, name.Name+typText)
		}
	}
	sort.Strings(out)
	return out
}

// structFields returns sorted "FieldName Type" strings (and embedded types).
func structFields(st *ast.StructType, src []byte, fset *token.FileSet) []string {
	var out []string
	for _, f := range st.Fields.List {
		typText := srcSlice(src, fset, f.Type.Pos(), f.Type.End())
		if len(f.Names) == 0 {
			out = append(out, "~"+typText) // embedded
			continue
		}
		for _, name := range f.Names {
			out = append(out, name.Name+" "+typText)
		}
	}
	sort.Strings(out)
	return out
}

func srcSlice(src []byte, fset *token.FileSet, from, to token.Pos) string {
	start := fset.Position(from).Offset
	end := fset.Position(to).Offset
	if start < 0 || end > len(src) || start >= end {
		return ""
	}
	return strings.TrimSpace(string(src[start:end]))
}

func kindOf(recv string) string {
	if recv != "" {
		return "method"
	}
	return "function"
}

// ---------------------------------------------------------------------------
// Change classification
// ---------------------------------------------------------------------------

func classifyChange(symbol string, old, new_ *Declaration, filepath string) DeclChange {
	pkg := ""
	kind := "unknown"
	exported := false
	if new_ != nil {
		pkg, kind, exported = new_.Package, new_.Kind, new_.IsExported
	} else if old != nil {
		pkg, kind, exported = old.Package, old.Kind, old.IsExported
	}

	ch := DeclChange{
		File: filepath, Symbol: symbol, Package: pkg,
		Kind: kind, Exported: exported, ChangeType: "body_change",
		Risk: "low",
	}
	if old != nil {
		ch.OldSignature = old.Signature
	}
	if new_ != nil {
		ch.NewSignature = new_.Signature
	}

	// --- Removed ---
	if old != nil && new_ == nil {
		ch.ChangeType = "removed"
		if exported {
			ch.Risk = "high"
			ch.Reason = fmt.Sprintf("`%s` removed. All callers, implementors, and type users will fail to compile.", symbol)
		} else {
			ch.Risk = "medium"
			ch.Reason = fmt.Sprintf("`%s` removed (unexported). Only callers within package %q are affected.", symbol, pkg)
		}
		return ch
	}

	// --- Added ---
	if old == nil && new_ != nil {
		ch.ChangeType = "added"
		ch.Risk = "low"
		ch.Reason = fmt.Sprintf("`%s` added. No existing callers to break.", symbol)
		return ch
	}

	// --- Both exist: compare ---

	// Export status change
	if old.IsExported != new_.IsExported {
		ch.ChangeType = "export_change"
		if old.IsExported && !new_.IsExported {
			ch.Risk = "high"
			ch.Reason = fmt.Sprintf("`%s` was exported, now unexported. All external package callers will fail to compile.", symbol)
		} else {
			ch.Risk = "medium"
			ch.Reason = fmt.Sprintf("`%s` was unexported, now exported. No breakage, but it is now part of the public API.", symbol)
		}
		return ch
	}

	switch kind {

	case "interface":
		return classifyInterfaceChange(ch, symbol, old, new_, exported)

	case "struct":
		return classifyStructChange(ch, symbol, old, new_, exported)

	case "function", "method":
		return classifyFuncChange(ch, symbol, old, new_, exported)

	case "type", "type_alias":
		if old.Signature != new_.Signature {
			ch.ChangeType = "type_change"
			wasAlias := strings.Contains(old.Signature, " = ")
			isAlias := strings.Contains(new_.Signature, " = ")
			switch {
			case wasAlias && !isAlias:
				ch.Risk = "high"
				ch.Reason = "Type alias removed — now a defined type. The two types are no longer interchangeable; explicit conversions now required everywhere they were previously mixed."
			case !wasAlias && isAlias:
				ch.Risk = "medium"
				ch.Reason = "Defined type changed to alias. Usually backward compatible but changes type identity and method sets."
			case old.HasGenerics != new_.HasGenerics:
				ch.Risk = "high"
				ch.Reason = "Generic type parameters added or removed. All instantiation sites must be updated."
			default:
				if exported {
					ch.Risk = "high"
					ch.Reason = "Underlying type changed. Code that assumed the old type's size, layout, method set, or conversions may break or silently misbehave."
				} else {
					ch.Risk = "medium"
					ch.Reason = "Unexported type definition changed. Internal package code that relies on the old type must be updated."
				}
			}
		}
		return ch

	case "const", "var":
		if old.Signature != new_.Signature {
			ch.ChangeType = "body_change"
			if exported {
				ch.Risk = "medium"
				ch.Reason = fmt.Sprintf("`%s` value or type changed. Code using it as a sentinel, map key, switch case, or iota-derived value may silently misbehave.", symbol)
			} else {
				ch.Risk = "low"
				ch.Reason = fmt.Sprintf("`%s` (unexported) value or type changed.", symbol)
			}
		}
		return ch
	}

	// Fallback: signatures differ but we can't classify precisely
	if old.Signature != new_.Signature {
		ch.Risk = "medium"
		ch.Reason = "Declaration changed; manual review recommended."
	}
	return ch
}

func classifyInterfaceChange(ch DeclChange, symbol string, old, new_ *Declaration, exported bool) DeclChange {
	oldSet := sliceToSet(old.Fields)
	newSet := sliceToSet(new_.Fields)

	var added, removed []string
	for m := range newSet {
		if !oldSet[m] {
			added = append(added, m)
		}
	}
	for m := range oldSet {
		if !newSet[m] {
			removed = append(removed, m)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)

	ch.AddedFields = added
	ch.RemovedFields = removed
	ch.ChangeType = "interface_change"

	if len(added) > 0 {
		if exported {
			ch.Risk = "high"
		} else {
			ch.Risk = "medium"
		}
		ch.Reason = fmt.Sprintf(
			"Method(s) added to interface %q: %v. Every type that implements this interface must now also implement the new method(s) — existing implementations will not compile.",
			symbol, added,
		)
	} else if len(removed) > 0 {
		ch.Risk = "low"
		ch.Reason = fmt.Sprintf(
			"Method(s) removed from interface %q: %v. Existing implementations still satisfy the interface, but callers that invoke these methods through the interface type will fail to compile.",
			symbol, removed,
		)
	} else {
		ch.Risk = "low"
		ch.Reason = "Interface body changed (comment or formatting only)."
	}
	return ch
}

func classifyStructChange(ch DeclChange, symbol string, old, new_ *Declaration, exported bool) DeclChange {
	oldSet := sliceToSet(old.Fields)
	newSet := sliceToSet(new_.Fields)

	var added, removed []string
	for f := range newSet {
		if !oldSet[f] {
			added = append(added, f)
		}
	}
	for f := range oldSet {
		if !newSet[f] {
			removed = append(removed, f)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)

	ch.AddedFields = added
	ch.RemovedFields = removed
	ch.ChangeType = "struct_change"

	if len(removed) > 0 {
		if exported {
			ch.Risk = "high"
		} else {
			ch.Risk = "medium"
		}
		ch.Reason = fmt.Sprintf(
			"Fields removed from struct %q: %v. Any code accessing these fields will not compile. Positional struct literals listing all fields will also break.",
			symbol, removed,
		)
	} else if len(added) > 0 {
		if exported {
			ch.Risk = "medium"
		} else {
			ch.Risk = "low"
		}
		ch.Reason = fmt.Sprintf(
			"Fields added to struct %q: %v. Keyed literals ({Field: val}) are backward compatible. Positional literals will not compile.",
			symbol, added,
		)
	} else if old.Signature != new_.Signature {
		ch.Risk = "medium"
		ch.Reason = fmt.Sprintf("Struct %q field types or tags changed — may break JSON/DB serialization, type assertions, or cgo interop.", symbol)
	} else {
		ch.Risk = "low"
		ch.Reason = "Struct changed (comment or formatting only)."
	}
	return ch
}

func classifyFuncChange(ch DeclChange, symbol string, old, new_ *Declaration, exported bool) DeclChange {
	if old.Signature == new_.Signature {
		ch.Risk = "low"
		ch.Reason = "Internal implementation changed. Signature stable."
		return ch
	}

	ch.ChangeType = "signature_change"

	// Receiver type or pointer change
	if old.Receiver != new_.Receiver {
		ch.Risk = "high"
		ch.Reason = fmt.Sprintf(
			"Method receiver type changed: `%s` → `%s`. The method no longer belongs to the old type — all existing call sites will fail to compile.",
			old.Receiver, new_.Receiver,
		)
		return ch
	}
	if old.IsPtrReceiver != new_.IsPtrReceiver {
		direction := "value→pointer"
		if !new_.IsPtrReceiver {
			direction = "pointer→value"
		}
		ch.Risk = "high"
		ch.Reason = fmt.Sprintf(
			"Receiver changed (%s). Pointer-receiver methods are only in *T's method set; value-receiver methods are in both T and *T. This can silently break interface satisfaction — existing variables of type T (non-pointer) may no longer satisfy interfaces.",
			direction,
		)
		return ch
	}

	// Generic type params changed
	if old.HasGenerics != new_.HasGenerics {
		ch.Risk = "high"
		ch.Reason = "Generic type parameters added or removed. All call sites that instantiate this function must be updated."
		return ch
	}

	// Classify by comparing param / return portions of the signature
	oldParams, oldReturns := splitSig(old.Signature)
	newParams, newReturns := splitSig(new_.Signature)

	if oldReturns != newReturns {
		if exported {
			ch.Risk = "high"
		} else {
			ch.Risk = "medium"
		}
		ch.Reason = fmt.Sprintf(
			"Return type changed: `%s` → `%s`. All call sites that use the return value must be updated.",
			oldReturns, newReturns,
		)
		return ch
	}

	oldCount := countParams(oldParams)
	newCount := countParams(newParams)

	if newCount < oldCount {
		if exported {
			ch.Risk = "high"
		} else {
			ch.Risk = "medium"
		}
		ch.Reason = fmt.Sprintf(
			"Parameters removed. Old: %s → New: %s. Callers passing the removed arguments will not compile.",
			oldParams, newParams,
		)
	} else if newCount > oldCount {
		if strings.Contains(newParams, "...") {
			ch.Risk = "low"
			ch.Reason = "Variadic parameter added. Backward compatible at all existing call sites."
		} else {
			if exported {
				ch.Risk = "high"
			} else {
				ch.Risk = "medium"
			}
			ch.Reason = fmt.Sprintf(
				"New required parameters added. Old: %s → New: %s. All existing call sites must pass the new arguments.",
				oldParams, newParams,
			)
		}
	} else {
		// Same count, types or names changed
		if exported {
			ch.Risk = "medium"
		} else {
			ch.Risk = "low"
		}
		ch.Reason = fmt.Sprintf(
			"Parameter types or names changed. Old: %s → New: %s. Type changes will cause compile errors; name-only changes are safe.",
			oldParams, newParams,
		)
	}
	return ch
}

// splitSig returns (params_text, returns_text) from a func signature string.
func splitSig(sig string) (string, string) {
	// Find the closing paren of the first param list
	depth := 0
	inParams := false
	for i, ch := range sig {
		if ch == '(' && !inParams {
			inParams = true
			depth = 1
			continue
		}
		if inParams {
			if ch == '(' {
				depth++
			} else if ch == ')' {
				depth--
				if depth == 0 {
					return sig[:i+1], strings.TrimSpace(sig[i+1:])
				}
			}
		}
	}
	return sig, ""
}

func countParams(params string) int {
	inner := strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(params), ")"), "(")
	inner = strings.TrimSpace(inner)
	if inner == "" {
		return 0
	}
	return strings.Count(inner, ",")+1
}

// ---------------------------------------------------------------------------
// go.mod / go.sum analysis
// ---------------------------------------------------------------------------

func analyzeGoMod(fd FileDiff) []DeclChange {
	var changes []DeclChange

	var removedLines, addedLines []string
	for _, h := range fd.Hunks {
		for _, line := range h.Lines {
			if strings.HasPrefix(line, "-") {
				removedLines = append(removedLines, strings.TrimPrefix(line, "-"))
			} else if strings.HasPrefix(line, "+") {
				addedLines = append(addedLines, strings.TrimPrefix(line, "+"))
			}
		}
	}

	type modEntry struct{ path, version string }
	parseEntry := func(line string) (modEntry, bool) {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "module "):
			return modEntry{path: "module", version: strings.TrimPrefix(line, "module ")}, true
		case strings.HasPrefix(line, "go "):
			return modEntry{path: "go_version", version: strings.TrimPrefix(line, "go ")}, true
		case strings.HasPrefix(line, "require "):
			p := strings.Fields(strings.TrimPrefix(line, "require "))
			if len(p) >= 2 {
				return modEntry{path: p[0], version: p[1]}, true
			}
		default:
			p := strings.Fields(line)
			if len(p) >= 2 && strings.Contains(p[1], ".") {
				return modEntry{path: p[0], version: p[1]}, true
			}
		}
		return modEntry{}, false
	}

	oldDeps := make(map[string]string)
	newDeps := make(map[string]string)
	for _, l := range removedLines {
		if e, ok := parseEntry(l); ok {
			oldDeps[e.path] = e.version
		}
	}
	for _, l := range addedLines {
		if e, ok := parseEntry(l); ok {
			newDeps[e.path] = e.version
		}
	}

	allKeys := make(map[string]bool)
	for k := range oldDeps {
		allKeys[k] = true
	}
	for k := range newDeps {
		allKeys[k] = true
	}

	for key := range allKeys {
		ov, nv := oldDeps[key], newDeps[key]
		if ov == nv {
			continue
		}

		dc := DeclChange{File: fd.Path, Symbol: key, Package: "go.mod", Kind: "mod", Exported: true, ChangeType: "mod_change"}

		switch key {
		case "module":
			dc.OldSignature = "module " + ov
			dc.NewSignature = "module " + nv
			dc.Risk = "high"
			dc.Reason = fmt.Sprintf("Module path changed: %s → %s. ALL import paths in every consumer must be updated. This is a major breaking change requiring a new major version per semver.", ov, nv)

		case "go_version":
			dc.OldSignature = "go " + ov
			dc.NewSignature = "go " + nv
			if versionLT(nv, ov) {
				dc.Risk = "high"
				dc.Reason = fmt.Sprintf("Go version downgraded: %s → %s. Language features and stdlib APIs available in %s may not exist in %s.", ov, nv, ov, nv)
			} else {
				dc.Risk = "medium"
				dc.Reason = fmt.Sprintf("Go version bumped: %s → %s. All contributors need toolchain ≥ %s. Some language semantics may change (e.g. loop variable capture in 1.22).", ov, nv, nv)
			}

		default:
			if ov == "" {
				dc.NewSignature = "require " + key + " " + nv
				dc.Risk = "low"
				dc.Reason = fmt.Sprintf("New dependency added: %s %s.", key, nv)
			} else if nv == "" {
				dc.OldSignature = "require " + key + " " + ov
				dc.Risk = "high"
				dc.Reason = fmt.Sprintf("Dependency removed: %s. Code importing it will fail to compile.", key)
			} else {
				dc.OldSignature = "require " + key + " " + ov
				dc.NewSignature = "require " + key + " " + nv
				if majorVersion(ov) != majorVersion(nv) {
					dc.Risk = "high"
					dc.Reason = fmt.Sprintf("Dependency %s MAJOR version bump: %s → %s. Major version changes are explicitly breaking per semver — import paths change.", key, ov, nv)
				} else {
					dc.Risk = "medium"
					dc.Reason = fmt.Sprintf("Dependency %s version changed: %s → %s. Review that module's changelog for breaking changes in this range.", key, ov, nv)
				}
			}
		}
		changes = append(changes, dc)
	}
	return changes
}

func versionLT(a, b string) bool {
	partsA := versionParts(a)
	partsB := versionParts(b)
	for i := 0; i < 3; i++ {
		if partsA[i] != partsB[i] {
			return partsA[i] < partsB[i]
		}
	}
	return false
}

func versionParts(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	parts := strings.Split(v, ".")
	var out [3]int
	for i := 0; i < 3 && i < len(parts); i++ {
		n, _ := strconv.Atoi(parts[i])
		out[i] = n
	}
	return out
}

func majorVersion(v string) string {
	v = strings.TrimPrefix(v, "v")
	return strings.Split(v, ".")[0]
}

// ---------------------------------------------------------------------------
// Per-file analysis
// ---------------------------------------------------------------------------

func analyzeFile(fd FileDiff, repoRoot, baseRef string) []DeclChange {
	path := fd.Path

	// go.mod / go.sum
	if path == "go.mod" || path == "go.sum" || strings.HasSuffix(path, "/go.mod") {
		return analyzeGoMod(fd)
	}
	if !strings.HasSuffix(path, ".go") {
		return nil
	}
	// Skip vendor directory
	if strings.HasPrefix(path, "vendor/") || strings.Contains(path, "/vendor/") {
		return nil
	}

	if fd.IsDeleted {
		return []DeclChange{{
			File: path, Symbol: "<entire file>", Kind: "file", Exported: true,
			ChangeType: "removed", Risk: "high",
			Reason: "Entire Go file deleted. All symbols it defined are removed; all importers will fail to compile.",
		}}
	}
	if fd.IsNew {
		return []DeclChange{{
			File: path, Symbol: "<new file>", Kind: "file",
			ChangeType: "added", Risk: "low",
			Reason: "New Go file. No existing callers to break.",
		}}
	}

	var changes []DeclChange
	if fd.IsRename && fd.OldPath != "" {
		changes = append(changes, DeclChange{
			File: path, Symbol: "<file rename>", Kind: "file", Exported: true,
			ChangeType: "rename",
			OldSignature: fd.OldPath, NewSignature: path,
			Risk: "high",
			Reason: fmt.Sprintf("File renamed: %s → %s. If the package path changed, all import statements must be updated.", fd.OldPath, path),
		})
	}

	// Read new source
	abs := filepath.Join(repoRoot, path)
	newSrc, err := os.ReadFile(abs)
	if err != nil {
		newSrc = nil
	}

	// Get old source
	var oldSrc []byte
	if baseRef != "" {
		if out := getFileAtRef(repoRoot, path, baseRef); out != "" {
			oldSrc = []byte(out)
		}
	} else if newSrc != nil {
		oldSrc = reconstructOld(newSrc, fd)
	}

	// Check for generated file
	if len(newSrc) > 0 && strings.Contains(string(newSrc[:min(500, len(newSrc))]), "Code generated") {
		return append(changes, DeclChange{
			File: path, Symbol: "<generated>", Kind: "file",
			ChangeType: "body_change", Risk: "low",
			Reason: "Generated file changed. Review the generator template, not this file directly.",
		})
	}

	oldDecls := extractDeclarations(oldSrc)
	newDecls := extractDeclarations(newSrc)

	// Collect all symbol names
	allSymbols := make(map[string]bool)
	for k := range oldDecls {
		allSymbols[k] = true
	}
	for k := range newDecls {
		allSymbols[k] = true
	}

	// Determine which symbols are near hunk lines (optimisation)
	touched := touchedSymbols(fd.Hunks, newDecls, oldDecls)

	for symbol := range allSymbols {
		if len(touched) > 0 && !touched[symbol] {
			continue
		}
		od, hasOld := oldDecls[symbol]
		nd, hasNew := newDecls[symbol]

		var oldPtr, newPtr *Declaration
		if hasOld {
			d := od
			oldPtr = &d
		}
		if hasNew {
			d := nd
			newPtr = &d
		}

		// Skip truly unchanged
		if hasOld && hasNew {
			if od.Signature == nd.Signature && eqSlices(od.Fields, nd.Fields) {
				continue
			}
		}

		ch := classifyChange(symbol, oldPtr, newPtr, path)
		changes = append(changes, ch)
	}

	if len(changes) == 0 {
		// Body-only change
		pkg := ""
		if f, err := parser.ParseFile(token.NewFileSet(), "", newSrc, 0); err == nil && f.Name != nil {
			pkg = f.Name.Name
		}
		changes = append(changes, DeclChange{
			File: path, Symbol: "<body change>", Package: pkg, Kind: "unknown",
			ChangeType: "body_change", Risk: "low",
			Reason: "Lines changed inside function/method bodies. No interface or signature changes detected.",
		})
	}

	return changes
}

// touchedSymbols returns the set of declaration names whose source lines
// overlap with the diff hunks (with a small context window).
func touchedSymbols(hunks []Hunk, newDecls, oldDecls map[string]Declaration) map[string]bool {
	touched := make(map[string]bool)
	for _, h := range hunks {
		rangeStart := h.NewStart - 5
		rangeEnd := h.NewStart + h.NewCount + 5
		for name := range newDecls {
			touched[name] = true // simplification: mark all if we have hunks
			_ = rangeStart
			_ = rangeEnd
		}
		for name := range oldDecls {
			touched[name] = true
		}
	}
	return touched
}

// reconstructOld inverts the hunk lines to approximate the pre-patch content.
func reconstructOld(newSrc []byte, fd FileDiff) []byte {
	lines := strings.Split(string(newSrc), "\n")
	// Apply hunks in reverse order
	for i := len(fd.Hunks) - 1; i >= 0; i-- {
		h := fd.Hunks[i]
		start := h.NewStart - 1
		if start < 0 {
			start = 0
		}
		end := start + h.NewCount
		if end > len(lines) {
			end = len(lines)
		}
		var oldLines []string
		for _, dl := range h.Lines {
			if strings.HasPrefix(dl, "-") || (!strings.HasPrefix(dl, "+") && !strings.HasPrefix(dl, "\\")) {
				oldLines = append(oldLines, strings.TrimPrefix(dl, "-"))
			}
		}
		newLines := make([]string, 0, len(lines)-h.NewCount+len(oldLines))
		newLines = append(newLines, lines[:start]...)
		newLines = append(newLines, oldLines...)
		newLines = append(newLines, lines[end:]...)
		lines = newLines
	}
	return []byte(strings.Join(lines, "\n"))
}

// ---------------------------------------------------------------------------
// Utilities
// ---------------------------------------------------------------------------

func sliceToSet(s []string) map[string]bool {
	m := make(map[string]bool, len(s))
	for _, v := range s {
		m[v] = true
	}
	return m
}

func eqSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func filterNonEmpty(ss []string) []string {
	var out []string
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func atoidef(s string, def int) int {
	if s == "" {
		return def
	}
	return atoi(s)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

// ---------------------------------------------------------------------------
// Output
// ---------------------------------------------------------------------------

func buildResult(changes []DeclChange, changedFiles []string, goModChanged bool) AnalysisResult {
	s := Summary{
		FilesChanged:        len(changedFiles),
		DeclarationsChanged: len(changes),
		GoModChanged:        goModChanged,
	}
	for _, c := range changes {
		switch c.Risk {
		case "high":
			s.HighRisk++
		case "medium":
			s.MediumRisk++
		default:
			s.LowRisk++
		}
	}
	if s.HighRisk > 0 {
		s.OverallRisk = "high"
	} else if s.MediumRisk > 0 {
		s.OverallRisk = "medium"
	} else {
		s.OverallRisk = "low"
	}
	return AnalysisResult{Summary: s, ChangedFiles: changedFiles, ChangedDeclarations: changes}
}

func printReport(changes []DeclChange, result AnalysisResult) {
	s := result.Summary
	fmt.Printf("\n%s\n", strings.Repeat("=", 60))
	fmt.Printf("  Go Code Review — Change Analysis\n")
	fmt.Printf("%s\n", strings.Repeat("=", 60))
	fmt.Printf("  Files changed        : %d\n", s.FilesChanged)
	fmt.Printf("  Declarations changed : %d\n", s.DeclarationsChanged)
	goMod := "no"
	if s.GoModChanged {
		goMod = "YES ⚠️"
	}
	fmt.Printf("  go.mod changed       : %s\n", goMod)
	fmt.Printf("  Overall risk         : %s\n", strings.ToUpper(s.OverallRisk))
	fmt.Printf("    🔴 High            : %d\n", s.HighRisk)
	fmt.Printf("    🟡 Medium          : %d\n", s.MediumRisk)
	fmt.Printf("    🟢 Low             : %d\n", s.LowRisk)
	fmt.Printf("%s\n\n", strings.Repeat("=", 60))

	// Sort by risk
	sort.Slice(changes, func(i, j int) bool {
		order := map[string]int{"high": 0, "medium": 1, "low": 2}
		return order[changes[i].Risk] < order[changes[j].Risk]
	})

	icons := map[string]string{"high": "🔴", "medium": "🟡", "low": "🟢"}
	for _, c := range changes {
		exp := "(exported)"
		if !c.Exported {
			exp = "(unexported)"
		}
		fmt.Printf("%s [%-6s] %s :: %s %s\n", icons[c.Risk], strings.ToUpper(c.Risk), c.File, c.Symbol, exp)
		fmt.Printf("         change : %s\n", c.ChangeType)
		if c.OldSignature != "" {
			fmt.Printf("         old    : %s\n", c.OldSignature)
		}
		if c.NewSignature != "" {
			fmt.Printf("         new    : %s\n", c.NewSignature)
		}
		if len(c.RemovedFields) > 0 {
			fmt.Printf("         removed: %v\n", c.RemovedFields)
		}
		if len(c.AddedFields) > 0 {
			fmt.Printf("         added  : %v\n", c.AddedFields)
		}
		fmt.Printf("         reason : %s\n\n", c.Reason)
	}
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	repoRoot := flag.String("repo-root", ".", "Path to repo root (where go.mod lives)")
	baseRef  := flag.String("base-ref",  "", "Base git ref to diff against (omit = auto-detect)")
	diffFile := flag.String("diff-file", "", "Path to a pre-existing .diff file")
	output   := flag.String("output",    "", "Write JSON results to this path")
	quiet    := flag.Bool("quiet",       false, "Suppress human-readable output; emit JSON only")
	flag.Parse()

	root := *repoRoot
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}

	diffText := getDiff(root, *baseRef, *diffFile)
	if strings.TrimSpace(diffText) == "" {
		fmt.Fprintln(os.Stderr, "No Go changes detected.")
		result := AnalysisResult{
			Summary:             Summary{OverallRisk: "none"},
			ChangedFiles:        []string{},
			ChangedDeclarations: []DeclChange{},
		}
		enc, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(enc))
		return
	}

	fileDiffs := parseDiff(diffText)
	var changedFiles []string
	goModChanged := false
	var allChanges []DeclChange

	for _, fd := range fileDiffs {
		changedFiles = append(changedFiles, fd.Path)
		if fd.Path == "go.mod" || strings.HasSuffix(fd.Path, "/go.mod") {
			goModChanged = true
		}
		changes := analyzeFile(fd, root, *baseRef)
		allChanges = append(allChanges, changes...)
	}

	result := buildResult(allChanges, changedFiles, goModChanged)

	if !*quiet {
		printReport(allChanges, result)
	}

	enc, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fatalf("marshalling JSON: %v", err)
	}
	jsonStr := string(enc)

	if *output != "" {
		if err := os.WriteFile(*output, enc, 0644); err != nil {
			fatalf("writing output: %v", err)
		}
	}
	fmt.Println(jsonStr)
}

// trace_callers.go — Go Code Review: Reverse Call Graph & Implementation Finder
//
// Reads the JSON produced by analyze_diff.go and walks the entire Go source
// tree to find, for each changed symbol:
//
//   - Direct callers (functions/methods that call it)
//   - Importers (files that import the changed package)
//   - Interface implementors (types that implement a changed interface)
//   - Struct users (files that access changed struct fields)
//   - Type users (variables/params declared with the changed type)
//
// Usage:
//
//	# Pipe from analyze_diff.go (most common)
//	go run analyze_diff.go --repo-root . | go run trace_callers.go --repo-root .
//
//	# Use a saved JSON file
//	go run trace_callers.go --repo-root . --changes-json /tmp/changes.json
//
//	# Write impact JSON to file
//	go run trace_callers.go --repo-root . --changes-json /tmp/changes.json --output /tmp/impact.json
//
// Requires: Go 1.18+. No external dependencies.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Input types (mirrors analyze_diff.go output)
// ---------------------------------------------------------------------------

type AnalysisResult struct {
	Summary             Summary       `json:"summary"`
	ChangedFiles        []string      `json:"changed_files"`
	ChangedDeclarations []DeclChange  `json:"changed_declarations"`
}

type Summary struct {
	FilesChanged        int    `json:"files_changed"`
	DeclarationsChanged int    `json:"declarations_changed"`
	HighRisk            int    `json:"high_risk"`
	MediumRisk          int    `json:"medium_risk"`
	LowRisk             int    `json:"low_risk"`
	OverallRisk         string `json:"overall_risk"`
	GoModChanged        bool   `json:"go_mod_changed"`
}

type DeclChange struct {
	File         string   `json:"file"`
	Symbol       string   `json:"symbol"`
	Package      string   `json:"package"`
	Kind         string   `json:"kind"`
	Exported     bool     `json:"exported"`
	ChangeType   string   `json:"change_type"`
	OldSignature string   `json:"old_signature,omitempty"`
	NewSignature string   `json:"new_signature,omitempty"`
	RemovedFields []string `json:"removed_fields,omitempty"`
	AddedFields  []string `json:"added_fields,omitempty"`
	Risk         string   `json:"risk"`
	Reason       string   `json:"reason"`
}

// ---------------------------------------------------------------------------
// Output types
// ---------------------------------------------------------------------------

type CallSite struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Snippet  string `json:"snippet"`
	CallType string `json:"call_type"` // direct_call | method_call | import | implements | embeds | type_use
}

type SymbolImpact struct {
	Symbol          string     `json:"symbol"`
	File            string     `json:"file"`
	Risk            string     `json:"risk"`
	CallSites       []CallSite `json:"call_sites"`
	Importers       []string   `json:"importers"`
	Implementors    []string   `json:"implementors"` // types that implement a changed interface
	Embedders       []string   `json:"embedders"`    // structs that embed a changed type
	TestCoverage    bool       `json:"test_coverage"`
	RiskEscalation  string     `json:"risk_escalation"`
}

type ImpactResult struct {
	Summary   ImpactSummary            `json:"summary"`
	ImpactMap map[string]*SymbolImpact `json:"impact_map"`
}

type ImpactSummary struct {
	SymbolsTracked          int `json:"symbols_tracked"`
	TotalCallSites          int `json:"total_call_sites"`
	TotalImporters          int `json:"total_importers"`
	TotalImplementors       int `json:"total_implementors"`
	SymbolsWithNoTestCoverage int `json:"symbols_with_no_test_coverage"`
}

// ---------------------------------------------------------------------------
// File walker
// ---------------------------------------------------------------------------

var ignoredDirs = map[string]bool{
	".git": true, ".hg": true, "__pycache__": true,
	"vendor": true, "node_modules": true, "testdata": true,
}

func walkGoFiles(root string) []string {
	var files []string
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if ignoredDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			files = append(files, path)
		}
		return nil
	})
	return files
}

// ---------------------------------------------------------------------------
// Package index: map import path → files
// ---------------------------------------------------------------------------

// packageOfFile returns the Go package import path for a file, relative to
// the module root, by parsing the package declaration and directory.
func packageOfFile(repoRoot, filePath string) string {
	rel, err := filepath.Rel(repoRoot, filepath.Dir(filePath))
	if err != nil {
		return ""
	}
	// Normalize to forward slashes
	return filepath.ToSlash(rel)
}

// ---------------------------------------------------------------------------
// Visitor helpers
// ---------------------------------------------------------------------------

// sourceLines splits source into lines for snippet extraction.
func sourceLines(src []byte) []string {
	return strings.Split(string(src), "\n")
}

func snippet(lines []string, lineno int) string {
	if lineno < 1 || lineno > len(lines) {
		return ""
	}
	return strings.TrimSpace(lines[lineno-1])
}

// ---------------------------------------------------------------------------
// Core impact analysis per file
// ---------------------------------------------------------------------------

type fileImpact struct {
	callersBySymbol      map[string][]CallSite // symbol name → call sites in this file
	importsPackages      []string              // import paths found in this file
	implementsInterfaces map[string][]string   // interface name → type names that implement it
	embedsTypes          map[string][]string   // type name → struct names that embed it
}

func analyzeFileForImpact(
	filePath string,
	repoRoot string,
	targetSymbols map[string]bool,    // short names to look for
	targetInterfaces map[string][]string, // interface name → required methods
	targetPackagePaths map[string]bool,   // package relative paths that changed
) fileImpact {
	result := fileImpact{
		callersBySymbol:      make(map[string][]CallSite),
		implementsInterfaces: make(map[string][]string),
		embedsTypes:          make(map[string][]string),
	}

	src, err := os.ReadFile(filePath)
	if err != nil {
		return result
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filePath, src, 0)
	if err != nil && f == nil {
		return result
	}

	lines := sourceLines(src)
	rel, _ := filepath.Rel(repoRoot, filePath)
	rel = filepath.ToSlash(rel)

	// --- Collect imports ---
	for _, imp := range f.Imports {
		if imp.Path != nil {
			importPath := strings.Trim(imp.Path.Value, `"`)
			result.importsPackages = append(result.importsPackages, importPath)
		}
	}

	// --- Walk AST for call sites and type uses ---
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			return false
		}

		pos := fset.Position(n.Pos())
		line := pos.Line

		switch node := n.(type) {

		// Function/method calls
		case *ast.CallExpr:
			var calledName string
			switch fn := node.Fun.(type) {
			case *ast.Ident:
				calledName = fn.Name
			case *ast.SelectorExpr:
				calledName = fn.Sel.Name
			}
			if calledName != "" && targetSymbols[calledName] {
				result.callersBySymbol[calledName] = append(
					result.callersBySymbol[calledName],
					CallSite{
						File:     rel,
						Line:     line,
						Snippet:  snippet(lines, line),
						CallType: "direct_call",
					},
				)
			}

		// Type declarations — check for interface implementation and struct embedding
		case *ast.TypeSpec:
			typeName := node.Name.Name

			// Check struct embedding
			if st, ok := node.Type.(*ast.StructType); ok {
				for _, field := range st.Fields.List {
					if len(field.Names) == 0 { // embedded field
						embeddedName := ""
						switch et := field.Type.(type) {
						case *ast.Ident:
							embeddedName = et.Name
						case *ast.StarExpr:
							if id, ok := et.X.(*ast.Ident); ok {
								embeddedName = id.Name
							}
						case *ast.SelectorExpr:
							embeddedName = et.Sel.Name
						}
						if embeddedName != "" && targetSymbols[embeddedName] {
							result.embedsTypes[embeddedName] = append(
								result.embedsTypes[embeddedName],
								typeName,
							)
						}
					}
				}
			}

			// Check interface implementation (best-effort: match method names)
			if iface, ok := node.Type.(*ast.InterfaceType); ok {
				_ = iface // this type IS an interface — not an implementor
			}

		// Method declarations — check if this type implements a target interface
		case *ast.FuncDecl:
			if node.Recv == nil || len(node.Recv.List) == 0 {
				return true
			}
			// Get receiver type name
			recvTypeName := ""
			switch rt := node.Recv.List[0].Type.(type) {
			case *ast.StarExpr:
				if id, ok := rt.X.(*ast.Ident); ok {
					recvTypeName = id.Name
				}
			case *ast.Ident:
				recvTypeName = rt.Name
			}
			methodName := node.Name.Name

			// For each target interface, record that this type provides one of its methods
			for ifaceName, requiredMethods := range targetInterfaces {
				for _, required := range requiredMethods {
					// required is like "MethodName(params) returns" — just match the name
					reqMethodName := strings.SplitN(required, "(", 2)[0]
					if reqMethodName == methodName {
						result.implementsInterfaces[ifaceName] = append(
							result.implementsInterfaces[ifaceName],
							fmt.Sprintf("%s:%d::%s", rel, line, recvTypeName),
						)
					}
				}
			}

		// Variable and field type references
		case *ast.Field:
			typeName := exprName(node.Type)
			if typeName != "" && targetSymbols[typeName] {
				result.callersBySymbol[typeName] = append(
					result.callersBySymbol[typeName],
					CallSite{
						File:     rel,
						Line:     line,
						Snippet:  snippet(lines, line),
						CallType: "type_use",
					},
				)
			}
		}

		return true
	})

	return result
}

// exprName extracts the base type name from a type expression.
func exprName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return exprName(e.X)
	case *ast.SelectorExpr:
		return e.Sel.Name
	case *ast.ArrayType:
		return exprName(e.Elt)
	case *ast.MapType:
		return exprName(e.Value)
	}
	return ""
}

// ---------------------------------------------------------------------------
// Main tracing logic
// ---------------------------------------------------------------------------

func traceImpact(analysis AnalysisResult, repoRoot string) ImpactResult {
	// Build target data structures
	impactMap := make(map[string]*SymbolImpact)
	targetSymbols := make(map[string]bool)
	targetInterfaces := make(map[string][]string) // interface name → method names
	targetPackagePaths := make(map[string]bool)
	changedFilesSet := make(map[string]bool)

	for _, f := range analysis.ChangedFiles {
		changedFilesSet[f] = true
		pkgPath := filepath.ToSlash(filepath.Dir(f))
		if pkgPath == "." {
			pkgPath = ""
		}
		targetPackagePaths[pkgPath] = true
	}

	for _, dc := range analysis.ChangedDeclarations {
		if strings.HasPrefix(dc.Symbol, "<") {
			continue // skip synthetic entries
		}
		key := dc.File + "::" + dc.Symbol
		impactMap[key] = &SymbolImpact{
			Symbol: key,
			File:   dc.File,
			Risk:   dc.Risk,
		}

		// Short name (MethodName from ReceiverType.MethodName)
		shortName := dc.Symbol
		if idx := strings.LastIndex(dc.Symbol, "."); idx >= 0 {
			shortName = dc.Symbol[idx+1:]
		}
		targetSymbols[shortName] = true
		targetSymbols[dc.Symbol] = true

		// Track interface method sets for implementation finding
		if dc.Kind == "interface" {
			methods := dc.AddedFields
			if len(dc.RemovedFields) > 0 {
				methods = append(methods, dc.RemovedFields...)
			}
			if dc.OldSignature != "" {
				// extract method names from old signature if fields not populated
			}
			targetInterfaces[dc.Symbol] = methods
			targetInterfaces[shortName] = methods
		}
	}

	// Walk all Go files
	goFiles := walkGoFiles(repoRoot)

	for _, filePath := range goFiles {
		rel, _ := filepath.Rel(repoRoot, filePath)
		rel = filepath.ToSlash(rel)

		// Skip files that are themselves changed
		if changedFilesSet[rel] {
			continue
		}

		isTest := strings.HasSuffix(filePath, "_test.go") ||
			strings.Contains(filePath, "/testdata/")

		fi := analyzeFileForImpact(filePath, repoRoot, targetSymbols, targetInterfaces, targetPackagePaths)

		// Match call sites back to changed declarations
		for symbolName, sites := range fi.callersBySymbol {
			for key, impact := range impactMap {
				shortKey := impact.Symbol
				if idx := strings.LastIndex(shortKey, "::"); idx >= 0 {
					shortKey = shortKey[idx+2:]
				}
				shortSymbol := shortKey
				if idx := strings.LastIndex(shortKey, "."); idx >= 0 {
					shortSymbol = shortKey[idx+1:]
				}
				if symbolName == shortSymbol || symbolName == shortKey {
					impact.CallSites = append(impact.CallSites, sites...)
					if isTest {
						impact.TestCoverage = true
					}
					_ = key
				}
			}
		}

		// Match importers
		pkgPath := packageOfFile(repoRoot, filePath)
		for _, importedPkg := range fi.importsPackages {
			for _, impact := range impactMap {
				changedPkg := filepath.ToSlash(filepath.Dir(impact.File))
				if changedPkg == "." {
					changedPkg = ""
				}
				if strings.HasSuffix(importedPkg, changedPkg) || importedPkg == changedPkg {
					if !containsStr(impact.Importers, rel) {
						impact.Importers = append(impact.Importers, rel)
					}
					if isTest {
						impact.TestCoverage = true
					}
				}
			}
		}
		_ = pkgPath

		// Match interface implementors
		for ifaceName, typeNames := range fi.implementsInterfaces {
			for key, impact := range impactMap {
				shortKey := impact.Symbol
				if idx := strings.LastIndex(shortKey, "::"); idx >= 0 {
					shortKey = shortKey[idx+2:]
				}
				if ifaceName == shortKey || ifaceName == impact.Symbol {
					for _, typeName := range typeNames {
						if !containsStr(impact.Implementors, typeName) {
							impact.Implementors = append(impact.Implementors, typeName)
						}
					}
					_ = key
				}
			}
		}

		// Match struct embedders
		for typeName, embedderNames := range fi.embedsTypes {
			for _, impact := range impactMap {
				shortKey := impact.Symbol
				if idx := strings.LastIndex(shortKey, "::"); idx >= 0 {
					shortKey = shortKey[idx+2:]
				}
				if typeName == shortKey {
					for _, en := range embedderNames {
						entry := fmt.Sprintf("%s::%s", rel, en)
						if !containsStr(impact.Embedders, entry) {
							impact.Embedders = append(impact.Embedders, entry)
						}
					}
				}
			}
		}
	}

	// Compute risk escalation and de-duplicate
	for _, impact := range impactMap {
		// Deduplicate
		impact.CallSites = dedupCallSites(impact.CallSites)
		impact.Importers = dedupStrings(impact.Importers)
		impact.Implementors = dedupStrings(impact.Implementors)
		impact.Embedders = dedupStrings(impact.Embedders)

		// Risk escalation notes
		var notes []string
		if len(impact.Implementors) > 0 && impact.Risk == "high" {
			notes = append(notes, fmt.Sprintf(
				"%d existing implementor(s) found — all must add the new method(s)",
				len(impact.Implementors),
			))
		}
		if !impact.TestCoverage && (len(impact.CallSites) > 0 || len(impact.Importers) > 0) {
			notes = append(notes, fmt.Sprintf(
				"%d call site(s) found with no test coverage detected",
				len(impact.CallSites),
			))
		}
		if len(impact.CallSites) == 0 && len(impact.Importers) == 0 && impact.Risk == "high" {
			notes = append(notes, "No callers or importers found — may be dead code or dynamically loaded via reflect/plugin")
		}
		if len(notes) > 0 {
			impact.RiskEscalation = strings.Join(notes, "; ")
		} else {
			impact.RiskEscalation = "No additional escalation."
		}
	}

	// Build summary
	sum := ImpactSummary{SymbolsTracked: len(impactMap)}
	for _, impact := range impactMap {
		sum.TotalCallSites += len(impact.CallSites)
		sum.TotalImporters += len(impact.Importers)
		sum.TotalImplementors += len(impact.Implementors)
		if !impact.TestCoverage && (len(impact.CallSites) > 0 || len(impact.Importers) > 0) {
			sum.SymbolsWithNoTestCoverage++
		}
	}

	return ImpactResult{Summary: sum, ImpactMap: impactMap}
}

// ---------------------------------------------------------------------------
// Report rendering
// ---------------------------------------------------------------------------

func printImpactReport(result ImpactResult) {
	s := result.Summary
	fmt.Printf("\n%s\n", strings.Repeat("=", 60))
	fmt.Printf("  Go Code Review — Impact & Caller Analysis\n")
	fmt.Printf("%s\n", strings.Repeat("=", 60))
	fmt.Printf("  Symbols tracked          : %d\n", s.SymbolsTracked)
	fmt.Printf("  Total call sites         : %d\n", s.TotalCallSites)
	fmt.Printf("  Total importers          : %d\n", s.TotalImporters)
	fmt.Printf("  Total implementors       : %d\n", s.TotalImplementors)
	fmt.Printf("  Without test coverage    : %d\n", s.SymbolsWithNoTestCoverage)
	fmt.Printf("%s\n\n", strings.Repeat("=", 60))

	// Sort keys by risk
	keys := make([]string, 0, len(result.ImpactMap))
	for k := range result.ImpactMap {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		order := map[string]int{"high": 0, "medium": 1, "low": 2}
		ri := order[result.ImpactMap[keys[i]].Risk]
		rj := order[result.ImpactMap[keys[j]].Risk]
		if ri != rj {
			return ri < rj
		}
		return keys[i] < keys[j]
	})

	icons := map[string]string{"high": "🔴", "medium": "🟡", "low": "🟢"}
	for _, key := range keys {
		impact := result.ImpactMap[key]
		fmt.Printf("%s [%-6s] %s\n", icons[impact.Risk], strings.ToUpper(impact.Risk), key)

		if len(impact.CallSites) > 0 {
			shown := impact.CallSites
			if len(shown) > 8 {
				shown = shown[:8]
			}
			fmt.Printf("         callers (%d):\n", len(impact.CallSites))
			for _, cs := range shown {
				fmt.Printf("           • %s:%d  %s\n", cs.File, cs.Line, cs.Snippet)
			}
			if len(impact.CallSites) > 8 {
				fmt.Printf("           … and %d more\n", len(impact.CallSites)-8)
			}
		}

		if len(impact.Implementors) > 0 {
			fmt.Printf("         implementors (%d): %s\n",
				len(impact.Implementors),
				strings.Join(impact.Implementors[:min(3, len(impact.Implementors))], ", "),
			)
		}
		if len(impact.Embedders) > 0 {
			fmt.Printf("         embedders (%d): %s\n",
				len(impact.Embedders),
				strings.Join(impact.Embedders[:min(3, len(impact.Embedders))], ", "),
			)
		}
		if len(impact.Importers) > 0 {
			shown := impact.Importers
			if len(shown) > 5 {
				shown = shown[:5]
			}
			fmt.Printf("         importers (%d): %s\n", len(impact.Importers), strings.Join(shown, ", "))
		}

		cov := "✅ has test coverage"
		if !impact.TestCoverage {
			cov = "⚠️  no test coverage detected"
		}
		fmt.Printf("         coverage  : %s\n", cov)

		if impact.RiskEscalation != "" && impact.RiskEscalation != "No additional escalation." {
			fmt.Printf("         notes     : %s\n", impact.RiskEscalation)
		}
		fmt.Println()
	}
}

// ---------------------------------------------------------------------------
// Utilities
// ---------------------------------------------------------------------------

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func dedupStrings(ss []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func dedupCallSites(cs []CallSite) []CallSite {
	seen := make(map[string]bool)
	var out []CallSite
	for _, c := range cs {
		key := fmt.Sprintf("%s:%d", c.File, c.Line)
		if !seen[key] {
			seen[key] = true
			out = append(out, c)
		}
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	repoRoot    := flag.String("repo-root",    ".", "Path to repo root")
	changesJSON := flag.String("changes-json", "", "JSON file from analyze_diff.go --output")
	output      := flag.String("output",       "", "Write impact JSON to this path")
	quiet       := flag.Bool("quiet",          false, "Suppress human-readable output; emit JSON only")
	flag.Parse()

	root := *repoRoot
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}

	// Read input JSON
	var raw []byte
	var err error
	if *changesJSON != "" {
		raw, err = os.ReadFile(*changesJSON)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error reading changes JSON: %v\n", err)
			os.Exit(1)
		}
	} else {
		// Read from stdin; analyze_diff prints human text then JSON — find the JSON
		allIn, err := io.ReadAll(os.Stdin)
		if err != nil || len(allIn) == 0 {
			fmt.Fprintln(os.Stderr, "provide --changes-json or pipe from analyze_diff.go")
			os.Exit(1)
		}
		// Find first '{' to skip any human-readable prefix
		idx := strings.Index(string(allIn), "{")
		if idx < 0 {
			fmt.Fprintln(os.Stderr, "no JSON found in stdin")
			os.Exit(1)
		}
		raw = allIn[idx:]
	}

	var analysis AnalysisResult
	if err := json.Unmarshal(raw, &analysis); err != nil {
		fmt.Fprintf(os.Stderr, "error parsing JSON: %v\n", err)
		os.Exit(1)
	}

	// Run trace
	result := traceImpact(analysis, root)

	if !*quiet {
		printImpactReport(result)
	}

	enc, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error marshalling JSON: %v\n", err)
		os.Exit(1)
	}

	if *output != "" {
		if err := os.WriteFile(*output, enc, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "error writing output: %v\n", err)
			os.Exit(1)
		}
	}
	fmt.Println(string(enc))
}

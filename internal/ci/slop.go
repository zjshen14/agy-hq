package ci

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const defaultModulePath = "github.com/zjshen14/agy-hq"

// lockRankedVaultJSONFiles lists Vault JSON state stores protected by internal/store rank locks
// and store.WriteAtomic; raw os.WriteFile calls targeting them in production code are forbidden.
var lockRankedVaultJSONFiles = []string{
	"tasks.json",
	"quota_state.json",
	"events.json",
	"timers.json",
	"goals.json",
	"extensions.json",
	"workspaces.json",
	"pending_subagents.json",
	"pending_nested_subagents.json",
	"nested_conversations.json",
}

type monolithCeiling struct {
	relPath string
	maxLOC  int
}

// monolithLOCCeilings ratchets monolith source files tightly near their post-cleanup baselines
// (1993 / 595 / 396 / 1093 / 1056 LOC, with a <=22 LOC headroom) so future PRs cannot re-bloat them.
var monolithLOCCeilings = []monolithCeiling{
	{relPath: "internal/kernel/kernel.go", maxLOC: 2015},
	{relPath: "cmd/hq/main.go", maxLOC: 615},
	{relPath: "internal/hooks/hooks.go", maxLOC: 415},
	{relPath: "internal/webui/webui.go", maxLOC: 1115},
	{relPath: "internal/kernel/workspace_lifecycle.go", maxLOC: 1075},
}

var reTestCUJToken = regexp.MustCompile(`\bTestCUJ[A-Za-z0-9_]*\b`)

type slopPkg struct {
	pkgPath  string
	relDir   string
	pkg      *types.Package
	info     *types.Info
	files    []*ast.File // build-tag-matched production files
	allFiles []*ast.File // all production files (used for OS-independent AST checks)
}

type slopWorkspace struct {
	rootDir  string
	modPath  string
	fset     *token.FileSet
	stdImp   types.Importer
	cache    map[string]*types.Package
	inProg   map[string]bool
	parsed   map[string]*slopPkg
	pkgDirs  []string
	packages []*slopPkg
	loadErr  error
}

func newSlopWorkspace(rootDir string) *slopWorkspace {
	ws := &slopWorkspace{
		rootDir: rootDir,
		modPath: readModulePath(rootDir),
		fset:    token.NewFileSet(),
		stdImp:  importer.Default(),
		cache:   make(map[string]*types.Package),
		inProg:  make(map[string]bool),
		parsed:  make(map[string]*slopPkg),
	}
	ws.pkgDirs = discoverProdPkgDirs(rootDir)
	for _, relDir := range ws.pkgDirs {
		pkgPath := ws.modPath + "/" + relDir
		sp, err := ws.loadPackage(pkgPath, relDir)
		if err != nil {
			ws.loadErr = err
			return ws
		}
		if sp != nil {
			ws.packages = append(ws.packages, sp)
		}
	}
	return ws
}

func readModulePath(rootDir string) string {
	raw, err := os.ReadFile(filepath.Join(rootDir, "go.mod"))
	if err != nil {
		return defaultModulePath
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && fields[0] == "module" {
			return fields[1]
		}
	}
	return defaultModulePath
}

func discoverProdPkgDirs(rootDir string) []string {
	var dirs []string
	seen := make(map[string]bool)
	for _, top := range []string{"cmd", "internal"} {
		topAbs := filepath.Join(rootDir, top)
		if st, err := os.Stat(topAbs); err != nil || !st.IsDir() {
			continue
		}
		_ = filepath.WalkDir(topAbs, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				name := d.Name()
				if name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
				strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return nil
			}
			relDir, rErr := filepath.Rel(rootDir, filepath.Dir(path))
			if rErr != nil {
				return nil
			}
			relDir = filepath.ToSlash(relDir)
			if !seen[relDir] {
				seen[relDir] = true
				dirs = append(dirs, relDir)
			}
			return nil
		})
	}
	sort.Strings(dirs)
	return dirs
}

// Import implements types.Importer using rootDir sources for module packages and importer.Default() for stdlib.
func (ws *slopWorkspace) Import(path string) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	if ws.modPath != "" && (path == ws.modPath || strings.HasPrefix(path, ws.modPath+"/")) {
		if p, ok := ws.cache[path]; ok {
			return p, nil
		}
		if ws.inProg[path] {
			return nil, fmt.Errorf("import cycle involving %s", path)
		}
		relDir := strings.TrimPrefix(path, ws.modPath)
		relDir = strings.TrimPrefix(relDir, "/")
		sp, err := ws.loadPackage(path, relDir)
		if err != nil {
			return nil, err
		}
		if sp == nil || sp.pkg == nil {
			return nil, fmt.Errorf("no matching Go source files in %s", relDir)
		}
		return sp.pkg, nil
	}
	return ws.stdImp.Import(path)
}

func (ws *slopWorkspace) loadPackage(pkgPath, relDir string) (*slopPkg, error) {
	if sp, ok := ws.parsed[pkgPath]; ok {
		return sp, nil
	}
	ws.inProg[pkgPath] = true
	defer delete(ws.inProg, pkgPath)

	absDir := filepath.Join(ws.rootDir, filepath.FromSlash(relDir))
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil, fmt.Errorf("read package dir %s: %w", relDir, err)
	}

	ctxt := build.Default
	var matchedFiles []*ast.File
	var allFiles []*ast.File
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		absFile := filepath.Join(absDir, name)
		parsed, pErr := parser.ParseFile(ws.fset, absFile, nil, parser.ParseComments)
		if pErr != nil {
			return nil, fmt.Errorf("parse %s/%s: %w", relDir, name, pErr)
		}
		allFiles = append(allFiles, parsed)

		match, mErr := ctxt.MatchFile(absDir, name)
		if mErr != nil {
			return nil, fmt.Errorf("match build tags %s/%s: %w", relDir, name, mErr)
		}
		if match {
			matchedFiles = append(matchedFiles, parsed)
		}
	}

	if len(matchedFiles) == 0 {
		sp := &slopPkg{
			pkgPath:  pkgPath,
			relDir:   relDir,
			allFiles: allFiles,
		}
		ws.parsed[pkgPath] = sp
		return sp, nil
	}

	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}
	conf := types.Config{Importer: ws}
	pkg, err := conf.Check(pkgPath, ws.fset, matchedFiles, info)
	if err != nil {
		return nil, fmt.Errorf("typecheck %s: %w", relDir, err)
	}

	ws.cache[pkgPath] = pkg
	sp := &slopPkg{
		pkgPath:  pkgPath,
		relDir:   relDir,
		pkg:      pkg,
		info:     info,
		files:    matchedFiles,
		allFiles: allFiles,
	}
	ws.parsed[pkgPath] = sp
	return sp, nil
}

// checkGateH5Slop runs all Gate H5 anti-slop subchecks (H5-A .. H5-F).
func checkGateH5Slop(rootDir string) []HarnessCheckResult {
	ws := newSlopWorkspace(rootDir)

	h5aOK, h5aDetail := checkGateH5ANoProdTestingImport(ws)
	h5bOK, h5bDetail := checkGateH5BDeadUnexportedSymbols(ws)
	h5cOK, h5cDetail := checkGateH5CNoUnlockedVaultWrites(ws)
	h5dOK, h5dDetail := checkGateH5DSidecarParity(rootDir)
	h5eOK, h5eDetail := checkGateH5ECUJDocParity(rootDir)
	h5fOK, h5fDetail := checkGateH5FMonolithLOCCeilings(rootDir)

	return []HarnessCheckResult{
		{Gate: "H5-A", Name: `Zero Production "testing" Imports`, Passed: h5aOK, Detail: h5aDetail},
		{Gate: "H5-B", Name: "Zero Dead Unexported Go Symbols (go/types)", Passed: h5bOK, Detail: h5bDetail},
		{Gate: "H5-C", Name: "Zero Unlocked Raw os.WriteFile on Vault JSON Stores", Passed: h5cOK, Detail: h5cDetail},
		{Gate: "H5-D", Name: "Console Sidecar Directory & Byte/Mode Parity", Passed: h5dOK, Detail: h5dDetail},
		{Gate: "H5-E", Name: "CUJ E2E-to-Doc Parity (0003-critical-user-journeys)", Passed: h5eOK, Detail: h5eDetail},
		{Gate: "H5-F", Name: "Monolith File LOC Ceiling Ratchet", Passed: h5fOK, Detail: h5fDetail},
	}
}

func relPosPath(rootDir string, fset *token.FileSet, p token.Pos) string {
	pos := fset.Position(p)
	rel, err := filepath.Rel(rootDir, pos.Filename)
	if err != nil {
		rel = pos.Filename
	}
	return fmt.Sprintf("%s:%d", filepath.ToSlash(rel), pos.Line)
}

func checkGateH5ANoProdTestingImport(ws *slopWorkspace) (bool, string) {
	if ws.loadErr != nil {
		return false, fmt.Sprintf("Go workspace load failed: %v", ws.loadErr)
	}
	if len(ws.packages) == 0 {
		return true, "no non-test Go packages under cmd/ or internal/ (skipped)"
	}

	var violations []string
	fileCount := 0
	for _, sp := range ws.packages {
		for _, f := range sp.allFiles {
			fileCount++
			for _, imp := range f.Imports {
				if strings.Trim(imp.Path.Value, `"`) == "testing" {
					violations = append(violations, relPosPath(ws.rootDir, ws.fset, imp.Pos()))
				}
			}
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		return false, fmt.Sprintf("production Go file(s) import \"testing\": %s", strings.Join(violations, ", "))
	}
	return true, fmt.Sprintf("%d non-test Go file(s) across %d package(s) verified (0 \"testing\" imports)", fileCount, len(ws.packages))
}

func checkGateH5BDeadUnexportedSymbols(ws *slopWorkspace) (bool, string) {
	if ws.loadErr != nil {
		return false, fmt.Sprintf("Go workspace typecheck failed: %v", ws.loadErr)
	}
	checkedPkgs := 0
	checkedSyms := 0
	var dead []string

	for _, sp := range ws.packages {
		if sp.pkg == nil || len(sp.files) == 0 {
			continue
		}
		checkedPkgs++
		pkgDead, symCount := findDeadUnexportedInPkg(ws.rootDir, ws.fset, sp)
		checkedSyms += symCount
		dead = append(dead, pkgDead...)
	}
	if checkedPkgs == 0 {
		return true, "no build-matched non-test Go packages under cmd/ or internal/ (skipped)"
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		return false, fmt.Sprintf("%d dead unexported symbol(s): %s", len(dead), strings.Join(dead, "; "))
	}
	return true, fmt.Sprintf("%d unexported package symbols/methods across %d package(s) verified live (0 dead)", checkedSyms, checkedPkgs)
}

func normalizeUsedObj(obj types.Object) types.Object {
	if fn, ok := obj.(*types.Func); ok {
		return fn.Origin()
	}
	return obj
}

func findDeadUnexportedInPkg(rootDir string, fset *token.FileSet, sp *slopPkg) ([]string, int) {
	declNodes := make(map[types.Object]ast.Node)
	linknamed := make(map[string]bool)
	var orderedObjs []types.Object

	for _, f := range sp.files {
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				if strings.HasPrefix(c.Text, "//go:linkname ") {
					fields := strings.Fields(strings.TrimPrefix(c.Text, "//go:linkname "))
					if len(fields) >= 1 {
						linknamed[fields[0]] = true
					}
				}
			}
		}
		for _, d := range f.Decls {
			switch decl := d.(type) {
			case *ast.FuncDecl:
				if obj := sp.info.Defs[decl.Name]; obj != nil {
					declNodes[obj] = decl
					orderedObjs = append(orderedObjs, obj)
				}
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if obj := sp.info.Defs[s.Name]; obj != nil {
							declNodes[obj] = s
							orderedObjs = append(orderedObjs, obj)
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if obj := sp.info.Defs[n]; obj != nil {
								declNodes[obj] = s
								orderedObjs = append(orderedObjs, obj)
							}
						}
					}
				}
			}
		}
	}

	// Collect named and anonymous interfaces declared/referenced in the package.
	var namedIfaces []*types.TypeName
	for _, obj := range orderedObjs {
		if tn, ok := obj.(*types.TypeName); ok {
			if iface, ok := tn.Type().Underlying().(*types.Interface); ok && iface.NumMethods() > 0 {
				namedIfaces = append(namedIfaces, tn)
			}
		}
	}

	// Track anonymous interfaces by AST node so we only activate them when their enclosing decl is live.
	anonIfaceTypes := make(map[*ast.InterfaceType]*types.Interface)
	for expr, tv := range sp.info.Types {
		if it, ok := expr.(*ast.InterfaceType); ok && tv.Type != nil {
			if iface, ok := tv.Type.Underlying().(*types.Interface); ok && iface.NumMethods() > 0 {
				anonIfaceTypes[it] = iface
			}
		}
	}

	reachable := make(map[types.Object]bool)
	liveTypes := make(map[*types.TypeName]bool)
	liveIfaces := make(map[*types.Interface]bool)
	var queue []types.Object

	mark := func(obj types.Object) {
		if obj == nil {
			return
		}
		obj = normalizeUsedObj(obj)
		if obj.Pkg() != sp.pkg || reachable[obj] {
			return
		}
		if _, ok := declNodes[obj]; !ok {
			return
		}
		reachable[obj] = true
		queue = append(queue, obj)
		if tn, ok := obj.(*types.TypeName); ok {
			liveTypes[tn] = true
			if iface, ok := tn.Type().Underlying().(*types.Interface); ok && iface.NumMethods() > 0 {
				liveIfaces[iface] = true
			}
		}
	}

	isLiveRoot := func(obj types.Object) bool {
		if obj.Exported() || obj.Name() == "_" || linknamed[obj.Name()] {
			return true
		}
		if fn, ok := obj.(*types.Func); ok {
			sig := fn.Type().(*types.Signature)
			if sig.Recv() == nil {
				if fn.Name() == "init" || (sp.pkg.Name() == "main" && fn.Name() == "main") {
					return true
				}
			}
			if fd, isFD := declNodes[obj].(*ast.FuncDecl); isFD && fd.Body == nil {
				return true
			}
		}
		return false
	}

	for _, obj := range orderedObjs {
		if isLiveRoot(obj) {
			mark(obj)
		}
	}

	inspectDeclAST := func(node ast.Node) {
		inspectExprOrStmt := func(root ast.Node) {
			ast.Inspect(root, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.Ident:
					if u := sp.info.Uses[x]; u != nil {
						mark(u)
					}
				case *ast.SelectorExpr:
					if sel := sp.info.Selections[x]; sel != nil && sel.Obj() != nil {
						mark(sel.Obj())
					}
				case *ast.InterfaceType:
					if iface, ok := anonIfaceTypes[x]; ok {
						liveIfaces[iface] = true
					}
				}
				return true
			})
		}

		// For ValueSpec (e.g. var a, b = expr1, expr2), inspect Type and Values so one
		// live variable in a multi-var declaration doesn't falsely inspect unrelated idents.
		if vs, ok := node.(*ast.ValueSpec); ok {
			if vs.Type != nil {
				inspectExprOrStmt(vs.Type)
			}
			for _, v := range vs.Values {
				inspectExprOrStmt(v)
			}
			return
		}
		inspectExprOrStmt(node)
	}

	// Fixed-point reachability combining AST uses and interface method satisfaction (direct & promoted).
	for {
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			inspectDeclAST(declNodes[cur])
		}

		prevQueueLen := len(queue)
		for _, tn := range namedIfaces {
			if reachable[tn] {
				if iface, ok := tn.Type().Underlying().(*types.Interface); ok {
					liveIfaces[iface] = true
				}
			}
		}
		for tn := range liveTypes {
			t := tn.Type()
			if _, isIface := t.Underlying().(*types.Interface); isIface {
				continue
			}
			ptrT := t
			if _, isPtr := t.(*types.Pointer); !isPtr {
				ptrT = types.NewPointer(t)
			}
			for iface := range liveIfaces {
				if !types.Implements(t, iface) && !types.Implements(ptrT, iface) {
					continue
				}
				for i := 0; i < iface.NumMethods(); i++ {
					m := iface.Method(i)
					if m.Exported() {
						continue
					}
					if implObj, _, _ := types.LookupFieldOrMethod(ptrT, true, sp.pkg, m.Name()); implObj != nil {
						mark(implObj)
					}
				}
			}
		}
		if len(queue) == prevQueueLen {
			break
		}
	}

	var dead []string
	checked := 0
	for _, obj := range orderedObjs {
		if obj.Exported() || obj.Name() == "_" {
			continue
		}
		switch o := obj.(type) {
		case *types.Func:
			sig := o.Type().(*types.Signature)
			if sig.Recv() == nil && (o.Name() == "init" || (sp.pkg.Name() == "main" && o.Name() == "main")) {
				continue
			}
		case *types.TypeName, *types.Var, *types.Const:
		default:
			continue
		}
		checked++
		if !reachable[obj] {
			dead = append(dead, fmt.Sprintf("%s (%s)", relPosPath(rootDir, fset, obj.Pos()), obj.Name()))
		}
	}
	return dead, checked
}

func checkGateH5CNoUnlockedVaultWrites(ws *slopWorkspace) (bool, string) {
	if ws.loadErr != nil {
		return false, fmt.Sprintf("Go workspace load failed: %v", ws.loadErr)
	}
	if len(ws.packages) == 0 {
		return true, "no non-test Go packages under cmd/ or internal/ (skipped)"
	}

	var violations []string
	checkedPkgs := 0
	for _, sp := range ws.packages {
		if sp.relDir == "internal/store" || strings.HasPrefix(sp.relDir, "internal/store/") {
			continue
		}
		if sp.pkg == nil || len(sp.files) == 0 {
			continue
		}
		checkedPkgs++
		violations = append(violations, findUnlockedStoreWrites(ws.rootDir, ws.fset, sp)...)
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		return false, fmt.Sprintf("unlocked raw os.WriteFile on lock-ranked Vault JSON store(s): %s", strings.Join(violations, "; "))
	}
	return true, fmt.Sprintf("%d non-store package(s) verified (0 raw os.WriteFile calls on lock-ranked Vault JSON stores)", checkedPkgs)
}

func matchLockRankedJSONLiteral(raw string) string {
	norm := strings.ReplaceAll(strings.TrimSpace(raw), `\`, `/`)
	for _, target := range lockRankedVaultJSONFiles {
		if norm == target || strings.HasSuffix(norm, "/"+target) {
			return target
		}
	}
	return ""
}

func findUnlockedStoreWrites(rootDir string, fset *token.FileSet, sp *slopPkg) []string {
	// Identify package-level consts/vars/helper functions that resolve to a lock-ranked JSON store path.
	taintedObjs := make(map[types.Object]string)

	var exprTarget func(expr ast.Expr, localTainted map[string]string) string
	exprTarget = func(expr ast.Expr, localTainted map[string]string) string {
		if expr == nil {
			return ""
		}
		var found string
		ast.Inspect(expr, func(n ast.Node) bool {
			if found != "" {
				return false
			}
			switch x := n.(type) {
			case *ast.BasicLit:
				if x.Kind == token.STRING {
					if m := matchLockRankedJSONLiteral(strings.Trim(x.Value, "`\"")); m != "" {
						found = m
						return false
					}
				}
			case *ast.Ident:
				if localTainted != nil {
					if m := localTainted[x.Name]; m != "" {
						found = m
						return false
					}
				}
				if obj := sp.info.Uses[x]; obj != nil {
					if m := taintedObjs[obj]; m != "" {
						found = m
						return false
					}
				}
			}
			return true
		})
		return found
	}

	// Fixed-point pass over package-level consts/vars and path-returning helper functions.
	for iter := 0; iter < 3; iter++ {
		changed := false
		for _, f := range sp.files {
			for _, d := range f.Decls {
				switch decl := d.(type) {
				case *ast.GenDecl:
					for _, spec := range decl.Specs {
						vs, ok := spec.(*ast.ValueSpec)
						if !ok {
							continue
						}
						for i, val := range vs.Values {
							if i < len(vs.Names) {
								if obj := sp.info.Defs[vs.Names[i]]; obj != nil && taintedObjs[obj] == "" {
									if m := exprTarget(val, nil); m != "" {
										taintedObjs[obj] = m
										changed = true
									}
								}
							}
						}
					}
				case *ast.FuncDecl:
					if decl.Body == nil {
						continue
					}
					fnObj := sp.info.Defs[decl.Name]
					if fnObj == nil || taintedObjs[fnObj] != "" {
						continue
					}
					ast.Inspect(decl.Body, func(n ast.Node) bool {
						ret, ok := n.(*ast.ReturnStmt)
						if !ok {
							return true
						}
						for _, res := range ret.Results {
							if m := exprTarget(res, nil); m != "" {
								taintedObjs[fnObj] = m
								changed = true
								return false
							}
						}
						return true
					})
				}
			}
		}
		if !changed {
			break
		}
	}

	var violations []string
	for _, f := range sp.files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			localTainted := make(map[string]string)
			propagateNames := func(names []*ast.Ident, rhsExprs []ast.Expr) bool {
				changed := false
				for i, lhs := range names {
					if lhs == nil || lhs.Name == "_" {
						continue
					}
					var rhs ast.Expr
					if i < len(rhsExprs) {
						rhs = rhsExprs[i]
					} else if len(rhsExprs) == 1 {
						rhs = rhsExprs[0]
					}
					if m := exprTarget(rhs, localTainted); m != "" && localTainted[lhs.Name] != m {
						localTainted[lhs.Name] = m
						changed = true
					}
				}
				return changed
			}
			for iter := 0; iter < 3; iter++ {
				changed := false
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					switch st := n.(type) {
					case *ast.AssignStmt:
						lhsIdents := make([]*ast.Ident, len(st.Lhs))
						for i, lhs := range st.Lhs {
							lhsIdents[i], _ = lhs.(*ast.Ident)
						}
						if propagateNames(lhsIdents, st.Rhs) {
							changed = true
						}
					case *ast.ValueSpec:
						if propagateNames(st.Names, st.Values) {
							changed = true
						}
					}
					return true
				})
				if !changed {
					break
				}
			}

			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "WriteFile" {
					return true
				}
				obj := sp.info.Uses[sel.Sel]
				if obj == nil || obj.Pkg() == nil || obj.Pkg().Path() != "os" {
					return true
				}
				if target := exprTarget(call.Args[0], localTainted); target != "" {
					violations = append(violations, fmt.Sprintf("%s (%s)", relPosPath(rootDir, fset, call.Pos()), target))
				}
				return true
			})
		}
	}
	return violations
}

func checkGateH5DSidecarParity(rootDir string) (bool, string) {
	pluginSidecars := filepath.Join(rootDir, ".agents", "plugins", "agy-hq", "sidecars", "agy-hq-console")
	tmplSidecars := filepath.Join(rootDir, "templates", "sidecars", "agy-hq-console")
	pluginManifest := filepath.Join(rootDir, ".agents", "plugins", "agy-hq", "plugin.json")

	_, pErr := os.Stat(pluginSidecars)
	_, tErr := os.Stat(tmplSidecars)
	_, mErr := os.Stat(pluginManifest)
	if pErr != nil && tErr != nil && mErr != nil {
		return true, "no agy-hq-console sidecar directories present (skipped)"
	}
	if pErr != nil || tErr != nil {
		return false, fmt.Sprintf("missing agy-hq-console sidecar directory (pluginErr=%v, tmplErr=%v)", pErr, tErr)
	}

	pTree, err := collectSidecarTree(pluginSidecars)
	if err != nil {
		return false, fmt.Sprintf("scan plugin sidecar agy-hq-console: %v", err)
	}
	tTree, err := collectSidecarTree(tmplSidecars)
	if err != nil {
		return false, fmt.Sprintf("scan template sidecar agy-hq-console: %v", err)
	}

	for rel := range pTree {
		if _, ok := tTree[rel]; !ok {
			return false, fmt.Sprintf("sidecar entry %q exists in .agents/plugins/agy-hq/sidecars/agy-hq-console/ but is missing in templates/sidecars/agy-hq-console/", rel)
		}
	}
	for rel := range tTree {
		if _, ok := pTree[rel]; !ok {
			return false, fmt.Sprintf("sidecar entry %q exists in templates/sidecars/agy-hq-console/ but is missing in .agents/plugins/agy-hq/sidecars/agy-hq-console/", rel)
		}
	}

	fileCount := 0
	var rels []string
	for rel := range pTree {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	for _, rel := range rels {
		pEnt := pTree[rel]
		tEnt := tTree[rel]
		if pEnt.isDir != tEnt.isDir {
			return false, fmt.Sprintf("sidecar entry %q kind mismatch (dir vs file)", rel)
		}
		if pEnt.isDir {
			continue
		}
		fileCount++
		if !bytes.Equal(pEnt.data, tEnt.data) {
			return false, fmt.Sprintf("sidecar byte drift on %q between .agents/plugins/agy-hq/sidecars/agy-hq-console/ and templates/sidecars/agy-hq-console/", rel)
		}
		pExec := pEnt.mode&0o111 != 0
		tExec := tEnt.mode&0o111 != 0
		if pExec != tExec {
			return false, fmt.Sprintf("sidecar executable mode drift on %q (plugin=%04o, template=%04o)", rel, pEnt.mode, tEnt.mode)
		}
	}

	for _, req := range []string{"main.mjs", "launch.sh", "sidecar.json"} {
		ent, ok := pTree[req]
		if !ok || ent.isDir {
			return false, fmt.Sprintf("required agy-hq-console asset %q missing", req)
		}
		if req == "launch.sh" && ent.mode&0o111 == 0 {
			return false, "sidecar asset agy-hq-console/launch.sh must have executable mode"
		}
	}

	return true, fmt.Sprintf("%d agy-hq-console sidecar file(s) verified with exact byte & mode parity", fileCount)
}

type sidecarEntry struct {
	isDir bool
	mode  os.FileMode
	data  []byte
}

func collectSidecarTree(baseDir string) (map[string]sidecarEntry, error) {
	out := make(map[string]sidecarEntry)
	err := filepath.WalkDir(baseDir, func(path string, d fs.DirEntry, wErr error) error {
		if wErr != nil {
			return wErr
		}
		if path == baseDir {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("unexpected symlink %s", path)
		}
		rel, err := filepath.Rel(baseDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			out[rel] = sidecarEntry{isDir: true, mode: info.Mode().Perm()}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = sidecarEntry{mode: info.Mode().Perm(), data: data}
		return nil
	})
	return out, err
}

func checkGateH5ECUJDocParity(rootDir string) (bool, string) {
	e2ePath := filepath.Join(rootDir, "internal", "e2e", "cuj_e2e_test.go")
	docPath := filepath.Join(rootDir, "docs", "product", "0003-critical-user-journeys.md")

	_, eErr := os.Stat(e2ePath)
	_, dErr := os.Stat(docPath)
	if os.IsNotExist(eErr) && os.IsNotExist(dErr) {
		return true, "no CUJ E2E suite or CUJ doc present (skipped)"
	}
	if eErr != nil || dErr != nil {
		return false, fmt.Sprintf("missing CUJ parity target (e2eErr=%v, docErr=%v)", eErr, dErr)
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, e2ePath, nil, 0)
	if err != nil {
		return false, fmt.Sprintf("parse internal/e2e/cuj_e2e_test.go: %v", err)
	}

	var testCUJs []string
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv != nil {
			continue
		}
		if strings.HasPrefix(fd.Name.Name, "TestCUJ") {
			testCUJs = append(testCUJs, fd.Name.Name)
		}
	}
	if len(testCUJs) == 0 {
		return false, "no TestCUJ* functions found in internal/e2e/cuj_e2e_test.go"
	}

	docBytes, err := os.ReadFile(docPath)
	if err != nil {
		return false, fmt.Sprintf("read docs/product/0003-critical-user-journeys.md: %v", err)
	}
	docRefs := make(map[string]bool)
	for _, tok := range reTestCUJToken.FindAllString(string(docBytes), -1) {
		docRefs[tok] = true
	}

	var missing []string
	for _, fn := range testCUJs {
		if !docRefs[fn] {
			missing = append(missing, fn)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return false, fmt.Sprintf("TestCUJ* function(s) missing from docs/product/0003-critical-user-journeys.md: %s", strings.Join(missing, ", "))
	}
	return true, fmt.Sprintf("%d/%d TestCUJ* functions documented in docs/product/0003-critical-user-journeys.md", len(testCUJs), len(testCUJs))
}

func checkGateH5FMonolithLOCCeilings(rootDir string) (bool, string) {
	checked := 0
	var exceeded []string
	for _, c := range monolithLOCCeilings {
		abs := filepath.Join(rootDir, filepath.FromSlash(c.relPath))
		raw, err := os.ReadFile(abs)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, fmt.Sprintf("read %s: %v", c.relPath, err)
		}
		checked++
		loc := bytes.Count(raw, []byte{'\n'})
		if len(raw) > 0 && raw[len(raw)-1] != '\n' {
			loc++
		}
		if loc > c.maxLOC {
			exceeded = append(exceeded, fmt.Sprintf("%s (%d > max %d LOC)", c.relPath, loc, c.maxLOC))
		}
	}
	if checked == 0 {
		return true, "no tracked monolith files present (skipped)"
	}
	if len(exceeded) > 0 {
		return false, fmt.Sprintf("monolith LOC ceiling exceeded: %s", strings.Join(exceeded, "; "))
	}
	return true, fmt.Sprintf("%d monolith file(s) within LOC ceilings", checked)
}

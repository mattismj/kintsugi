package tools

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"kintsugi/internal/provider"
)

// astGrepTool is a Go-only structural query. It walks go/ast over the
// workspace and matches a small pattern subset:
//
//   - "func <name>(…)"       → function/method declarations whose name matches
//   - "type <name> …"        → type declarations whose name matches
//   - "call <fn>"            → every call site of the named function
//
// Results are file:line plus a one-line snippet. No tree-sitter dependency.
func astGrepTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name:        "ast_grep",
			Description: "Go-only structural query over the workspace. Pattern subset: \"func <name>(…)\", \"type <name> …\", \"call <fn>\". Returns file:line + snippet.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"pattern"},
				Properties: map[string]provider.ToolProp{
					"pattern": {Type: "string", Description: "Pattern string, e.g. \"func Execute(…)\", \"type Review …\", \"call ParseReview\"."},
					"path":    {Type: "string", Description: "Optional workspace-relative file or directory to search (default: workspace root)."},
					"max":     {Type: "integer", Description: "Maximum results (default 100)."},
				},
			},
		},
	}
}

// astGrepQuery is the parsed form of a pattern.
type astGrepQuery struct {
	kind string // "func" | "type" | "call"
	name string
}

// parseASTGrepPattern accepts the documented subset.
//
//   - "func <name>(…)" or "func <name>"
//   - "type <name> …" or "type <name>"
//   - "call <fn>"
//
// Whitespace is trimmed; the trailing "(…)" or "…" is optional noise.
func parseASTGrepPattern(pattern string) (astGrepQuery, error) {
	p := strings.TrimSpace(pattern)
	if p == "" {
		return astGrepQuery{}, fmt.Errorf("ast_grep: empty pattern")
	}
	fields := strings.Fields(p)
	if len(fields) == 0 {
		return astGrepQuery{}, fmt.Errorf("ast_grep: empty pattern")
	}
	kind := strings.ToLower(fields[0])
	switch kind {
	case "func":
		if len(fields) < 2 {
			return astGrepQuery{}, fmt.Errorf("ast_grep: func pattern needs a name")
		}
		name := strings.TrimRight(fields[1], "(…)")
		name = strings.TrimSuffix(name, "(")
		name = strings.TrimSuffix(name, ")")
		if name == "" {
			return astGrepQuery{}, fmt.Errorf("ast_grep: func pattern needs a name")
		}
		return astGrepQuery{kind: "func", name: name}, nil
	case "type":
		if len(fields) < 2 {
			return astGrepQuery{}, fmt.Errorf("ast_grep: type pattern needs a name")
		}
		name := strings.Trim(fields[1], "…")
		if name == "" {
			return astGrepQuery{}, fmt.Errorf("ast_grep: type pattern needs a name")
		}
		return astGrepQuery{kind: "type", name: name}, nil
	case "call":
		if len(fields) < 2 {
			return astGrepQuery{}, fmt.Errorf("ast_grep: call pattern needs a function name")
		}
		return astGrepQuery{kind: "call", name: fields[1]}, nil
	}
	return astGrepQuery{}, fmt.Errorf("ast_grep: unsupported pattern %q (want func|type|call)", kind)
}

// astGrepMatch is one result.
type astGrepMatch struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Snippet string `json:"snippet"`
}

// runASTGrep walks the workspace (or a subtree) and returns matches for q.
func (e *Executor) runASTGrep(args string) Result {
	var a struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Max     int    `json:"max"`
	}
	if err := decode(args, &a); err != nil || strings.TrimSpace(a.Pattern) == "" {
		return Result{Content: `{"error":"ast_grep: missing required property 'pattern'"}`, Failed: true}
	}
	q, err := parseASTGrepPattern(a.Pattern)
	if err != nil {
		return failJSON(err)
	}
	start := a.Path
	if strings.TrimSpace(start) == "" {
		start = "."
	}
	base, err := e.resolveFor("ast_grep", start)
	if err != nil {
		return failJSON(err)
	}
	limit := a.Max
	if limit <= 0 {
		limit = 100
	}

	var matches []astGrepMatch
	fset := token.NewFileSet()
	filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(e.Workspace, p)
		rel = filepath.ToSlash(rel)
		if !e.Scope.AllowsPath(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > 2<<20 {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if perr != nil {
			return nil
		}
		found := matchAST(fset, f, q)
		for _, m := range found {
			m.File = rel
			matches = append(matches, m)
			if len(matches) >= limit {
				return filepath.SkipAll
			}
		}
		return nil
	})
	if len(matches) == 0 {
		return Result{Content: "no matches"}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].File != matches[j].File {
			return matches[i].File < matches[j].File
		}
		return matches[i].Line < matches[j].Line
	})
	var b strings.Builder
	for _, m := range matches {
		fmt.Fprintf(&b, "%s:%d: %s\n", m.File, m.Line, m.Snippet)
	}
	return Result{Content: strings.TrimRight(b.String(), "\n")}
}

// matchAST walks one parsed file and returns matches for q.
func matchAST(fset *token.FileSet, f *ast.File, q astGrepQuery) []astGrepMatch {
	var out []astGrepMatch
	switch q.kind {
	case "func":
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Name == nil {
				continue
			}
			if fd.Name.Name != q.name {
				continue
			}
			pos := fset.Position(fd.Pos())
			snip := funcSnippet(fd)
			out = append(out, astGrepMatch{Line: pos.Line, Snippet: snip})
		}
	case "type":
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, sp := range gd.Specs {
				ts, ok := sp.(*ast.TypeSpec)
				if !ok || ts.Name == nil {
					continue
				}
				if ts.Name.Name != q.name {
					continue
				}
				pos := fset.Position(ts.Pos())
				out = append(out, astGrepMatch{Line: pos.Line, Snippet: typeSnippet(ts)})
			}
		}
	case "call":
		ast.Inspect(f, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := callName(ce)
			if name != q.name && tailName(name) != q.name {
				return true
			}
			pos := fset.Position(ce.Pos())
			out = append(out, astGrepMatch{Line: pos.Line, Snippet: callSnippet(ce)})
			return true
		})
	}
	return out
}

// funcSnippet renders a one-line declaration header.
func funcSnippet(fd *ast.FuncDecl) string {
	var b strings.Builder
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		b.WriteString("func (" + exprString(fd.Recv.List[0].Type) + ") ")
	} else {
		b.WriteString("func ")
	}
	b.WriteString(fd.Name.Name)
	b.WriteString("(")
	if fd.Type.Params != nil {
		b.WriteString(paramList(fd.Type.Params))
	}
	b.WriteString(")")
	if fd.Type.Results != nil && len(fd.Type.Results.List) > 0 {
		b.WriteString(" " + paramList(fd.Type.Results))
	}
	return b.String()
}

// typeSnippet renders the declaration head.
func typeSnippet(ts *ast.TypeSpec) string {
	return "type " + ts.Name.Name + " " + exprString(ts.Type)
}

// callName returns the textual name of a call expression's function.
func callName(ce *ast.CallExpr) string {
	return exprString(ce.Fun)
}

// tailName returns the last segment of a dotted name (pkg.Fn → Fn).
func tailName(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}

// callSnippet renders the call head up to the opening paren.
func callSnippet(ce *ast.CallExpr) string {
	return exprString(ce.Fun) + "(…)"
}

// exprString renders an expression as a compact string.
func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	case *ast.StarExpr:
		return "*" + exprString(x.X)
	case *ast.ArrayType:
		return "[]" + exprString(x.Elt)
	case *ast.MapType:
		return "map[" + exprString(x.Key) + "]" + exprString(x.Value)
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.FuncType:
		return "func(…)"
	case *ast.ChanType:
		return "chan " + exprString(x.Value)
	case *ast.Ellipsis:
		return "…" + exprString(x.Elt)
	case *ast.IndexExpr:
		return exprString(x.X) + "[" + exprString(x.Index) + "]"
	}
	return "…"
}

// paramList renders a parameter/return list as a comma-separated signature.
func paramList(fl *ast.FieldList) string {
	var parts []string
	for _, f := range fl.List {
		t := exprString(f.Type)
		if len(f.Names) == 0 {
			parts = append(parts, t)
			continue
		}
		for _, n := range f.Names {
			parts = append(parts, n.Name+" "+t)
		}
	}
	return strings.Join(parts, ", ")
}

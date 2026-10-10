// Package archtest checks source-code rules, such as "no float on a money
// path", as ordinary tests, without an extra lint tool.
package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type Finding struct {
	Pos  string
	Rule string
}

// floatVerb matches fmt verbs that format or scan floats (%f, %.2f, %e, %g).
var floatVerb = regexp.MustCompile(`%[-+# 0]*[0-9]*(\.[0-9]+)?[eEfFgG]`)

// FloatFindings reports every way a binary float can enter non-test code
// under root. It walks the syntax tree instead of grepping, so comments are
// ignored and format verbs are caught. Without type information, a float
// hidden behind an alias declared elsewhere is not detected.
func FloatFindings(root string) ([]Finding, error) {
	fset := token.NewFileSet()
	var out []Finding
	add := func(pos token.Pos, rule string) {
		out = append(out, Finding{Pos: fset.Position(pos).String(), Rule: rule})
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		for _, imp := range file.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == "math" || p == "math/big" {
				add(imp.Pos(), `import "`+p+`"`)
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				if n.Name == "float32" || n.Name == "float64" {
					add(n.Pos(), n.Name)
				}
			case *ast.SelectorExpr:
				pkg, _ := n.X.(*ast.Ident)
				switch {
				case pkg != nil && pkg.Name == "strconv" && strings.Contains(n.Sel.Name, "Float"):
					add(n.Pos(), "strconv."+n.Sel.Name)
				case pkg != nil && pkg.Name == "json" && n.Sel.Name == "Number":
					add(n.Pos(), "json.Number")
				case n.Sel.Name == "Float64" || n.Sel.Name == "Float32":
					add(n.Pos(), "."+n.Sel.Name+"()")
				}
			case *ast.BasicLit:
				if n.Kind == token.STRING && floatVerb.MatchString(n.Value) {
					add(n.Pos(), "float format verb")
				}
			}
			return true
		})
		return nil
	})
	return out, err
}

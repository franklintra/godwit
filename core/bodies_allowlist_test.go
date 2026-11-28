package core_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fork keeps mail bodies in message_bodies and leaves
// messages.body_text and messages.body_html NULL (ADR 0053). Code that
// reads them from messages shows an empty body, so a statement may name
// them only in a draft statement, a statement on message_bodies, or the
// files that own the layout. Read a message's body with bodyText/bodyHTML
// (core/bodysql.go) or store.BodyTextSQL/BodyHTMLSQL. This fails on a merge
// that brings upstream code reading the inline columns (FORK.md).
func TestBodiesReadThroughHelper(t *testing.T) {
	root, err := filepath.Abs("..")
	require.NoError(t, err)
	owners := map[string]bool{
		"core/store/bodies.go":         true, // the helper, the triggers, the move
		"core/store/schema.go":         true, // upstream's migrations
		"scripts/perfbench/rewrite.go": true, // checks the layout itself: both places, before and after the move
	}
	column := regexp.MustCompile(`\bbody_(text|html)\b`)
	ok := regexp.MustCompile(`\b(drafts|message_bodies)\b`)
	messages := regexp.MustCompile(`\bmessages\b`)
	var bad []string
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" || d.Name() == "testdata" || rel == "web" || rel == "site" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") || owners[rel] {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, isLit := n.(*ast.BasicLit)
			if !isLit || lit.Kind != token.STRING || !column.MatchString(lit.Value) {
				return true
			}
			if ok.MatchString(lit.Value) && !messages.MatchString(lit.Value) {
				return true
			}
			bad = append(bad, fset.Position(lit.Pos()).String())
			return true
		})
		return nil
	})
	require.NoError(t, err)
	assert.Empty(t, bad, "read message bodies through bodyText/bodyHTML (core/bodysql.go), not messages.body_text or messages.body_html")
}

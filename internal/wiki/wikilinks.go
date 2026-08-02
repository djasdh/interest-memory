package wiki

import (
	"strings"

	obsidian "github.com/powerman/goldmark-obsidian"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"

	"go.abhg.dev/goldmark/wikilink"
)

// ExtractWikilinks parses markdown body and returns the distinct [[target]]
// references in the page (excluding embedded resources like ![[img.png]]).
// Header fragments ([[Page#Section]]) reduce to the page target.
func ExtractWikilinks(body string) []string {
	if strings.TrimSpace(body) == "" {
		return nil
	}
	md := goldmark.New(goldmark.WithExtensions(obsidian.NewObsidian()))
	source := []byte(body)
	doc := md.Parser().Parse(text.NewReader(source))

	seen := map[string]bool{}
	var out []string
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if n.Kind() != wikilink.Kind {
			return ast.WalkContinue, nil
		}
		node, ok := n.(*wikilink.Node)
		if !ok || node.Embed {
			return ast.WalkContinue, nil
		}
		target := string(node.Target)
		if target == "" || strings.HasPrefix(target, "#") {
			return ast.WalkContinue, nil
		}
		if !seen[target] {
			seen[target] = true
			out = append(out, target)
		}
		return ast.WalkContinue, nil
	})
	return out
}

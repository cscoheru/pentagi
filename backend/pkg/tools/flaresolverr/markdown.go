package flaresolverr

import (
	"fmt"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

// HTMLToMarkdown extracts readable text from a rendered page. FlareSolverr
// only returns raw HTML, but the browser tool's markdown action has to return
// text. The conversion is intentionally small: it keeps headings, links,
// lists and paragraphs, and drops scripts, styles and navigation.
func HTMLToMarkdown(page string) string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(page))
	if err != nil {
		return ""
	}

	doc.Find("script, style, noscript, template, svg, nav, header, footer").Remove()

	var b strings.Builder
	body := doc.Find("body")
	if body.Length() == 0 {
		body = doc.Selection
	}

	for _, node := range body.Nodes {
		writeChildren(node, &b)
	}

	out := strings.TrimSpace(b.String())
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return out
}

var skipTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true,
	"svg": true, "nav": true, "header": true, "footer": true,
}

// writeNode renders one element node.
func writeNode(node *html.Node, b *strings.Builder) {
	if node.Type != html.ElementNode || skipTags[node.Data] {
		return
	}

	switch node.Data {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		level := int(node.Data[1] - '0')
		text := strings.TrimSpace(textOf(node))
		if text != "" {
			fmt.Fprintf(b, "\n\n%s %s\n\n", strings.Repeat("#", level), text)
		}
	case "a":
		text := strings.TrimSpace(textOf(node))
		href := attrOf(node, "href")
		switch {
		case text != "" && href != "":
			fmt.Fprintf(b, "[%s](%s)", text, href)
		case text != "":
			b.WriteString(text)
		}
	case "li":
		b.WriteString("\n- ")
		writeChildren(node, b)
	case "p", "div", "section", "article", "ul", "ol", "table", "tr", "blockquote", "pre", "br":
		if node.Data == "br" {
			b.WriteString("\n")
		}
		writeChildren(node, b)
		if node.Data != "br" {
			b.WriteString("\n")
		}
	case "strong", "b":
		fmt.Fprintf(b, "**%s**", strings.TrimSpace(textOf(node)))
	case "em", "i":
		fmt.Fprintf(b, "*%s*", strings.TrimSpace(textOf(node)))
	case "code":
		fmt.Fprintf(b, "`%s`", strings.TrimSpace(textOf(node)))
	default:
		writeChildren(node, b)
	}
}

// writeChildren renders all child nodes, including raw text nodes.
func writeChildren(node *html.Node, b *strings.Builder) {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		switch child.Type {
		case html.TextNode:
			text := strings.TrimSpace(child.Data)
			if text != "" {
				b.WriteString(text)
				b.WriteString(" ")
			}
		case html.ElementNode:
			writeNode(child, b)
		}
	}
}

// textOf returns the concatenated text of a node and its descendants.
func textOf(node *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return b.String()
}

// attrOf returns an element attribute value.
func attrOf(node *html.Node, name string) string {
	for _, attr := range node.Attr {
		if attr.Key == name {
			return attr.Val
		}
	}
	return ""
}

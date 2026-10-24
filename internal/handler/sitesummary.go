package handler

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"github.com/vsriram/simple-host/internal/db"
	"golang.org/x/net/html"
)

// ExtractSiteSummary reads only the uploaded index, including an implied body
// in HTML fragments. Invalid or incomplete HTML is handled by the HTML parser.
func ExtractSiteSummary(files map[string][]byte) db.SiteSummary {
	data, ok := files["index.html"]
	if !ok {
		return db.SiteSummary{}
	}
	root, _ := html.Parse(bytes.NewReader(data))
	if root == nil {
		return db.SiteSummary{}
	}
	var summary db.SiteSummary
	var headings, visible []string
	titleFound, descriptionFound := false, false
	var textContent func(*html.Node) string
	textContent = func(n *html.Node) string {
		if n.Type == html.TextNode {
			return n.Data
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "noscript":
				return ""
			}
		}
		var parts []string
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			parts = append(parts, textContent(c))
		}
		return strings.Join(parts, "")
	}
	var walk func(*html.Node, bool)
	walk = func(n *html.Node, inBody bool) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "noscript":
				return
			case "body":
				inBody = true
			case "title":
				if !titleFound {
					summary.Title = summaryCap(textContent(n), 200)
					titleFound = true
				}
			case "meta":
				var name, content string
				for _, a := range n.Attr {
					if strings.EqualFold(a.Key, "name") {
						name = a.Val
					}
					if strings.EqualFold(a.Key, "content") {
						content = a.Val
					}
				}
				if !descriptionFound && strings.EqualFold(name, "description") {
					summary.Description = summaryCap(content, 300)
					descriptionFound = true
				}
			case "h1", "h2", "h3":
				if len(headings) < 6 {
					headings = append(headings, strings.Join(strings.Fields(textContent(n)), " "))
				}
			}
		}
		if n.Type == html.TextNode && inBody {
			visible = append(visible, n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inBody)
		}
	}
	walk(root, false)
	summary.Headings = summaryCap(strings.Join(headings, " | "), 300)
	summary.Text = summaryCap(strings.Join(visible, " "), 300)
	return summary
}

func summaryCap(s string, limit int) string {
	s = strings.Join(strings.Fields(strings.ToValidUTF8(s, "")), " ")
	if len(s) <= limit {
		return s
	}
	for !utf8.RuneStart(s[limit]) {
		limit--
	}
	return strings.TrimSpace(s[:limit])
}

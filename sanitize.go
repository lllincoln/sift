package main

import (
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const (
	maxContentBytes = 512 * 1024
	// Below this many characters a feed only has a teaser, so opening the
	// article goes straight to the original page.
	minReadableChars = 280
	snippetChars     = 220
)

var allowedTags = setOf("a", "abbr", "b", "blockquote", "br", "caption", "cite", "code", "del", "em",
	"figcaption", "figure", "h1", "h2", "h3", "h4", "h5", "h6", "hr", "i", "img", "ins", "kbd", "li",
	"mark", "ol", "p", "pre", "q", "s", "small", "strong", "sub", "sup", "table", "tbody", "td", "tfoot",
	"th", "thead", "tr", "u", "ul")

// droppedTags lose their children too; other unknown tags are unwrapped.
var droppedTags = setOf("applet", "audio", "button", "canvas", "embed", "form", "frameset", "head",
	"iframe", "input", "link", "math", "meta", "noscript", "object", "script", "select", "style", "svg",
	"template", "textarea", "title", "video")

func setOf(items ...string) map[string]bool {
	s := make(map[string]bool, len(items))
	for _, it := range items {
		s[it] = true
	}
	return s
}

// sanitize reduces untrusted feed HTML to an allowlist of formatting tags,
// and also returns its plain text.
func sanitize(raw, baseURL string) (clean, text string) {
	if strings.TrimSpace(raw) == "" {
		return "", ""
	}
	context := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := html.ParseFragment(strings.NewReader(raw), context)
	if err != nil {
		return "", ""
	}
	base, _ := url.Parse(baseURL)
	root := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	var plain strings.Builder
	for _, n := range nodes {
		cleanNode(root, n, base, &plain)
	}
	var out strings.Builder
	for c := root.FirstChild; c != nil; c = c.NextSibling {
		if html.Render(&out, c) != nil {
			return "", ""
		}
	}
	return out.String(), strings.Join(strings.Fields(plain.String()), " ")
}

func cleanNode(parent, n *html.Node, base *url.URL, plain *strings.Builder) {
	switch n.Type {
	case html.TextNode:
		parent.AppendChild(&html.Node{Type: html.TextNode, Data: n.Data})
		plain.WriteString(n.Data)
		return
	case html.ElementNode:
	default:
		return
	}
	if droppedTags[n.Data] || n.Namespace != "" {
		return
	}
	target := parent
	if allowedTags[n.Data] {
		attrs, keep := cleanAttrs(n, base)
		if !keep {
			return
		}
		target = &html.Node{Type: html.ElementNode, Data: n.Data, DataAtom: n.DataAtom, Attr: attrs}
		parent.AppendChild(target)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		cleanNode(target, c, base, plain)
	}
	plain.WriteByte(' ')
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func cleanAttrs(n *html.Node, base *url.URL) ([]html.Attribute, bool) {
	switch n.Data {
	case "a":
		if href := safeURL(attr(n, "href"), base); href != "" {
			return []html.Attribute{{Key: "href", Val: href}}, true
		}
		return nil, true // keep the link text
	case "img":
		src := safeURL(attr(n, "src"), base)
		if src == "" || attr(n, "width") == "1" || attr(n, "height") == "1" { // tracking pixels
			return nil, false
		}
		attrs := []html.Attribute{{Key: "src", Val: src}, {Key: "loading", Val: "lazy"}}
		if alt := attr(n, "alt"); alt != "" {
			attrs = append(attrs, html.Attribute{Key: "alt", Val: alt})
		}
		return attrs, true
	case "td", "th":
		var attrs []html.Attribute
		for _, key := range []string{"colspan", "rowspan"} {
			if v := attr(n, key); v != "" {
				attrs = append(attrs, html.Attribute{Key: key, Val: v})
			}
		}
		return attrs, true
	}
	return nil, true
}

// safeURL resolves ref against base, returning "" unless it is http(s).
func safeURL(ref string, base *url.URL) string {
	u, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return ""
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.String()
}

const arxivAbsPrefix = "https://arxiv.org/abs/"

var arxivPaperPath = regexp.MustCompile(`^/(?:abs|pdf|html)/(.+?)(?:v\d+)?(?:\.pdf)?$`)

// canonicalLink lets the same story published in several feeds be stored
// once, by resolving it and removing the fragment and tracking parameters.
func canonicalLink(link string, feedURL *url.URL) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return ""
	}
	if feedURL != nil {
		u = feedURL.ResolveReference(u)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	if m := arxivPaperPath.FindStringSubmatch(u.Path); m != nil && strings.TrimPrefix(u.Host, "www.") == "arxiv.org" {
		return arxivAbsPrefix + m[1] // any version or format of a paper is one story
	}
	u.Fragment = ""
	if u.RawQuery != "" {
		q := u.Query()
		removed := false
		for k := range q {
			if strings.HasPrefix(strings.ToLower(k), "utm_") || k == "smid" {
				q.Del(k)
				removed = true
			}
		}
		if removed {
			u.RawQuery = q.Encode()
		}
	}
	return u.String()
}

// truncate shortens s to about n characters at a word boundary.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := string(r[:n])
	if i := strings.LastIndexByte(cut, ' '); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,.;:-") + "…"
}

package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"html"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// entry is one article, normalized from RSS 2.0, RSS 1.0 (RDF) or Atom.
type entry struct {
	Link      string
	Title     string
	Content   string    // raw, unsanitized HTML
	Published time.Time // zero if the feed gave no usable date
}

type parsedFeed struct {
	Title   string
	Entries []entry
}

// xmlNode is used for elements that collide with extensions. encoding/xml
// matches `xml:"title"` in any namespace, so media:title or atom:link would
// otherwise overwrite the real value.
type xmlNode struct {
	XMLName xml.Name
	Type    string `xml:"type,attr"`
	Rel     string `xml:"rel,attr"`
	Href    string `xml:"href,attr"`
	Text    string `xml:",chardata"`
	Inner   string `xml:",innerxml"`
}

type rawItem struct {
	Title       []xmlNode `xml:"title"`
	Link        []xmlNode `xml:"link"`
	GUID        string    `xml:"guid"`
	ID          string    `xml:"id"`
	Description []xmlNode `xml:"description"`
	Summary     []xmlNode `xml:"summary"`
	Content     []xmlNode `xml:"content"`
	Encoded     string    `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
	PubDate     string    `xml:"pubDate"`
	Date        string    `xml:"http://purl.org/dc/elements/1.1/ date"`
	Published   string    `xml:"published"`
	Updated     string    `xml:"updated"`
}

type rawFeed struct {
	Title   []xmlNode `xml:"title"`
	Channel struct {
		Title []xmlNode `xml:"title"`
		Items []rawItem `xml:"item"`
	} `xml:"channel"`
	Items   []rawItem `xml:"item"` // RSS 1.0 keeps items outside the channel
	Entries []rawItem `xml:"entry"`
}

func isCoreNamespace(space string) bool {
	return space == "" || space == "http://purl.org/rss/1.0/" ||
		strings.Contains(space, "/Atom") || strings.Contains(space, "purl.org/atom")
}

// core returns the first non-empty element from the feed's own vocabulary.
func core(nodes []xmlNode) *xmlNode {
	for i := range nodes {
		if isCoreNamespace(nodes[i].XMLName.Space) && strings.TrimSpace(nodes[i].Text+nodes[i].Inner) != "" {
			return &nodes[i]
		}
	}
	return nil
}

func (n *xmlNode) html() string {
	if n == nil {
		return ""
	}
	switch strings.ToLower(n.Type) {
	case "xhtml":
		return n.Inner
	case "text":
		return html.EscapeString(n.Text)
	}
	return n.Text
}

func plainTitle(nodes []xmlNode) string {
	n := core(nodes)
	if n == nil {
		return ""
	}
	return strings.Join(strings.Fields(html.UnescapeString(n.Text)), " ")
}

func itemLink(it *rawItem) string {
	for _, l := range it.Link {
		if rel := strings.ToLower(l.Rel); l.Href != "" && (rel == "" || rel == "alternate") {
			return strings.TrimSpace(l.Href)
		}
	}
	if n := core(it.Link); n != nil && strings.TrimSpace(n.Text) != "" {
		return strings.TrimSpace(n.Text)
	}
	for _, id := range []string{it.GUID, it.ID} {
		if id = strings.TrimSpace(id); strings.HasPrefix(id, "http://") || strings.HasPrefix(id, "https://") {
			return id
		}
	}
	return ""
}

func itemContent(it *rawItem) string {
	if strings.TrimSpace(it.Encoded) != "" {
		return it.Encoded
	}
	for _, nodes := range [][]xmlNode{it.Content, it.Description, it.Summary} {
		if n := core(nodes); n != nil {
			return n.html()
		}
	}
	return ""
}

func parseFeed(body []byte) (*parsedFeed, error) {
	dec := xml.NewDecoder(bytes.NewReader(normalizeEncoding(body)))
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	// normalizeEncoding already produced UTF-8, whatever the prolog claims.
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }

	var raw rawFeed
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}

	items := raw.Entries
	items = append(items, raw.Channel.Items...)
	items = append(items, raw.Items...)
	title := plainTitle(raw.Title)
	if title == "" {
		title = plainTitle(raw.Channel.Title)
	}
	if len(items) == 0 && title == "" {
		return nil, errors.New("not an RSS or Atom feed")
	}

	feed := &parsedFeed{Title: title}
	for i := range items {
		it := &items[i]
		e := entry{Link: itemLink(it), Title: plainTitle(it.Title), Content: itemContent(it)}
		for _, d := range []string{it.Published, it.PubDate, it.Date, it.Updated} {
			if t, ok := parseDate(d); ok {
				e.Published = t
				break
			}
		}
		feed.Entries = append(feed.Entries, e)
	}
	return feed, nil
}

var xmlEncodingRe = regexp.MustCompile(`(?i)<\?xml[^>]*encoding=["']([^"']+)["']`)

// cp1252High maps bytes 0x80-0x9F. Above that, Windows-1252 equals Latin-1.
var cp1252High = [32]rune{
	'€', '\u0081', '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', '\u008d', 'Ž', '\u008f',
	'\u0090', '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', '\u009d', 'ž', 'Ÿ',
}

// normalizeEncoding returns valid UTF-8 with the control characters that make
// encoding/xml give up removed.
func normalizeEncoding(b []byte) []byte {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	head := b[:min(len(b), 256)]
	if m := xmlEncodingRe.FindSubmatch(head); m != nil {
		switch strings.ToLower(string(m[1])) {
		case "iso-8859-1", "latin1", "latin-1", "windows-1252", "cp1252":
			out := make([]byte, 0, len(b)+len(b)/16)
			for _, c := range b {
				r := rune(c)
				if c >= 0x80 && c < 0xA0 {
					r = cp1252High[c-0x80]
				}
				out = utf8.AppendRune(out, r)
			}
			b = out
		}
	}
	return bytes.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return -1
		}
		return r
	}, b)
}

var dateLayouts = []string{
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04 -0700",
	"2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	"2 Jan 2006 15:04:05 MST",
	time.RFC3339,
	"2006-01-02T15:04:05-0700",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// Go's parser accepts these abbreviations but treats them as UTC.
var usZones = strings.NewReplacer(
	" EST", " -0500", " EDT", " -0400", " CST", " -0600", " CDT", " -0500",
	" MST", " -0700", " MDT", " -0600", " PST", " -0800", " PDT", " -0700",
)

func parseDate(s string) (time.Time, bool) {
	s = usZones.Replace(strings.TrimSpace(s))
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

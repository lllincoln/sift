package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseRSS(t *testing.T) {
	feed, err := parseFeed([]byte(`<?xml version="1.0"?>
<rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom" xmlns:media="http://search.yahoo.com/mrss/"
     xmlns:content="http://purl.org/rss/1.0/modules/content/">
<channel><title>Example</title><atom:link rel="self" href="https://x.test/feed"/>
<item><title>First &amp; best</title><media:title>Wrong</media:title>
<link>https://x.test/1</link><atom:link rel="self" href="https://x.test/self"/>
<description>short</description><content:encoded><![CDATA[<p>long</p>]]></content:encoded>
<pubDate>Mon, 02 Jan 2006 15:04:05 EST</pubDate></item>
</channel></rss>`))
	if err != nil {
		t.Fatal(err)
	}
	if feed.Title != "Example" || len(feed.Entries) != 1 {
		t.Fatalf("got %+v", feed)
	}
	e := feed.Entries[0]
	if e.Title != "First & best" || e.Link != "https://x.test/1" || e.Content != "<p>long</p>" {
		t.Errorf("got %+v", e)
	}
	if want := time.Date(2006, 1, 2, 20, 4, 5, 0, time.UTC); !e.Published.Equal(want) {
		t.Errorf("date = %v, want %v", e.Published, want)
	}
}

func TestParseAtom(t *testing.T) {
	feed, err := parseFeed([]byte(`<feed xmlns="http://www.w3.org/2005/Atom"><title>A</title>
<entry><title>E</title><link rel="edit" href="https://x.test/edit"/><link rel="alternate" href="https://x.test/e"/>
<id>urn:x</id><updated>2024-01-02T03:04:05Z</updated>
<content type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml"><b>hi</b></div></content></entry></feed>`))
	if err != nil {
		t.Fatal(err)
	}
	e := feed.Entries[0]
	if e.Link != "https://x.test/e" || !strings.Contains(e.Content, "<b>hi</b>") || e.Published.IsZero() {
		t.Errorf("got %+v", e)
	}
}

func TestParseRDF(t *testing.T) {
	feed, err := parseFeed([]byte(`<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"
xmlns="http://purl.org/rss/1.0/" xmlns:dc="http://purl.org/dc/elements/1.1/">
<channel><title>R</title></channel>
<item><title>I</title><link>https://x.test/i</link><dc:date>2024-01-02T03:04:05Z</dc:date></item></rdf:RDF>`))
	if err != nil {
		t.Fatal(err)
	}
	if feed.Title != "R" || len(feed.Entries) != 1 || feed.Entries[0].Published.IsZero() {
		t.Errorf("got %+v", feed)
	}
}

func TestParseLatin1AndControlChars(t *testing.T) {
	feed, err := parseFeed([]byte("<?xml version=\"1.0\" encoding=\"windows-1252\"?><rss><channel><title>caf\xe9 \x93q\x94\x08</title></channel></rss>"))
	if err != nil {
		t.Fatal(err)
	}
	if feed.Title != "café “q”" {
		t.Errorf("title = %q", feed.Title)
	}
}

func TestParseRejectsHTML(t *testing.T) {
	if _, err := parseFeed([]byte(`<html><body><p>nope</p></body></html>`)); err == nil {
		t.Error("expected error")
	}
}

func TestSanitize(t *testing.T) {
	clean, text := sanitize(`<p onclick="x()" style="a">Hi <a href="javascript:alert(1)">bad</a>
<a href="/rel" onmouseover="x">good</a></p><script>alert(1)</script><svg><script>alert(2)</script></svg>
<iframe src="https://evil.test"></iframe><img src="x.png" onerror="y()"><img src="https://t.test/p" width="1">
<div><em>kept</em></div>`, "https://x.test/a/b")

	for _, bad := range []string{"onclick", "style", "javascript", "<script", "alert", "<iframe", "onerror", "onmouseover", "t.test", "<svg"} {
		if strings.Contains(clean, bad) {
			t.Errorf("output contains %q: %s", bad, clean)
		}
	}
	for _, good := range []string{`href="https://x.test/rel"`, `src="https://x.test/a/x.png"`, "<em>kept</em>"} {
		if !strings.Contains(clean, good) {
			t.Errorf("output missing %q: %s", good, clean)
		}
	}
	if strings.Contains(text, "alert") || !strings.Contains(text, "kept") {
		t.Errorf("text = %q", text)
	}
}

func TestCanonicalLink(t *testing.T) {
	base := parseURL("https://x.test/feed")
	cases := map[string]string{
		"https://x.test/a?utm_source=r&id=1#frag": "https://x.test/a?id=1",
		"/a":              "https://x.test/a",
		"mailto:a@x.test": "",
	}
	for in, want := range cases {
		if got := canonicalLink(in, base); got != want {
			t.Errorf("canonicalLink(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSaveFeedDedupesAndKeepsReadState(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var autoVacuum int
	if err := db.QueryRow(`PRAGMA auto_vacuum`).Scan(&autoVacuum); err != nil || autoVacuum != 2 {
		t.Errorf("auto_vacuum = %d, err %v", autoVacuum, err)
	}

	feeds, err := listFeeds(db)
	if err != nil || len(feeds) == 0 {
		t.Fatalf("seed: %d feeds, err %v", len(feeds), err)
	}
	a, b := feeds[0], feeds[1]
	now := time.Now()
	cutoff := now.Add(-30 * 24 * time.Hour)
	old := now.Add(-60 * 24 * time.Hour)
	parsed := &parsedFeed{Title: "T", Entries: []entry{
		{Link: "https://x.test/1", Title: "One", Published: now.Add(-time.Hour)},
		{Link: "https://x.test/old", Title: "Old", Published: old},
		{Link: "https://x.test/undated", Title: "Undated"},
	}}
	if err := saveFeed(db, a, parsed, "", "", now, cutoff); err != nil {
		t.Fatal(err)
	}
	unread := func() []Item {
		items, err := listItems(db, itemFilter{UnreadOnly: true, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		return items
	}
	items := unread()
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1 (old skipped, undated back catalogue read)", len(items))
	}

	if err := setRead(db, items[0].ID, true); err != nil {
		t.Fatal(err)
	}
	// The same stories arriving through a second feed must not reappear.
	if err := saveFeed(db, b, parsed, "", "", now, cutoff); err != nil {
		t.Fatal(err)
	}
	if got := unread(); len(got) != 0 {
		t.Errorf("duplicates came back unread: %+v", got)
	}

	// An undated story that appears after the first fetch is new.
	if err := saveFeed(db, a, &parsedFeed{Entries: []entry{{Link: "https://x.test/new", Title: "New"}}}, "", "", now, cutoff); err != nil {
		t.Fatal(err)
	}
	fresh := unread()
	if len(fresh) != 1 || fresh[0].Title != "New" || time.Since(fresh[0].Published) > time.Minute {
		t.Errorf("new undated story: %+v", fresh)
	}
	if err := setRead(db, fresh[0].ID, true); err != nil {
		t.Fatal(err)
	}

	// An undated story still in its feed survives past the retention window.
	later := now.Add(45 * 24 * time.Hour)
	if err := saveFeed(db, a, &parsedFeed{Entries: []entry{{Link: "https://x.test/undated", Title: "Undated"}}}, "", "", later, later.Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := prune(db, later.Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	all, err := listItems(db, itemFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Title != "Undated" || !all[0].Read {
		t.Errorf("after prune: %+v", all)
	}
}

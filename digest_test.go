package main

import (
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cand(id int64, publisher, title string, age time.Duration, now time.Time) *candidate {
	return &candidate{
		Item:       Item{ID: id, Title: title, Published: now.Add(-age)},
		publisher:  publisher,
		publishers: map[string]bool{publisher: true},
		perWeek:    20,
	}
}

func TestPublisherOf(t *testing.T) {
	cases := map[string]string{
		"https://rss.nytimes.com/services/xml/rss/nyt/World.xml": "nytimes.com",
		"https://feeds.bbci.co.uk/news/world/rss.xml":            "bbci.co.uk",
		"https://thezvi.substack.com/feed":                       "thezvi.substack.com",
		"https://hnrss.org/best":                                 "hnrss.org",
		"https://geohot.github.io/blog/feed.xml":                 "geohot.github.io",
		"https://terrytao.wordpress.com/feed/":                   "terrytao.wordpress.com",
	}
	for in, want := range cases {
		if got := publisherOf(in); got != want {
			t.Errorf("publisherOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDigestMergesCoverageAndBoostsIt(t *testing.T) {
	now := time.Now()
	got := selectDigest([]*candidate{
		cand(1, "a.com", "Quiet local bakery wins regional award", time.Minute, now),
		cand(2, "b.com", "Senate passes sweeping border funding bill", 6*time.Hour, now),
		cand(3, "c.com", "Senate passes border funding bill after long debate", 6*time.Hour, now),
		cand(4, "d.com", "Border funding bill passes the Senate", 6*time.Hour, now),
	}, now, 10, nil)
	if len(got) != 2 {
		t.Fatalf("got %d stories, want 2: %+v", len(got), got)
	}
	if got[0].Outlets != 3 || !strings.Contains(got[0].Title, "Senate") {
		t.Errorf("first = %+v, want the Senate story with 3 outlets", got[0])
	}
}

func TestDigestLimitsPublishersAndTopics(t *testing.T) {
	now := time.Now()
	var cands []*candidate
	for i := range 10 {
		cands = append(cands, cand(int64(i), "wire.com", fmt.Sprintf("Unrelated headline number%d alpha%d", i, i), time.Hour, now))
	}
	for i := range 20 {
		cands = append(cands, cand(int64(100+i), fmt.Sprintf("s%d.com", i), fmt.Sprintf("OpenAI item%d thing%d", i, i), time.Hour, now))
	}
	cands = append(cands, cand(200, "other.com", "Gardeners rediscover heritage tomatoes", 50*time.Hour, now))

	got := selectDigest(cands, now, 14, nil)
	wire, ai, tomatoes := 0, 0, false
	for _, it := range got {
		wire += strings.Count(it.Title, "Unrelated")
		ai += strings.Count(it.Title, "OpenAI")
		tomatoes = tomatoes || strings.Contains(it.Title, "tomatoes")
	}
	if wire > digestMaxPerSource || ai > digestMaxPerTopic || !tomatoes {
		t.Errorf("wire=%d ai=%d tomatoes=%v: %+v", wire, ai, tomatoes, got)
	}
}

func TestDigestPrefersFreeVersions(t *testing.T) {
	now := time.Now()
	hn := cand(4, "hnrss.org", "Why the harbour cranes stopped", time.Hour, now)
	hn.Link = "https://www.wsj.com/articles/cranes"
	got := selectDigest([]*candidate{
		cand(1, "nytimes.com", "Senate passes sweeping border funding bill", time.Hour, now),
		cand(2, "bbci.co.uk", "Senate passes border funding bill after debate", 5*time.Hour, now),
		hn,
		cand(3, "a.com", "A quiet history of canals", 40*time.Hour, now),
	}, now, 10, nil)
	if len(got) != 3 {
		t.Fatalf("got %d stories: %+v", len(got), got)
	}
	if got[0].ID != 2 || got[0].Outlets != 2 {
		t.Errorf("want the free version leading with 2 outlets, got %+v", got[0])
	}
	if got[2].ID != 4 {
		t.Errorf("want the paywalled link last, got %+v", got)
	}
}

func TestDigestPrefersIndependentWriters(t *testing.T) {
	now := time.Now()
	cands := []*candidate{cand(1, "someblog.net", "Notes on restoring a lathe", 24*time.Hour, now)}
	for i, p := range []string{"cnbc.com", "npr.org", "bbci.co.uk", "arstechnica.com"} {
		for j := range 3 {
			cands = append(cands, cand(int64(10*i+j+10), p, fmt.Sprintf("Headline%d word%d item%d%d", i, j, i, j), 2*time.Hour, now))
		}
	}
	got := selectDigest(cands, now, len(cands), nil)
	if got[0].ID != 1 {
		t.Errorf("first = %+v, want the independent post", got[0])
	}
	if len(got) != len(cands) {
		t.Errorf("got %d of %d stories; limits should reorder, not remove", len(got), len(cands))
	}
}

func TestDigestDemotesRoutinePosts(t *testing.T) {
	now := time.Now()
	got := selectDigest([]*candidate{
		cand(1, "a.com", "Open Thread 400", time.Minute, now),
		cand(2, "b.com", "The surprising history of canals", 10*time.Hour, now),
	}, now, 1, nil)
	if !strings.Contains(got[0].Title, "canals") {
		t.Errorf("got %+v", got[0])
	}
}

func TestMigrationReplacesDeadFeedsAndCountsSources(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Recreate a version 1 database holding a dead feed and a story.
	for _, q := range []string{
		`DROP TABLE item_sources`,
		`DROP TABLE ratings`,
		`DROP TABLE reads`,
		`INSERT INTO feeds(url) VALUES ('https://avc.com/feed/')`,
		`DELETE FROM feeds WHERE url = 'https://avc.xyz/feed'`,
		`DELETE FROM feeds WHERE url = 'https://terrytao.wordpress.com/feed/'`,
		`INSERT INTO items(feed_id, link, title, snippet, content, published, last_seen)
		 SELECT MIN(id), 'https://x.test/1', 'T', '', '', 1, 1 FROM feeds`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	var added int
	if err := db.QueryRow(`SELECT COUNT(*) FROM feeds WHERE url = 'https://terrytao.wordpress.com/feed/'`).Scan(&added); err != nil || added != 1 {
		t.Errorf("independent feed added = %d, err %v", added, err)
	}
	var dead, replaced, sources int
	if err := db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM feeds WHERE url = 'https://avc.com/feed/'),
		(SELECT COUNT(*) FROM feeds WHERE url = 'https://avc.xyz/feed'),
		(SELECT COUNT(*) FROM item_sources)`).Scan(&dead, &replaced, &sources); err != nil {
		t.Fatal(err)
	}
	if dead != 0 || replaced != 1 || sources != 1 {
		t.Errorf("dead=%d replaced=%d sources=%d", dead, replaced, sources)
	}
}

func TestCanonicalLinkMergesArxivVersions(t *testing.T) {
	for _, in := range []string{"https://arxiv.org/pdf/2610.08144v2.pdf", "http://www.arxiv.org/abs/2610.08144v1", "https://arxiv.org/html/2610.08144"} {
		if got := canonicalLink(in, nil); got != "https://arxiv.org/abs/2610.08144" {
			t.Errorf("canonicalLink(%q) = %q", in, got)
		}
	}
}

func TestPapersStayLongerThanNews(t *testing.T) {
	now := time.Now()
	paper := cand(1, "arxiv.org", "On subcubic shortest paths", 7*24*time.Hour, now)
	paper.Link = arxivAbsPrefix + "2610.1"
	news := cand(2, "b.net", "Harbour festival returns", 7*24*time.Hour, now)
	got := selectDigest([]*candidate{news, paper}, now, 2, nil)
	if got[0].ID != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestPreferencesShiftRanking(t *testing.T) {
	now := time.Now()
	stories := func() []*candidate {
		a := cand(1, "a.net", "Compiler optimizations revisited again", time.Hour, now)
		a.feedURL = "https://a.net/feed"
		b := cand(2, "b.net", "Celebrity fashion trends this autumn", time.Hour, now)
		b.feedURL = "https://b.net/feed"
		return []*candidate{b, a}
	}
	likes := newPreferences()
	dislikes := newPreferences()
	for range 3 {
		likes.record("https://a.net/feed", "Compiler internals explained", 1)
		likes.record("https://b.net/feed", "Celebrity gossip roundup", -1)
		dislikes.record("https://b.net/feed", "Celebrity gossip roundup", 1)
		dislikes.record("https://a.net/feed", "Compiler internals explained", -1)
	}
	if got := selectDigest(stories(), now, 2, likes); got[0].ID != 1 {
		t.Errorf("liked story should lead: %+v", got)
	}
	if got := selectDigest(stories(), now, 2, dislikes); got[0].ID != 2 {
		t.Errorf("preference should flip the order: %+v", got)
	}
}

func TestRatingsReadsAndPreferences(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	feeds, err := listFeeds(db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	parsed := &parsedFeed{Entries: []entry{
		{Link: "https://x.test/1", Title: "One", Published: now},
		{Link: "https://x.test/2", Title: "Two", Published: now},
	}}
	if err := saveFeed(db, feeds[0], parsed, "", "", now, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	items, err := listItems(db, itemFilter{Limit: 2})
	if err != nil || len(items) != 2 {
		t.Fatalf("%v %v", items, err)
	}
	id := items[0].ID
	rating := func() int {
		it, err := getItem(db, id)
		if err != nil {
			t.Fatal(err)
		}
		return it.Rating
	}
	if err := toggleRating(db, id, 1, now); err != nil || rating() != 1 {
		t.Fatalf("like: rating %d, err %v", rating(), err)
	}
	if err := toggleRating(db, id, 1, now); err != nil || rating() != 0 {
		t.Fatalf("toggle off: rating %d, err %v", rating(), err)
	}
	if err := toggleRating(db, id, -1, now); err != nil || rating() != -1 {
		t.Fatalf("dislike: rating %d, err %v", rating(), err)
	}
	if it, _ := getItem(db, id); !it.Read {
		t.Error("dislike should mark the story read")
	}

	// Reading time is only a statistic: it must not affect ranking.
	for _, itemID := range []int64{id, items[1].ID} {
		if err := recordRead(db, itemID, 300, now); err != nil {
			t.Fatal(err)
		}
	}
	prefs, err := loadPreferences(db)
	if err != nil {
		t.Fatal(err)
	}
	if got := prefs.feeds[feeds[0].URL]; got.up != 0 || got.down != 1 {
		t.Errorf("tally = %+v, want only the one dislike", got)
	}

	if err := prune(db, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var kept int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM ratings) + (SELECT COUNT(*) FROM reads)`).Scan(&kept); err != nil || kept != 3 {
		t.Errorf("rows after prune = %d, err %v", kept, err)
	}
}

func TestDigestPages(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	feeds, _ := listFeeds(db)
	now := time.Now()
	parsed := &parsedFeed{}
	for i := range 65 {
		parsed.Entries = append(parsed.Entries, entry{Link: fmt.Sprintf("https://x.test/%d", i), Title: fmt.Sprintf("Word%d distinct%d title%d", i, i, i), Published: now})
	}
	if err := saveFeed(db, feeds[0], parsed, "", "", now, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	app := &App{db: db}
	get := func(url string) string {
		rec := httptest.NewRecorder()
		app.routes().ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", url, rec.Code)
		}
		return rec.Body.String()
	}
	for url, want := range map[string][2]string{
		"/":        {"30", "Page 1 of 3"},
		"/?page=3": {"5", "Page 3 of 3"},
		"/?page=9": {"5", "Page 3 of 3"},
	} {
		body := get(url)
		if got := fmt.Sprint(strings.Count(body, `class="story"`)); got != want[0] || !strings.Contains(body, want[1]) {
			t.Errorf("%s: %s stories, want %s and %q", url, got, want[0], want[1])
		}
	}
	if body := get("/?page=2"); !strings.Contains(body, `start="31"`) || !strings.Contains(body, `/?page=2#item-`) {
		t.Error("page 2 should continue numbering and return votes to itself")
	}
}

package main

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadTracker(t *testing.T) {
	start := time.Now()
	tr := readTracker{timeout: 20 * time.Minute}
	at := func(d time.Duration) time.Time { return start.Add(d) }

	if _, _, ok := tr.navigate(1, at(0)); ok {
		t.Error("nothing was being read yet")
	}
	if _, _, ok := tr.navigate(1, at(time.Minute)); ok {
		t.Error("a reload shouldn't finish the read")
	}
	if id, secs, ok := tr.navigate(0, at(5*time.Minute)); !ok || id != 1 || secs != 300 {
		t.Errorf("got %d, %d, %v; want the full 5 minutes from first open", id, secs, ok)
	}
	if _, _, ok := tr.navigate(0, at(6*time.Minute)); ok {
		t.Error("list pages aren't reads")
	}
	tr.navigate(2, at(10*time.Minute))
	if id, secs, ok := tr.navigate(3, at(31*time.Minute)); !ok || id != 2 || secs != 0 {
		t.Errorf("got %d, %d, %v; a gap over the timeout counts the read with no time", id, secs, ok)
	}
	if id, secs, ok := tr.navigate(0, at(32*time.Minute)); !ok || id != 3 || secs != 60 {
		t.Errorf("got %d, %d, %v; opening an article after a gap should still time it", id, secs, ok)
	}
}

func TestStreaks(t *testing.T) {
	cases := []struct {
		days          []int64
		today         int64
		current, best int
	}{
		{nil, 10, 0, 0},
		{[]int64{1, 2, 3, 7, 8}, 8, 2, 3},
		{[]int64{7, 8}, 9, 2, 2},
		{[]int64{7, 8}, 10, 0, 2},
	}
	for _, c := range cases {
		if current, best := streaks(c.days, c.today); current != c.current || best != c.best {
			t.Errorf("streaks(%v, %d) = %d, %d; want %d, %d", c.days, c.today, current, best, c.current, c.best)
		}
	}
}

func TestStatsPage(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	app := &App{db: db}
	get := func() string {
		rec := httptest.NewRecorder()
		app.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/stats", nil))
		if rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	if !strings.Contains(get(), "Read or rate something") {
		t.Error("empty state missing")
	}

	now := time.Now()
	feeds, _ := listFeeds(db)
	parsed := &parsedFeed{Entries: []entry{{Link: "https://x.test/1", Title: "A remarkable essay", Published: now}}}
	if err := saveFeed(db, feeds[0], parsed, "", "", now, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	items, _ := listItems(db, itemFilter{Limit: 1})
	if err := recordRead(db, items[0].ID, 90, now); err != nil {
		t.Fatal(err)
	}
	if err := toggleRating(db, items[0].ID, 1, now); err != nil {
		t.Fatal(err)
	}
	body := get()
	for _, want := range []string{"A remarkable essay", "1m", feeds[0].URL, `<dd>1d</dd>`} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

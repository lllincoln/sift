package main

import (
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed defaultlist.txt
var defaultFeeds string

const schema = `
CREATE TABLE feeds (
	id            INTEGER PRIMARY KEY,
	url           TEXT NOT NULL UNIQUE,
	title         TEXT NOT NULL DEFAULT '',
	etag          TEXT NOT NULL DEFAULT '',
	last_modified TEXT NOT NULL DEFAULT '',
	last_fetch    INTEGER NOT NULL DEFAULT 0,
	error         TEXT NOT NULL DEFAULT ''
);
CREATE TABLE items (
	id        INTEGER PRIMARY KEY,
	feed_id   INTEGER NOT NULL REFERENCES feeds(id) ON DELETE CASCADE,
	link      TEXT NOT NULL UNIQUE,
	title     TEXT NOT NULL,
	snippet   TEXT NOT NULL,
	content   TEXT NOT NULL,
	published INTEGER NOT NULL,
	last_seen INTEGER NOT NULL,
	read      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX items_timeline ON items(published DESC, id DESC);
CREATE INDEX items_unread ON items(read, published DESC, id DESC);
CREATE INDEX items_feed ON items(feed_id, published DESC, id DESC);
`

// schemaItemSources records every feed that carried a story, since items are
// stored once per link. The digest uses it to measure how widely one spread.
const schemaItemSources = `
CREATE TABLE item_sources (
	item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	feed_id INTEGER NOT NULL REFERENCES feeds(id) ON DELETE CASCADE,
	PRIMARY KEY (item_id, feed_id)
) WITHOUT ROWID;
CREATE INDEX item_sources_feed ON item_sources(feed_id);
INSERT INTO item_sources(item_id, feed_id) SELECT id, feed_id FROM items;
`

// independentHeading marks the part of defaultlist.txt added in version 3.
const independentHeading = "# Independent writers"

// schemaRatings is keyed by link, not item id, so taste outlives the 30-day
// item retention and the removal of a feed.
const schemaRatings = `
CREATE TABLE ratings (
	link     TEXT PRIMARY KEY,
	feed_url TEXT NOT NULL,
	title    TEXT NOT NULL,
	rating   INTEGER NOT NULL,
	rated_at INTEGER NOT NULL
) WITHOUT ROWID;
`

// arxivPicksFeed is arXiv links that Hacker News voted to 30 points or more.
const arxivPicksFeed = "https://hnrss.org/newest?q=arxiv.org&search_attrs=url&points=30&count=100"

// schemaReads holds one row per timed read, keyed for the same reason as ratings.
const schemaReads = `
CREATE TABLE reads (
	id       INTEGER PRIMARY KEY,
	link     TEXT NOT NULL,
	feed_url TEXT NOT NULL,
	title    TEXT NOT NULL,
	seconds  INTEGER NOT NULL,
	read_at  INTEGER NOT NULL
);
CREATE INDEX reads_time ON reads(read_at);
CREATE INDEX reads_link ON reads(link);
`

const schemaVersion = 5

var movedFeeds = map[string]string{
	"https://avc.com/feed/":                     "https://avc.xyz/feed",
	"https://a16z.com/feed/":                    "https://www.a16z.news/feed",
	"https://review.firstround.com/feed.xml":    "https://review.firstround.com/articles/rss/",
	"https://worksinprogress.co/feed/":          "https://worksinprogress.co/rss.xml",
	"https://www.newthingsunderthesun.com/feed": "https://www.newthingsunderthesun.com/rss.xml",
}

func openDB(path string) (*sql.DB, error) {
	// auto_vacuum must precede table creation, so it stays first.
	dsn := "file:" + path + "?_pragma=auto_vacuum(INCREMENTAL)&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: no lock contention, and little memory on small boards.
	// Rows must therefore be closed before running another query.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version >= schemaVersion {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if version < 1 {
		if _, err := tx.Exec(schema); err != nil {
			return err
		}
		// Seeded once, so removing every feed later doesn't bring them back.
		if err := insertFeedList(tx, defaultFeeds); err != nil {
			return err
		}
	}
	if version < 2 {
		if _, err := tx.Exec(schemaItemSources); err != nil {
			return err
		}
		// Databases seeded from the original list hold these dead URLs.
		for from, to := range movedFeeds {
			var exists bool
			if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM feeds WHERE url = ?)`, from).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				continue
			}
			if _, err := tx.Exec(`DELETE FROM feeds WHERE url = ?`, from); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO feeds(url) VALUES (?)`, to); err != nil {
				return err
			}
		}
	}
	if version >= 1 && version < 3 {
		_, added, _ := strings.Cut(defaultFeeds, independentHeading)
		if err := insertFeedList(tx, added); err != nil {
			return err
		}
	}
	if version < 5 {
		if _, err := tx.Exec(schemaReads); err != nil {
			return err
		}
	}
	if version < 4 {
		if _, err := tx.Exec(schemaRatings); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO feeds(url) VALUES (?)`, arxivPicksFeed); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

func insertFeedList(tx *sql.Tx, list string) error {
	for _, line := range strings.Split(list, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO feeds(url) VALUES (?)`, line); err != nil {
			return err
		}
	}
	return nil
}

type Feed struct {
	ID           int64
	URL          string
	Title        string
	ETag         string
	LastModified string
	LastFetch    time.Time
	Error        string
	Unread       int
}

// Name is the feed's title, or its URL until the first successful fetch.
func (f Feed) Name() string {
	if f.Title != "" {
		return f.Title
	}
	return f.URL
}

type Item struct {
	ID        int64
	FeedID    int64
	FeedName  string
	Link      string
	Title     string
	Snippet   string
	Content   string
	Published time.Time
	Read      bool
	Rating    int // 1 liked, -1 disliked, 0 neither
}

func listFeeds(db *sql.DB) ([]Feed, error) {
	rows, err := db.Query(`
		SELECT f.id, f.url, f.title, f.etag, f.last_modified, f.last_fetch, f.error,
		       (SELECT COUNT(*) FROM items i WHERE i.feed_id = f.id AND i.read = 0)
		FROM feeds f ORDER BY COALESCE(NULLIF(f.title, ''), f.url) COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var feeds []Feed
	for rows.Next() {
		var f Feed
		var last int64
		if err := rows.Scan(&f.ID, &f.URL, &f.Title, &f.ETag, &f.LastModified, &last, &f.Error, &f.Unread); err != nil {
			return nil, err
		}
		if last > 0 {
			f.LastFetch = time.Unix(last, 0)
		}
		feeds = append(feeds, f)
	}
	return feeds, rows.Err()
}

type cursor struct {
	Published int64
	ID        int64
}

type itemFilter struct {
	UnreadOnly bool
	FeedID     int64
	Before     *cursor
	Limit      int
}

func listItems(db *sql.DB, filter itemFilter) ([]Item, error) {
	query := `SELECT i.id, i.feed_id, COALESCE(NULLIF(f.title, ''), f.url), i.title, i.snippet, i.published, i.read, COALESCE(r.rating, 0)
		FROM items i JOIN feeds f ON f.id = i.feed_id LEFT JOIN ratings r ON r.link = i.link WHERE 1=1`
	var args []any
	if filter.UnreadOnly {
		query += ` AND i.read = 0`
	}
	if filter.FeedID != 0 {
		query += ` AND i.feed_id = ?`
		args = append(args, filter.FeedID)
	}
	if filter.Before != nil {
		query += ` AND (i.published, i.id) < (?, ?)`
		args = append(args, filter.Before.Published, filter.Before.ID)
	}
	query += ` ORDER BY i.published DESC, i.id DESC LIMIT ?`
	args = append(args, filter.Limit)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		var it Item
		var published int64
		if err := rows.Scan(&it.ID, &it.FeedID, &it.FeedName, &it.Title, &it.Snippet, &published, &it.Read, &it.Rating); err != nil {
			return nil, err
		}
		it.Published = time.Unix(published, 0)
		items = append(items, it)
	}
	return items, rows.Err()
}

func getItem(db *sql.DB, id int64) (*Item, error) {
	var it Item
	var published int64
	err := db.QueryRow(`
		SELECT i.id, i.feed_id, COALESCE(NULLIF(f.title, ''), f.url), i.link, i.title, i.snippet, i.content, i.published, i.read, COALESCE(r.rating, 0)
		FROM items i JOIN feeds f ON f.id = i.feed_id LEFT JOIN ratings r ON r.link = i.link WHERE i.id = ?`, id).
		Scan(&it.ID, &it.FeedID, &it.FeedName, &it.Link, &it.Title, &it.Snippet, &it.Content, &published, &it.Read, &it.Rating)
	if err != nil {
		return nil, err
	}
	it.Published = time.Unix(published, 0)
	return &it, nil
}

// toggleRating applies a vote, or clears it if the same vote is repeated.
// A dislike also marks the story read, since it's been dismissed.
func toggleRating(db *sql.DB, itemID int64, rating int, now time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var link, title, feedURL string
	var current int
	if err := tx.QueryRow(`
		SELECT i.link, i.title, f.url, COALESCE(r.rating, 0)
		FROM items i JOIN feeds f ON f.id = i.feed_id LEFT JOIN ratings r ON r.link = i.link
		WHERE i.id = ?`, itemID).Scan(&link, &title, &feedURL, &current); err != nil {
		return err
	}
	if current == rating {
		if _, err := tx.Exec(`DELETE FROM ratings WHERE link = ?`, link); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO ratings(link, feed_url, title, rating, rated_at) VALUES (?, ?, ?, ?, ?)`,
		link, feedURL, title, rating, now.Unix()); err != nil {
		return err
	}
	if rating < 0 {
		if _, err := tx.Exec(`UPDATE items SET read = 1 WHERE id = ?`, itemID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func setRead(db *sql.DB, id int64, read bool) error {
	_, err := db.Exec(`UPDATE items SET read = ? WHERE id = ?`, read, id)
	return err
}

// markAllRead stops at upTo so stories that arrived after the page was
// rendered, which the reader hasn't seen, stay unread.
func markAllRead(db *sql.DB, feedID, upTo int64) error {
	query := `UPDATE items SET read = 1 WHERE read = 0 AND id <= ?`
	args := []any{upTo}
	if feedID != 0 {
		query += ` AND feed_id = ?`
		args = append(args, feedID)
	}
	_, err := db.Exec(query, args...)
	return err
}

func maxItemID(db *sql.DB) (int64, error) {
	var id sql.NullInt64
	err := db.QueryRow(`SELECT MAX(id) FROM items`).Scan(&id)
	return id.Int64, err
}

func lastUpdated(db *sql.DB) (time.Time, error) {
	var last sql.NullInt64
	err := db.QueryRow(`SELECT MAX(last_fetch) FROM feeds`).Scan(&last)
	if err != nil || last.Int64 == 0 {
		return time.Time{}, err
	}
	return time.Unix(last.Int64, 0), nil
}

func addFeed(db *sql.DB, feedURL string) (int64, error) {
	if _, err := db.Exec(`INSERT OR IGNORE INTO feeds(url) VALUES (?)`, feedURL); err != nil {
		return 0, err
	}
	var id int64
	err := db.QueryRow(`SELECT id FROM feeds WHERE url = ?`, feedURL).Scan(&id)
	return id, err
}

func deleteFeed(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM feeds WHERE id = ?`, id)
	return err
}

func recordFetchError(db *sql.DB, id int64, now time.Time, msg string) error {
	_, err := db.Exec(`UPDATE feeds SET last_fetch = ?, error = ? WHERE id = ?`, now.Unix(), msg, id)
	return err
}

func recordNotModified(db *sql.DB, id int64, now time.Time) error {
	return recordFetchError(db, id, now, "")
}

// saveFeed stores a successful fetch. Dated entries older than cutoff are
// skipped: once pruned, they must not return as unread on the next fetch.
func saveFeed(db *sql.DB, feed Feed, parsed *parsedFeed, etag, lastModified string, now, cutoff time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var hasItems bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM items WHERE feed_id = ?)`, feed.ID).Scan(&hasItems); err != nil {
		return err
	}

	base := parseURL(feed.URL)
	for _, e := range parsed.Entries {
		link := canonicalLink(e.Link, base)
		if link == "" || (!e.Published.IsZero() && e.Published.Before(cutoff)) {
			continue
		}
		published, read := e.Published, false
		switch {
		case published.IsZero() && !hasItems:
			// An undated feed's first fetch is its whole back catalogue, not
			// news: file it at the bottom of the timeline, already read.
			published, read = cutoff, true
		case published.IsZero() || published.After(now):
			published = now // a future date shouldn't pin a story to the top
		}
		content, text := sanitize(e.Content, link)
		title := e.Title
		if title == "" {
			title = truncate(text, 80)
		}
		if title == "" {
			title = "(untitled)"
		}
		snippet := truncate(text, snippetChars)
		if strings.HasPrefix(text, "Article URL:") { // hnrss metadata, not a summary
			snippet = ""
		}
		if len([]rune(text)) < minReadableChars || len(content) > maxContentBytes {
			content = ""
		}
		var itemID int64
		if err := tx.QueryRow(`
			INSERT INTO items(feed_id, link, title, snippet, content, published, last_seen, read)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(link) DO UPDATE SET last_seen = excluded.last_seen
			RETURNING id`,
			feed.ID, link, title, snippet, content, published.Unix(), now.Unix(), read).Scan(&itemID); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO item_sources(item_id, feed_id) VALUES (?, ?)`, itemID, feed.ID); err != nil {
			return err
		}
	}
	title := parsed.Title
	if title == "" {
		title = feed.Title
	}
	if feed.URL == arxivPicksFeed {
		title = "arXiv: top papers"
	}
	if _, err := tx.Exec(`UPDATE feeds SET title = ?, etag = ?, last_modified = ?, last_fetch = ?, error = '' WHERE id = ?`,
		title, etag, lastModified, now.Unix(), feed.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// prune removes aged-out items that have also left their feed, and returns
// the freed pages to the filesystem. Undated items are given a first-seen
// date, so the last_seen check stops them from being pruned while still
// listed and then re-added as new.
func prune(db *sql.DB, cutoff time.Time) error {
	if _, err := db.Exec(`DELETE FROM items WHERE published < ? AND last_seen < ?`, cutoff.Unix(), cutoff.Unix()); err != nil {
		return err
	}
	_, err := db.Exec(`PRAGMA incremental_vacuum`)
	return err
}

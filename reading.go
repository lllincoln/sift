package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// readTracker times the open article. With one reader, the next page load
// marks the end of the last article, including ones that opened on the
// publisher's site, where the app can't run any code.
type readTracker struct {
	mu      sync.Mutex
	itemID  int64
	started time.Time
	timeout time.Duration
}

// navigate records a move to itemID (0 for any non-article page) and returns
// the finished read, if there was one. A gap beyond the timeout means the app
// was closed, so the article counts as read but with 0 seconds.
func (t *readTracker) navigate(itemID int64, now time.Time) (finishedID int64, seconds int, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if itemID != 0 && itemID == t.itemID {
		return 0, 0, false // a reload continues the same read
	}
	prevID, prevStart := t.itemID, t.started
	t.itemID, t.started = itemID, now
	if prevID == 0 {
		return 0, 0, false
	}
	elapsed := now.Sub(prevStart)
	if elapsed > t.timeout {
		return prevID, 0, true
	}
	return prevID, int(elapsed.Round(time.Second).Seconds()), true
}

func (a *App) navigate(itemID int64) {
	now := time.Now()
	finished, seconds, ok := a.reading.navigate(itemID, now)
	if !ok {
		return
	}
	if err := recordRead(a.db, finished, seconds, now); err != nil {
		log.Printf("record read: %v", err)
	}
}

// page wraps handlers for non-article pages, which end the current read.
func (a *App) page(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.navigate(0)
		h(w, r)
	}
}

func recordRead(db *sql.DB, itemID int64, seconds int, now time.Time) error {
	_, err := db.Exec(`
		INSERT INTO reads(link, feed_url, title, seconds, read_at)
		SELECT i.link, f.url, i.title, ?, ? FROM items i JOIN feeds f ON f.id = i.feed_id WHERE i.id = ?`,
		seconds, now.Unix(), itemID)
	return err
}

func formatDuration(seconds int) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	}
	return fmt.Sprintf("%dh %dm", seconds/3600, seconds%3600/60)
}

package main

import (
	"bytes"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

//go:embed templates/*.html static/style.css static/app.js
var assets embed.FS

const pageSize = 50

var templates = map[string]*template.Template{}

func init() {
	funcs := template.FuncMap{
		"duration": formatDuration,
		"votes": func(id int64, rating int, next string) map[string]any {
			return map[string]any{"ID": id, "Rating": rating, "Next": next}
		},
		"ago":    ago,
		"unsafe": func(sanitized string) template.HTML { return template.HTML(sanitized) },
		"date":   func(t time.Time) string { return t.UTC().Format("January 2, 2006") },
		"host": func(s string) string {
			if u := parseURL(s); u != nil {
				return strings.TrimPrefix(u.Host, "www.")
			}
			return s
		},
	}
	for _, page := range []string{"list", "article", "feeds", "digest", "stats"} {
		templates[page] = template.Must(template.New("layout.html").Funcs(funcs).
			ParseFS(assets, "templates/layout.html", "templates/"+page+".html"))
	}
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	return t.UTC().Format("Jan 2, 2006")
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.page(a.handleDigest))
	mux.HandleFunc("GET /latest", a.page(a.handleList))
	mux.HandleFunc("GET /stats", a.page(a.handleStats))
	mux.HandleFunc("GET /article/{id}", a.handleArticle)
	mux.HandleFunc("POST /article/{id}/unread", a.handleMarkUnread)
	mux.HandleFunc("POST /article/{id}/rate", a.handleRate)
	mux.HandleFunc("POST /read-all", a.handleReadAll)
	mux.HandleFunc("POST /refresh", a.handleRefresh)
	mux.HandleFunc("GET /feeds", a.page(a.handleFeeds))
	mux.HandleFunc("POST /feeds", a.handleAddFeed)
	mux.HandleFunc("POST /feeds/{id}/delete", a.handleDeleteFeed)
	mux.HandleFunc("GET /style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		http.ServeFileFS(w, r, assets, "static/style.css")
	})
	mux.HandleFunc("GET /app.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		http.ServeFileFS(w, r, assets, "static/app.js")
	})
	return securityHeaders(http.NewCrossOriginProtection().Handler(mux))
}

// securityHeaders allows only the app's own script file, so a sanitizer miss
// in feed content still couldn't execute.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; img-src http: https: data:; style-src 'self'; script-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func (a *App) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := templates[name].Execute(&buf, data); err != nil {
		serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store") // read state must be fresh after Back
	buf.WriteTo(w)
}

func serverError(w http.ResponseWriter, err error) {
	log.Printf("error: %v", err)
	http.Error(w, "Something went wrong.", http.StatusInternalServerError)
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil
}

// listURL builds a timeline link, leaving out defaults.
func listURL(showAll bool, feedID int64, before string) string {
	q := url.Values{}
	if showAll {
		q.Set("show", "all")
	}
	if feedID != 0 {
		q.Set("feed", strconv.FormatInt(feedID, 10))
	}
	if before != "" {
		q.Set("before", before)
	}
	if len(q) == 0 {
		return "/latest"
	}
	return "/latest?" + q.Encode()
}

func parseCursor(s string) *cursor {
	published, id, ok := strings.Cut(s, "_")
	if !ok {
		return nil
	}
	p, err1 := strconv.ParseInt(published, 10, 64)
	i, err2 := strconv.ParseInt(id, 10, 64)
	if err1 != nil || err2 != nil {
		return nil
	}
	return &cursor{Published: p, ID: i}
}

func (a *App) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	showAll := q.Get("show") == "all"
	feedID, _ := strconv.ParseInt(q.Get("feed"), 10, 64)
	cur := parseCursor(q.Get("before"))

	items, err := listItems(a.db, itemFilter{UnreadOnly: !showAll, FeedID: feedID, Before: cur, Limit: pageSize + 1})
	if err != nil {
		serverError(w, err)
		return
	}
	olderURL := ""
	if len(items) > pageSize {
		items = items[:pageSize]
		last := items[pageSize-1]
		olderURL = listURL(showAll, feedID, fmt.Sprintf("%d_%d", last.Published.Unix(), last.ID))
	}
	upTo, err := maxItemID(a.db)
	if err != nil {
		serverError(w, err)
		return
	}
	updated, err := lastUpdated(a.db)
	if err != nil {
		serverError(w, err)
		return
	}
	feedName := ""
	if feedID != 0 {
		if err := a.db.QueryRow(`SELECT COALESCE(NULLIF(title, ''), url) FROM feeds WHERE id = ?`, feedID).Scan(&feedName); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.NotFound(w, r)
			} else {
				serverError(w, err)
			}
			return
		}
	}
	a.render(w, "list", map[string]any{
		"Title":      "Latest",
		"Nav":        "list",
		"Here":       r.URL.RequestURI(),
		"Items":      items,
		"ShowAll":    showAll,
		"FeedID":     feedID,
		"FeedName":   feedName,
		"UnreadURL":  listURL(false, feedID, ""),
		"AllURL":     listURL(true, feedID, ""),
		"OlderURL":   olderURL,
		"Paged":      cur != nil,
		"UpTo":       upTo,
		"Updated":    updated,
		"Refreshing": a.refreshing.Load(),
	})
}

func (a *App) handleArticle(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	it, err := getItem(a.db, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if err := setRead(a.db, id, true); err != nil {
		serverError(w, err)
		return
	}
	a.navigate(id)
	if it.Content == "" { // only a teaser in the feed
		http.Redirect(w, r, it.Link, http.StatusFound)
		return
	}
	a.render(w, "article", map[string]any{"Title": it.Title, "Nav": "article", "Item": it})
}

func (a *App) handleMarkUnread(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := setRead(a.db, id, false); err != nil {
		serverError(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) handleRate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	rating, err := strconv.Atoi(r.FormValue("rating"))
	if !ok || err != nil || (rating != 1 && rating != -1) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	switch err := toggleRating(a.db, id, rating, time.Now()); {
	case errors.Is(err, sql.ErrNoRows):
		http.NotFound(w, r)
		return
	case err != nil:
		serverError(w, err)
		return
	}
	if r.Header.Get("X-Vote") != "" { // saved in place by app.js
		w.WriteHeader(http.StatusNoContent)
		return
	}
	next := r.FormValue("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (a *App) handleReadAll(w http.ResponseWriter, r *http.Request) {
	upTo, err := strconv.ParseInt(r.FormValue("upto"), 10, 64)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	feedID, _ := strconv.ParseInt(r.FormValue("feed"), 10, 64)
	if err := markAllRead(a.db, feedID, upTo); err != nil {
		serverError(w, err)
		return
	}
	http.Redirect(w, r, listURL(false, feedID, ""), http.StatusSeeOther)
}

func (a *App) handleRefresh(w http.ResponseWriter, r *http.Request) {
	go a.refreshAll(a.ctx)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) renderFeeds(w http.ResponseWriter, formError, formValue string) {
	feeds, err := listFeeds(a.db)
	if err != nil {
		serverError(w, err)
		return
	}
	a.render(w, "feeds", map[string]any{
		"Title": "Feeds", "Nav": "feeds", "Feeds": feeds, "FormError": formError, "FormValue": formValue,
	})
}

func (a *App) handleFeeds(w http.ResponseWriter, r *http.Request) {
	a.renderFeeds(w, "", "")
}

func (a *App) handleAddFeed(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.FormValue("url"))
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		a.renderFeeds(w, "Enter a full feed URL starting with http:// or https://", raw)
		return
	}
	id, err := addFeed(a.db, u.String())
	if err != nil {
		serverError(w, err)
		return
	}
	go a.refreshFeed(a.ctx, Feed{ID: id, URL: u.String()})
	http.Redirect(w, r, "/feeds", http.StatusSeeOther)
}

func (a *App) handleDeleteFeed(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := deleteFeed(a.db, id); err != nil {
		serverError(w, err)
		return
	}
	http.Redirect(w, r, "/feeds", http.StatusSeeOther)
}

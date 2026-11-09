package main

import (
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"time"
)

const (
	secondsPerDay = 86400
	chartDays     = 30
	chartHeight   = 100
	chartColumn   = 20 // 16px bar plus 4px gap, in viewBox units
	statsPreview  = 10 // rows shown before "show more"
)

// periodStats is one column of the summary table. Since 0 means all time.
type periodStats struct {
	Label    string
	Articles int
	Seconds  int
	Average  int // per article with a measured time
	Likes    int
	Dislikes int
}

type sourceStats struct {
	Name     string
	Articles int
	Seconds  int
	Likes    int
	Dislikes int
	Share    int // percent of the top source's time
}

type articleStats struct {
	Title   string
	Link    string
	Seconds int
	Reads   int
}

type dayBar struct {
	Column   int // x of the hover target
	Path     template.HTMLAttr
	Label    string
	Date     string
	Seconds  int
	Articles int
}

func loadPeriod(db *sql.DB, label string, since int64) (periodStats, error) {
	p := periodStats{Label: label}
	var timed int
	err := db.QueryRow(`
		SELECT COUNT(DISTINCT link), COALESCE(SUM(seconds), 0), COUNT(DISTINCT CASE WHEN seconds > 0 THEN link END)
		FROM reads WHERE read_at > ?`, since).Scan(&p.Articles, &p.Seconds, &timed)
	if err != nil {
		return p, err
	}
	if timed > 0 {
		p.Average = p.Seconds / timed
	}
	err = db.QueryRow(`SELECT COALESCE(SUM(rating = 1), 0), COALESCE(SUM(rating = -1), 0) FROM ratings WHERE rated_at > ?`,
		since).Scan(&p.Likes, &p.Dislikes)
	return p, err
}

// streaks takes ascending day numbers. The current streak survives until a
// full day is missed, so it doesn't read 0 each morning.
func streaks(days []int64, today int64) (current, best int) {
	run := 0
	for i, d := range days {
		if i > 0 && d == days[i-1]+1 {
			run++
		} else {
			run = 1
		}
		best = max(best, run)
	}
	if n := len(days); n > 0 && days[n-1] >= today-1 {
		current = run
	}
	return current, best
}

// barPath draws a bar with rounded top corners, square on the baseline.
func barPath(x, top, width, base int) string {
	r := min(4, base-top, width/2)
	return fmt.Sprintf("M%d %dV%dQ%d %d %d %dH%dQ%d %d %d %dV%dZ",
		x, base, top+r, x, top, x+r, top, x+width-r, x+width, top, x+width, top+r, base)
}

// dailyBars lays out the last chartDays days, oldest first.
func dailyBars(seconds, articles map[int64]int, today int64) []dayBar {
	peak := 1
	for d := today - chartDays + 1; d <= today; d++ {
		peak = max(peak, seconds[d])
	}
	bars := make([]dayBar, 0, chartDays)
	for i := range chartDays {
		day := today - chartDays + 1 + int64(i)
		bar := dayBar{
			Column:   i * chartColumn,
			Date:     time.Unix(day*secondsPerDay, 0).UTC().Format("Jan 2"),
			Seconds:  seconds[day],
			Articles: articles[day],
		}
		bar.Label = fmt.Sprintf("%s: %s, %d articles", bar.Date, formatDuration(bar.Seconds), bar.Articles)
		if bar.Seconds > 0 {
			height := max(2, bar.Seconds*chartHeight/peak)
			bar.Path = template.HTMLAttr(fmt.Sprintf(`d="%s"`, barPath(bar.Column+2, chartHeight-height, 16, chartHeight)))
		}
		bars = append(bars, bar)
	}
	return bars
}

func loadDaily(db *sql.DB) (seconds, articles map[int64]int, days []int64, err error) {
	rows, err := db.Query(`
		SELECT read_at / ?, SUM(seconds), COUNT(DISTINCT link) FROM reads
		GROUP BY read_at / ? ORDER BY 1`, secondsPerDay, secondsPerDay)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	seconds, articles = map[int64]int{}, map[int64]int{}
	for rows.Next() {
		var day int64
		var s, a int
		if err := rows.Scan(&day, &s, &a); err != nil {
			return nil, nil, nil, err
		}
		seconds[day], articles[day] = s, a
		days = append(days, day)
	}
	return seconds, articles, days, rows.Err()
}

func loadSources(db *sql.DB) ([]sourceStats, error) {
	rows, err := db.Query(`
		WITH r AS (SELECT feed_url, COUNT(DISTINCT link) articles, SUM(seconds) seconds FROM reads GROUP BY feed_url),
		     v AS (SELECT feed_url, SUM(rating = 1) likes, SUM(rating = -1) dislikes FROM ratings GROUP BY feed_url),
		     u AS (SELECT feed_url FROM r UNION SELECT feed_url FROM v)
		SELECT COALESCE(NULLIF(f.title, ''), u.feed_url), COALESCE(r.articles, 0), COALESCE(r.seconds, 0),
		       COALESCE(v.likes, 0), COALESCE(v.dislikes, 0)
		FROM u LEFT JOIN r ON r.feed_url = u.feed_url LEFT JOIN v ON v.feed_url = u.feed_url
		LEFT JOIN feeds f ON f.url = u.feed_url
		ORDER BY COALESCE(r.seconds, 0) DESC, COALESCE(v.likes, 0) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sourceStats
	for rows.Next() {
		var s sourceStats
		if err := rows.Scan(&s.Name, &s.Articles, &s.Seconds, &s.Likes, &s.Dislikes); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func loadArticles(db *sql.DB, query string) ([]articleStats, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []articleStats
	for rows.Next() {
		var a articleStats
		if err := rows.Scan(&a.Title, &a.Link, &a.Seconds, &a.Reads); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func ratedArticlesQuery(rating int) string {
	return fmt.Sprintf(`SELECT title, link, 0, 0 FROM ratings WHERE rating = %d ORDER BY rated_at DESC LIMIT 100`, rating)
}

func (a *App) handleStats(w http.ResponseWriter, r *http.Request) {
	data, err := a.buildStats(time.Now())
	if err != nil {
		serverError(w, err)
		return
	}
	a.render(w, "stats", data)
}

func (a *App) buildStats(now time.Time) (map[string]any, error) {
	windows := []struct {
		label string
		since int64
	}{
		{"Today", now.Add(-24 * time.Hour).Unix()},
		{"Week", now.Add(-7 * 24 * time.Hour).Unix()},
		{"Month", now.Add(-30 * 24 * time.Hour).Unix()},
		{"Year", now.Add(-365 * 24 * time.Hour).Unix()},
		{"All time", 0},
	}
	var periods []periodStats
	for _, window := range windows {
		p, err := loadPeriod(a.db, window.label, window.since)
		if err != nil {
			return nil, err
		}
		periods = append(periods, p)
	}

	seconds, articles, days, err := loadDaily(a.db)
	if err != nil {
		return nil, err
	}
	today := now.Unix() / secondsPerDay
	current, best := streaks(days, today)

	sources, err := loadSources(a.db)
	if err != nil {
		return nil, err
	}
	if len(sources) > 0 && sources[0].Seconds > 0 {
		for i := range sources {
			if sources[i].Seconds > 0 {
				sources[i].Share = max(2, sources[i].Seconds*100/sources[0].Seconds)
			}
		}
	}
	top, err := loadArticles(a.db, `SELECT MAX(title), link, SUM(seconds), COUNT(*) FROM reads
		GROUP BY link HAVING SUM(seconds) > 0 ORDER BY SUM(seconds) DESC LIMIT 25`)
	if err != nil {
		return nil, err
	}
	liked, err := loadArticles(a.db, ratedArticlesQuery(1))
	if err != nil {
		return nil, err
	}
	disliked, err := loadArticles(a.db, ratedArticlesQuery(-1))
	if err != nil {
		return nil, err
	}

	var longest, feeds, stored, unread int
	if err := a.db.QueryRow(`SELECT COALESCE(MAX(seconds), 0) FROM reads`).Scan(&longest); err != nil {
		return nil, err
	}
	if err := a.db.QueryRow(`SELECT (SELECT COUNT(*) FROM feeds), COUNT(*), COALESCE(SUM(read = 0), 0) FROM items`).
		Scan(&feeds, &stored, &unread); err != nil {
		return nil, err
	}

	allTime := periods[len(periods)-1]
	perDay := 0
	if len(days) > 0 {
		perDay = allTime.Seconds / len(days)
	}
	bars := dailyBars(seconds, articles, today)
	peak := 0
	for _, bar := range bars {
		peak = max(peak, bar.Seconds)
	}
	return map[string]any{
		"Title": "Stats", "Nav": "stats", "Periods": periods, "AllTime": allTime, "Week": periods[1],
		"CurrentStreak": current, "BestStreak": best, "ActiveDays": len(days), "PerDay": perDay, "Longest": longest,
		"Bars": bars, "Peak": peak, "FirstDay": bars[0].Date, "LastDay": bars[len(bars)-1].Date,
		"Sources": sources[:min(statsPreview, len(sources))], "SourcesRest": sources[min(statsPreview, len(sources)):], "SourceCount": len(sources),
		"Top": top[:min(statsPreview, len(top))], "TopRest": top[min(statsPreview, len(top)):], "Liked": liked, "Disliked": disliked,
		"Feeds": feeds, "Stored": stored, "Unread": unread,
	}, nil
}

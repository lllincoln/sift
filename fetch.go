package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	fetchWorkers = 4
	maxFeedBytes = 10 << 20
)

func parseURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		return nil
	}
	return u
}

func (a *App) cutoff(now time.Time) time.Time { return now.Add(-a.retention) }

// run refreshes on startup and then on every interval until ctx is cancelled.
func (a *App) run(ctx context.Context) {
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	a.refreshAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.refreshAll(ctx)
		}
	}
}

// refreshAll is a no-op if a refresh is already running.
func (a *App) refreshAll(ctx context.Context) {
	if !a.refreshing.CompareAndSwap(false, true) {
		return
	}
	defer a.refreshing.Store(false)

	feeds, err := listFeeds(a.db)
	if err != nil {
		log.Printf("list feeds: %v", err)
		return
	}
	jobs := make(chan Feed)
	var wg sync.WaitGroup
	for range fetchWorkers {
		wg.Go(func() {
			for f := range jobs {
				a.refreshFeed(ctx, f)
			}
		})
	}
sending:
	for _, f := range feeds {
		select {
		case jobs <- f:
		case <-ctx.Done():
			break sending
		}
	}
	close(jobs)
	wg.Wait()

	if ctx.Err() == nil {
		if err := prune(a.db, a.cutoff(time.Now())); err != nil {
			log.Printf("prune: %v", err)
		}
	}
}

func (a *App) refreshFeed(ctx context.Context, f Feed) {
	err := a.fetchFeed(ctx, f)
	if err == nil || ctx.Err() != nil {
		return
	}
	log.Printf("fetch %s: %v", f.URL, err)
	if err := recordFetchError(a.db, f.ID, time.Now(), err.Error()); err != nil {
		log.Printf("record error: %v", err)
	}
}

func (a *App) fetchFeed(ctx context.Context, f Feed) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; sift/1.0)")
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml;q=0.9, text/xml;q=0.9, */*;q=0.5")
	if f.ETag != "" {
		req.Header.Set("If-None-Match", f.ETag)
	}
	if f.LastModified != "" {
		req.Header.Set("If-Modified-Since", f.LastModified)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	now := time.Now()
	if resp.StatusCode == http.StatusNotModified {
		return recordNotModified(a.db, f.ID, now)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxFeedBytes {
		return errors.New("feed larger than 10 MB")
	}
	parsed, err := parseFeed(body)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	return saveFeed(a.db, f, parsed, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), now, a.cutoff(now))
}

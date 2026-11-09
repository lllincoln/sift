package main

import (
	"database/sql"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	digestPageSize     = 30
	digestWindow       = 72 * time.Hour
	paperWindow        = 14 * 24 * time.Hour // good papers surface slowly and stay relevant
	storyHalfLife      = 48.0                // hours
	paperHalfLife      = 7 * 24.0
	digestMaxPerSource = 3
	digestMaxPerTopic  = 10
	paywallPenalty     = 0.35
	independentBoost   = 2.0
	// Coverage counts favour newsrooms, so they also share a ceiling.
	digestMaxInstitutional = 8
)

// paywalled lists publishers, as named by publisherOf, whose articles mostly
// need a subscription. Feeds don't say, so this is maintained by hand.
var paywalled = setOf("barrons.com", "bloomberg.com", "dowjones.io", "economist.com", "ft.com",
	"lennysnewsletter.com", "newyorker.com", "nytimes.com", "pragmaticengineer.com", "stratechery.com",
	"theatlantic.com", "theinformation.com", "washingtonpost.com", "wsj.com")

// institutional lists newsrooms, companies and magazines. Every other feed is
// treated as an independent writer, so newly added blogs count automatically.
var institutional = setOf("a16z.news", "arstechnica.com", "bbci.co.uk", "cnbc.com", "dowjones.io",
	"firstround.com", "hnrss.org", "npr.org", "nytimes.com", "ourworldindata.org", "quantamagazine.org",
	"worksinprogress.co", "wsj.com")

// DigestItem is a chosen story and the number of publishers that ran it.
type DigestItem struct {
	Item
	Outlets int
}

type candidate struct {
	Item
	feedURL    string
	publisher  string
	publishers map[string]bool // every publisher carrying this link
	textLen    int
	perWeek    float64 // how often the feed posts
	tokens     map[string]bool
	topic      string
	outlets    int
	score      float64
}

var stopwords = setOf(strings.Fields(`a an and are as at be but by can could did do does for from had has have he
	her his how i if in into is it its just may more most new no not of on one or our out over say says she should
	so than that the their them they this to up us was we were what when where which who why will with would you
	your after about amid before been get gets got make makes off some still there these those while`)...)

func stem(w string) string {
	if len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
		return w[:len(w)-1]
	}
	return w
}

func tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(strings.ReplaceAll(s, "’", "'")), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	})
	var out []string
	for _, f := range fields {
		f = strings.TrimSuffix(strings.Trim(f, "'"), "'s")
		if len(f) >= 2 && !stopwords[f] {
			out = append(out, stem(f))
		}
	}
	return out
}

// topicWords approximates subject matter. Feeds alone can't: an "economy"
// story and an AI essay can come from the same feed, and four different
// newsletters can all be about AI.
var topicWords = func() map[string]string {
	lists := map[string]string{
		"ai":       "ai llm llms openai anthropic gpt claude gemini chatbot agi deepmind",
		"politics": "trump biden congress senate senator election democrat republican president supreme lawmaker shutdown immigration governor voter ballot",
		"world":    "war ukraine russia china israel gaza iran nato military missile diplomat ceasefire minister troops",
		"money":    "stock market fed inflation economy economic tariff earnings investor bank gdp oil recession treasury wall shares dow venture",
		"science":  "science scientist study researcher nasa space climate physics biology cancer vaccine health species quantum medicine",
		"tech":     "software apple google microsoft amazon meta nvidia chip security hack hacker startup programming developer browser database linux",
	}
	words := map[string]string{}
	for topic, list := range lists {
		for _, w := range strings.Fields(list) {
			words[stem(w)] = topic
		}
	}
	return words
}()

func detectTopic(titleTokens, snippetTokens []string) string {
	hits := map[string]int{}
	for _, t := range titleTokens {
		if topic := topicWords[t]; topic != "" {
			hits[topic] += 3
		}
	}
	for _, t := range snippetTokens {
		if topic := topicWords[t]; topic != "" {
			hits[topic]++
		}
	}
	best, bestHits := "", 1 // one snippet mention isn't enough
	for _, topic := range []string{"ai", "politics", "world", "money", "science", "tech"} {
		if hits[topic] > bestHits {
			best, bestHits = topic, hits[topic]
		}
	}
	return best
}

// publisherOf groups a site's many section feeds (rss.nytimes.com,
// search.cnbc.com) under one name. Subdomains of blog hosts are separate writers.
func publisherOf(feedURL string) string {
	u := parseURL(feedURL)
	if u == nil {
		return feedURL
	}
	host := strings.ToLower(u.Hostname())
	for _, shared := range []string{".substack.com", ".github.io", ".wordpress.com", ".bearblog.dev", ".mataroa.blog"} {
		if strings.HasSuffix(host, shared) {
			return host
		}
	}
	labels := strings.Split(host, ".")
	keep := 2
	if n := len(labels); n >= 3 && len(labels[n-1]) == 2 && len(labels[n-2]) <= 3 {
		keep = 3 // example.co.uk
	}
	if len(labels) <= keep {
		return host
	}
	return strings.Join(labels[len(labels)-keep:], ".")
}

func tokenSimilarity(a, b map[string]bool) (shared int, jaccard float64) {
	for t := range a {
		if b[t] {
			shared++
		}
	}
	union := len(a) + len(b) - shared
	if union == 0 {
		return 0, 0
	}
	return shared, float64(shared) / float64(union)
}

func sameStory(a, b map[string]bool) bool {
	shared, jaccard := tokenSimilarity(a, b)
	return shared >= 3 && jaccard >= 0.3
}

var lowValueTitle = regexp.MustCompile(`(?i)\b(quiz|crossword|wordle|horoscope|open thread|premium|squid blogging|assorted links|links for|reading list|what i.ve been reading|live updates?|how to watch|sponsored|podcast|best deals?|deals? of)\b`)

// baseScore rates a story before outlet coverage is known.
func (c *candidate) baseScore(now time.Time, prefs *preferences, titleTokens []string) float64 {
	age := now.Sub(c.Published).Hours()
	halfLife := storyHalfLife
	if isPaper(c.Link) {
		halfLife = paperHalfLife
	}
	recency := math.Pow(0.5, max(age, 0)/halfLife)
	// A writer who posts monthly has picked this one; a wire doesn't choose.
	rarity := 1 / math.Sqrt(1+c.perWeek/3)
	depth := 0.6 * math.Min(1, float64(c.textLen)/12000)
	score := recency * (1 + rarity + depth)
	if lowValueTitle.MatchString(c.Title) {
		score *= 0.25
	}
	// The link is checked too: aggregators like Hacker News point at paywalls.
	if paywalled[c.publisher] || paywalled[publisherOf(c.Link)] {
		score *= paywallPenalty
	}
	if !institutional[c.publisher] {
		score *= independentBoost
	}
	return score * prefs.multiplier(c.feedURL, c.topic, titleTokens)
}

// selectDigest merges coverage of the same event, ranks each story, then
// picks greedily so that every pick discounts its publisher and topic. The
// limits apply until nothing eligible remains; the rest follow in score order.
func selectDigest(cands []*candidate, now time.Time, size int, prefs *preferences) []DigestItem {
	for _, c := range cands {
		titleTokens := tokenize(c.Title)
		c.tokens = setOf(titleTokens...)
		c.topic = detectTopic(titleTokens, tokenize(c.Snippet))
		c.score = c.baseScore(now, prefs, titleTokens)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].ID > cands[j].ID
	})

	// The strongest version of each story comes first, so it leads the cluster.
	var leaders []*candidate
	for _, c := range cands {
		var match *candidate
		for _, l := range leaders {
			if sameStory(l.tokens, c.tokens) {
				match = l
				break
			}
		}
		if match == nil {
			leaders = append(leaders, c)
			continue
		}
		for p := range c.publishers {
			match.publishers[p] = true
		}
	}
	for _, l := range leaders {
		l.outlets = len(l.publishers)
		l.score *= 1 + 0.9*math.Log2(float64(max(l.outlets, 1)))
	}

	picked := []DigestItem{}
	bySource, byTopic := map[string]int{}, map[string]int{}
	used := make(map[*candidate]bool)
	relaxed := false
	newsrooms := 0
	for len(picked) < size {
		var best *candidate
		bestScore := 0.0
		for _, l := range leaders {
			if used[l] {
				continue
			}
			if !relaxed && (bySource[l.publisher] >= digestMaxPerSource ||
				(institutional[l.publisher] && newsrooms >= digestMaxInstitutional) ||
				(l.topic != "" && byTopic[l.topic] >= digestMaxPerTopic)) {
				continue
			}
			adjusted := l.score * math.Pow(0.55, float64(bySource[l.publisher]))
			if l.topic != "" {
				adjusted *= math.Pow(0.7, float64(byTopic[l.topic]))
			}
			if adjusted > bestScore {
				best, bestScore = l, adjusted
			}
		}
		if best == nil {
			if relaxed {
				break
			}
			relaxed = true
			continue
		}
		used[best] = true
		bySource[best.publisher]++
		if institutional[best.publisher] {
			newsrooms++
		}
		if best.topic != "" {
			byTopic[best.topic]++
		}
		picked = append(picked, DigestItem{Item: best.Item, Outlets: best.outlets})
	}
	return picked
}

func isPaper(link string) bool { return strings.HasPrefix(link, arxivAbsPrefix) }

// tally counts votes on one feed, topic or title word.
type tally struct{ up, down int }

// lean runs from -1 to 1. The added 2 stops a single vote from swinging it far.
func (t tally) lean() float64 { return float64(t.up-t.down) / float64(t.up+t.down+2) }

// preferences is what past likes and dislikes say about the reader's taste.
type preferences struct {
	feeds, topics, words map[string]tally
}

func newPreferences() *preferences {
	return &preferences{feeds: map[string]tally{}, topics: map[string]tally{}, words: map[string]tally{}}
}

func (p *preferences) record(feedURL, title string, rating int) {
	add := func(m map[string]tally, key string) {
		t := m[key]
		if rating > 0 {
			t.up++
		} else {
			t.down++
		}
		m[key] = t
	}
	tokens := tokenize(title)
	add(p.feeds, feedURL)
	if topic := detectTopic(tokens, nil); topic != "" {
		add(p.topics, topic)
	}
	for word := range setOf(tokens...) {
		add(p.words, word)
	}
}

// multiplier scales a story's score from 1/4 to 4 times. Diversity limits are
// applied afterwards, so liking one subject can't take over the digest.
func (p *preferences) multiplier(feedURL, topic string, titleTokens []string) float64 {
	if p == nil {
		return 1
	}
	exponent := p.feeds[feedURL].lean() + 0.7*p.topics[topic].lean()
	sum, count := 0.0, 0
	for _, w := range titleTokens {
		if t := p.words[w]; t.up+t.down >= 2 { // one-off words are noise
			sum += t.lean()
			count++
		}
	}
	if count > 0 {
		exponent += sum / float64(count)
	}
	return math.Pow(2, math.Max(-2, math.Min(2, exponent)))
}

func loadPreferences(db *sql.DB) (*preferences, error) {
	rows, err := db.Query(`SELECT feed_url, title, rating FROM ratings ORDER BY rated_at DESC LIMIT 2000`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	p := newPreferences()
	for rows.Next() {
		var feedURL, title string
		var rating int
		if err := rows.Scan(&feedURL, &title, &rating); err != nil {
			return nil, err
		}
		p.record(feedURL, title, rating)
	}
	return p, rows.Err()
}

func loadCandidates(db *sql.DB, now time.Time) ([]*candidate, error) {
	const rateDays = 30
	rates := map[int64]float64{}
	rateRows, err := db.Query(`SELECT feed_id, COUNT(*) FROM items WHERE published > ? GROUP BY feed_id`,
		now.Add(-rateDays*24*time.Hour).Unix())
	if err != nil {
		return nil, err
	}
	defer rateRows.Close()
	for rateRows.Next() {
		var id int64
		var count int
		if err := rateRows.Scan(&id, &count); err != nil {
			return nil, err
		}
		rates[id] = float64(count) * 7 / rateDays
	}
	if err := rateRows.Err(); err != nil {
		return nil, err
	}
	rateRows.Close() // the single connection is needed for the next query

	rows, err := db.Query(`
		SELECT i.id, i.feed_id, COALESCE(NULLIF(f.title, ''), f.url), f.url, i.link, i.title, i.snippet,
		       LENGTH(i.content), i.published, i.read, COALESCE(r.rating, 0),
		       (SELECT GROUP_CONCAT(s2.url, char(10)) FROM item_sources s
		        JOIN feeds s2 ON s2.id = s.feed_id WHERE s.item_id = i.id)
		FROM items i JOIN feeds f ON f.id = i.feed_id LEFT JOIN ratings r ON r.link = i.link
		WHERE COALESCE(r.rating, 0) != -1
		  AND (i.published > ? OR (i.published > ? AND i.link LIKE ?))`,
		now.Add(-digestWindow).Unix(), now.Add(-paperWindow).Unix(), arxivAbsPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cands []*candidate
	for rows.Next() {
		var c candidate
		var feedURL string
		var published int64
		var sources sql.NullString
		if err := rows.Scan(&c.ID, &c.FeedID, &c.FeedName, &feedURL, &c.Link, &c.Title, &c.Snippet,
			&c.textLen, &published, &c.Read, &c.Rating, &sources); err != nil {
			return nil, err
		}
		c.Published = time.Unix(published, 0)
		c.publisher = publisherOf(feedURL)
		c.feedURL = feedURL
		c.perWeek = rates[c.FeedID]
		c.publishers = map[string]bool{c.publisher: true}
		for _, u := range strings.Split(sources.String, "\n") {
			if u != "" {
				c.publishers[publisherOf(u)] = true
			}
		}
		if isPaper(c.Link) {
			c.publisher = "arxiv.org" // papers compete as a source of their own
		}
		cands = append(cands, &c)
	}
	return cands, rows.Err()
}

func digestPageURL(page int) string {
	if page <= 1 {
		return "/"
	}
	return "/?page=" + strconv.Itoa(page)
}

func (a *App) handleDigest(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	prefs, err := loadPreferences(a.db)
	if err != nil {
		serverError(w, err)
		return
	}
	cands, err := loadCandidates(a.db, now)
	if err != nil {
		serverError(w, err)
		return
	}
	stories := selectDigest(cands, now, len(cands), prefs)
	pages := max(1, (len(stories)+digestPageSize-1)/digestPageSize)
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(1, min(page, pages))
	start := (page - 1) * digestPageSize
	end := min(start+digestPageSize, len(stories))

	a.render(w, "digest", map[string]any{
		"Title":      "Digest",
		"Nav":        "digest",
		"Items":      stories[start:end],
		"Start":      start + 1,
		"Page":       page,
		"Pages":      pages,
		"Here":       digestPageURL(page),
		"PrevURL":    digestPageURL(page - 1),
		"NextURL":    digestPageURL(page + 1),
		"Considered": len(cands),
		"Refreshing": a.refreshing.Load(),
	})
}

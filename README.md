# Sift

A small single-user RSS and Atom reader. It's one static Go binary with SQLite built in, shipped as a roughly 14 MB `scratch` image for amd64 and every Raspberry Pi architecture (arm64, armv7, armv6).

There is no login. Run it only on a network you trust.

## Run

Copy `docker-compose.yml` to the machine and run:

```sh
docker compose up -d
```

Open `http://<host>:8080`. The feeds in `defaultlist.txt` are added on first start; after that, manage them on the Feeds page.

The home page ranks every story from the last 3 days; the full timeline is under Latest. It favours stories covered by several outlets, posts from infrequent writers and full-length articles, ranks independent writers above newsrooms and paywalled outlets below both (the `institutional` and `paywalled` lists in `digest.go`), then limits how many come from one publisher or topic. It runs locally with no external service.

Like and Dislike teach the digest: votes shift the ranking of similar sources, topics and title words, and a disliked story leaves the digest. Time spent reading is tracked separately, on the Stats page with your other totals, and does not affect ranking.

Papers come from one feed: arXiv links that reached 30 points on Hacker News. Those stay eligible for 14 days.

## Configuration

| Variable          | Default       | Purpose                                                |
| ----------------- | ------------- | ------------------------------------------------------ |
| `ADDR`            | `:8080`       | Listen address                                         |
| `DB_PATH`         | `/data/rss.db` in the image, `rss.db` otherwise | Database location |
| `REFRESH_MINUTES` | `30`          | Time between feed refreshes                            |
| `READ_TIMEOUT_MINUTES` | `20` | Longest gap counted as reading; longer means the app was closed |
| `RETENTION_DAYS`  | `30`          | Age at which stories are deleted, read or not          |

If you bind-mount a directory instead of using the named volume, make it writable by UID 65532.

## Publishing

Pushes to `main` and `v*` tags run the tests, then publish a multi-arch image to `ghcr.io/<owner>/<repo>` with GitHub Actions. Pull requests run the tests and build without publishing.

New GHCR packages are private. Either make the package public in its GitHub settings, or run `docker login ghcr.io` on the host.

## Develop

```sh
go test ./...
go run .   # http://localhost:8080, using ./rss.db
```

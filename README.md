# daans.cat
_Disclaimer: Claude and Codex where used while making this website_

Pictures of my cat. One photo per page, an RSS feed, and a password-only admin
panel to post from my phone.

A single Go binary (~12 MB image, no database, no JavaScript). Photos and their
captions live in a plain folder, so a backup is `cp -r data`.

```
daans.cat                           archive  rss

[              enormous cat photo              ]

July 13, 2026

sat on the clean laundry again

← older                                  newer →
```

---

## Quick start (local)

```sh
go run . hash                 # type a password, get a bcrypt hash
export ADMIN_PASSWORD_HASH='$2a$12$...'
go run .                      # http://localhost:8080, admin at /admin
```

## Docker

```sh
cp .env.example .env
docker compose run --rm --entrypoint /daans-cat daans-cat hash
mkdir -p data && sudo chown -R 65532:65532 data
docker compose up -d
```

It listens on `127.0.0.1:8080`. Put Caddy in front (see `deploy/Caddyfile`) and you have HTTPS on daans.cat with no further work.

## Configuration

Everything can be set through the environment variables.

| Variable | Default | Notes |
| --- | --- | --- |
| `ADMIN_PASSWORD_HASH` | — | **Required.** bcrypt hash from `daans-cat hash`. |
| `SITE_URL` | `http://localhost:8080` | Canonical origin, no trailing slash. Used for RSS/sitemap links and to decide on secure cookies. |
| `SITE_TITLE` | `daans.cat` | Site name, used in page titles, RSS, and Open Graph. |
| `SITE_DESC` | `pictures of daan's cat` | Used for the `<meta description>`, Open Graph, and the RSS channel description. |
| `SECURITY_TXT_URL` | `https://dnbr.cloud/.well-known/security.txt` | Where `/security.txt` and `/.well-known/security.txt` redirect. Set empty to disable both routes. |
| `ADDR` | `:8080` | Listen address. |
| `DATA_DIR` | `./data` | Where `posts.json`, `media/`, and `originals/` live. |
| `BEHIND_PROXY` | `false` | Set `true` behind a reverse proxy (Caddy/Traefik/nginx) so the rate limiter and access logs read the real client IP from `X-Forwarded-For`. |
| `SECURE_COOKIES` | `auto` | `auto` = secure whenever `SITE_URL` is https; override with `true`/`false`. |

## Tests
The AI added some tests without me asking for them. It tried it best so I left them in here for your enjoyment.

```sh
go test ./...
```

Covers slug generation, the EXIF orientation parser (including truncated and
malicious input), upload rejection, EXIF stripping, path traversal, rate
limiting, and session handling.

# Paper Plane

Paper Plane is a self-hosted, single-binary Go application for hosting many
static sites from one instance. Each "project" is a static site served under a
path prefix (`${APP_URL}/<slug>`), and all projects are managed through a small,
password-protected admin dashboard.

It is built for simplicity: one static binary, one SQLite file, one data
directory. No CGO, no Node toolchain, no external services.

## Features

- **Project CRUD** — create, edit (rename), unlink/relink, and delete static
  sites from the dashboard.
- **File management** — upload single files, multiple files, or a `.zip`
  (extracted on the server, preserving relative paths); edit text files
  (HTML/CSS/JS/TXT/MD) in a simple in-browser editor; delete files.
- **Empty-project bootstrap** — creating a project with no upload auto-generates
  a placeholder `index.html`.
- **Path-based static serving** — `GET /<slug>/...` serves the project's files
  with a default `index.html`, correct `Content-Type`, and cache headers.
- **`<base>` injection** — the served root `index.html` gets
  `<base href="/<slug>/">` injected (best-effort) so absolute-rooted asset paths
  resolve under the slug prefix.
- **Unlink without delete** — `unlinked` projects return 404 to visitors while
  their files remain on disk; relink to restore.
- **Auth + CSRF** — global admin password (bcrypt), signed stateless session
  cookie, CSRF token on every mutation.
- **SQLite storage** — metadata in SQLite (WAL mode, pure-Go driver); site files
  on the filesystem.
- **Single binary** — templates and static assets are `go:embed`-ed into the
  binary; ships as a small distroless image.

## Quick start (local)

Prerequisites: **Go 1.26+**.

```sh
git clone https://github.com/kalfian/paper-plane.git
cd paper-plane

# Optional config; all have sensible defaults. The .env is loaded automatically
# at startup (no need to export by hand). You can skip this entirely to run with
# defaults.
cp .env.example .env

make run
```

Then open <http://localhost:8080/_app/setup> and **choose your admin password**
(first run only). After that you land in the dashboard; subsequent visits sign
in at <http://localhost:8080/_app/login>. The bare root (`/`) redirects to the
admin app.

> **How the admin password works:** there is no `ADMIN_PASSWORD` environment
> variable. On a fresh instance the first-run **setup** page
> (`/_app/setup`) lets you set the password once; it is hashed with bcrypt and
> stored in the SQLite `settings` table (never in plaintext, never in the env).
> Once set, the setup page redirects to login and can't reset the credential. To
> change it later, use **Settings → Change password** in the admin UI. To reset a
> forgotten password, clear the stored hash (e.g. start from a fresh data
> directory).

## Configuration

All configuration is via environment variables (read once at startup). None are
required — the admin password is set interactively on first run, not via the
environment.

| Variable   | Required | Default | Description                                                                 |
| ---------- | -------- | ------- | --------------------------------------------------------------------------- |
| `APP_URL`  | no       | —       | Public base URL of the instance. Used for absolute links; trailing slash is trimmed. If it starts with `https://`, session cookies are marked `Secure`. |
| `DATA_DIR` | no       | `/data` | Directory for persistent data (SQLite DB + site files). Created if missing. |
| `PORT`     | no       | `8080`  | TCP port the HTTP server listens on.                                        |

Copy-paste starting point (same as [`.env.example`](.env.example)) — save as `.env`:

```sh
# Public base URL (optional; used for absolute links + Secure cookies on https).
APP_URL=http://localhost:8080

# Directory for persistent data — SQLite DB + uploaded site files (optional).
DATA_DIR=./data

# TCP port the HTTP server listens on (optional).
PORT=8080
```

For local `make run`, the `.env` is loaded automatically at startup. In Docker,
leave `DATA_DIR` at the image default (`/data`) so it matches the mounted volume.

## Deploy with Docker

Prebuilt images are published to `ghcr.io/kalfian/paper-plane`. CI
(`.github/workflows/release.yml`) runs `go vet` + tests, then builds and pushes
on every push to `main` (tag `latest`) and on every `v*` git tag (semver tags
`{{version}}` and `{{major}}.{{minor}}`, plus a `sha` tag). `APP_URL` is supplied
at runtime; the admin password is set on first run and never baked into the image.

Run the published image:

```sh
docker run -d -p 8080:8080 \
  -v paperplane-data:/data \
  -e APP_URL=https://example.com \
  ghcr.io/kalfian/paper-plane
```

Or build locally:

```sh
docker build -t paper-plane .
docker run -d -p 8080:8080 -v paperplane-data:/data paper-plane
```

After the container is up, open `/_app/setup` to choose the admin password.

### Docker Compose (recommended)

The repo ships a [`docker-compose.yml`](docker-compose.yml). It reads your `.env`
for the optional `APP_URL`/`PORT` and persists data in a named volume:

```sh
docker compose up -d
```

Then open <http://localhost:8080/_app/setup> to set your admin password (first
run), and <http://localhost:8080/_app/login> thereafter. Common operations:

```sh
docker compose logs -f    # follow logs
docker compose pull       # pull a newer published image
docker compose up -d      # apply the pulled image
docker compose down       # stop (data volume is kept)
```

To build from source instead of pulling `ghcr.io/kalfian/paper-plane`, uncomment
the `build: .` line (and comment out `image:`) in `docker-compose.yml`, then
`docker compose up -d --build`.

Notes:

- **Data persistence** — mount a volume at `/data` (the image declares
  `VOLUME ["/data"]`). It holds `paperplane.db` and `sites/<project-id>/`. The
  data directory is owned by the nonroot uid `65532` so the runtime user can
  write to it.
- **Health check** — the image ships a `HEALTHCHECK` that invokes the binary's
  own `healthcheck` subcommand (`/paperplane healthcheck`), which probes
  `http://127.0.0.1:${PORT}/_app/healthz` and exits 0/1. The distroless runtime
  has no shell or curl, so the subcommand is used instead.

## How it works / Architecture

**Routing.** A single `net/http.ServeMux` (Go 1.22 method+path patterns) serves
two namespaces. The admin app lives under `/_app/*` and always wins because its
patterns are more specific than the catch-all `GET /` fallback. Everything else
is treated as a site request: the first path segment is resolved as a project
slug, and if the project is `active`, its files are served. Unknown or
`unlinked` slugs return 404. `GET /<slug>` (no trailing slash) 301-redirects to
`/<slug>/` so relative assets and the injected `<base>` resolve correctly.

Key admin routes: `GET /_app/login`, `POST /_app/login`, `POST /_app/logout`,
`GET /_app/` (dashboard), `.../projects` CRUD, `.../projects/{id}/files*` for
file management, `GET /_app/healthz` (unauthenticated), and
`GET /_app/static/*` for embedded CSS/htmx.

**Storage.** Project metadata (id, name, slug, status, timestamps) and instance
settings (`admin_password_hash`, `cookie_secret`) live in SQLite at
`DATA_DIR/paperplane.db`. Site files live on the filesystem at
`DATA_DIR/sites/<project-id>/`. Migrations run automatically at startup.

**Asset-path caveat.** For a site's root `index.html`, Paper Plane injects
`<base href="/<slug>/">` right after `<head>` (skipped if the document already
has a `<base>` or no `<head>`). This makes root-relative asset URLs work under
the slug prefix, but it is best-effort — **relative asset paths are
recommended**, and SPA client-side routing under a path prefix is out of scope.

Directory layout:

```
paper-plane/
├── cmd/paperplane/main.go     # entrypoint + `healthcheck` subcommand
├── internal/
│   ├── config/                # env → Config
│   ├── store/                 # SQLite store + migrations
│   ├── sitefs/                # path-safe file store + zip extraction
│   ├── auth/                  # bcrypt, signed session cookie, CSRF
│   ├── model/                 # Project, Status
│   └── server/                # routing, middleware, admin + serve handlers
├── web/                       # go:embed templates + static assets
├── docs/                      # PRD.md, PLAN.md
├── Dockerfile
└── .github/workflows/release.yml
```

## Development

```sh
make run     # go run ./cmd/paperplane  (loads .env; set password at /_app/setup)
make test    # go test ./... -race -cover
make vet     # go vet ./...
make build   # CGO_ENABLED=0 build → ./bin/paperplane
make tidy    # go mod tidy
```

The build is **pure Go with no CGO** (SQLite via `modernc.org/sqlite`), so the
binary is statically linked and runs on a distroless/static base image.

Packages: `config` (env loading), `store` (SQLite + migrations), `sitefs`
(path-safe file operations + zip extraction), `auth` (password/session/CSRF),
`model` (domain types), and `server` (HTTP wiring), with `web` holding the
embedded templates and static assets.

## Security notes

- **Password** — chosen on first run (`/_app/setup`) and stored as a bcrypt
  hash; the plaintext is never persisted and never passed through the
  environment. Change it later via Settings → Change password.
- **Sessions** — stateless, HMAC-SHA256-signed cookies (`HttpOnly`,
  `SameSite=Lax`, 7-day TTL); marked `Secure` when `APP_URL` is HTTPS.
- **CSRF** — every mutating request (`POST`) requires a valid signed CSRF token.
- **Upload guards** — zip-slip and path-traversal are rejected; uploads are
  capped at **50 MiB per request** and **500 entries per zip archive**.
- **TLS** — Paper Plane does not terminate TLS. Put a reverse proxy (Caddy,
  Nginx, Traefik) in front for HTTPS, and set `APP_URL` to your `https://` URL
  so session cookies are marked `Secure`.

## Limitations / Roadmap

Current MVP (v1) limitations, per the [PRD](docs/PRD.md):

- **Single admin** — one global password; no multi-user or RBAC.
- **Immutable slug** — slugs cannot be renamed after creation (to avoid broken
  links).
- **Path-based only** — no subdomain or custom-domain routing per project.
- **No build pipeline** — files are served exactly as uploaded; no site build,
  versioning, or rollback.
- **No visitor auth** — served sites are public.
- Absolute/SPA asset paths are only partially handled by `<base>` injection.

Planned:

- **v1.1** — folder uploads (`webkitdirectory`), per-project visitor password
  protection, slug rename with redirects.
- **v2** — subdomain routing, custom domains, multi-user.

See [`docs/PRD.md`](docs/PRD.md) and [`docs/PLAN.md`](docs/PLAN.md) for full
requirements and architecture decisions.

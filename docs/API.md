# Paper Plane REST API

A JSON REST API for managing projects and their files programmatically — designed
so that scripts and AI agents can create and edit static sites without driving the
admin web UI.

- **Base path:** `/api/v1`
- **Content type:** `application/json` for every request body and response.
- **Auth:** bearer API key (see below). No cookies, no CSRF.
- **Spec:** a machine-readable OpenAPI document lives at
  [`docs/openapi.yaml`](./openapi.yaml).

The admin UI remains under `/_app/*` and is unaffected. Hosted static sites are
still served at `/<slug>/`.

---

## Authentication

Every endpoint except the discovery root requires an API key, sent as a bearer
token:

```
Authorization: Bearer pp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
```

### Getting a key

API keys are created in the admin UI: **Settings → API keys → Create key**. The
plaintext key is shown **exactly once** at creation — copy it immediately. Only a
SHA-256 hash is stored; the key cannot be recovered later. Revoke a key any time
from the same page.

> Keys are intentionally **not** creatable through the API itself, to avoid
> privilege escalation: an existing key cannot mint new keys.

### Auth errors

Missing/invalid keys return `401`:

```json
{ "error": { "code": "unauthorized", "message": "Invalid API key." } }
```

---

## Error format

All errors use a consistent envelope:

```json
{ "error": { "code": "<machine_code>", "message": "<human message>" } }
```

| HTTP | `code`             | When                                             |
|------|--------------------|--------------------------------------------------|
| 400  | `invalid_request`  | Malformed JSON, missing path, bad base64.        |
| 400  | `unsafe_path`      | File path escapes the site (traversal/absolute). |
| 401  | `unauthorized`     | Missing or invalid API key.                      |
| 404  | `not_found`        | Project or file does not exist.                  |
| 409  | `slug_exists`      | Slug already in use.                             |
| 413  | `too_large`        | Body or file content exceeds the size limit.     |
| 422  | `validation_error` | A field failed validation.                        |
| 500  | `internal`         | Unexpected server error.                         |

---

## Discovery

### `GET /api/v1`  *(public, no auth)*

Returns basic API metadata — useful for capability discovery.

```sh
curl -s https://example.com/api/v1
```

```json
{
  "name": "Paper Plane API",
  "version": "v1",
  "auth": "Send 'Authorization: Bearer <api-key>'. Create keys in the admin UI under Settings.",
  "resources": {
    "projects": "/api/v1/projects",
    "files": "/api/v1/projects/{id}/files"
  }
}
```

---

## Projects

A **project** is one static site. Its `id` is an immutable identifier used in all
API paths; its `slug` is the public URL segment (`/<slug>/`).

### Project object

```json
{
  "id": "V1StGXR8_Z5j",
  "name": "Marketing site",
  "slug": "marketing",
  "status": "active",
  "index_file": "index.html",
  "site_url": "/marketing/",
  "file_count": 4,
  "size": 20481,
  "created_at": "2026-07-17T09:00:00Z",
  "updated_at": "2026-07-17T09:05:00Z"
}
```

| Field        | Type   | Notes                                               |
|--------------|--------|-----------------------------------------------------|
| `id`         | string | Immutable; used in API paths.                       |
| `name`       | string | Display name.                                       |
| `slug`       | string | URL segment, unique, lowercase `[a-z0-9-]`. Immutable after creation. |
| `status`     | string | `active` (served) or `unlinked` (returns 404).      |
| `index_file` | string | Landing page filename served at the site root.      |
| `site_url`   | string | Public path of the site.                            |
| `file_count` | int    | Number of files.                                    |
| `size`       | int    | Total file size in bytes.                           |
| `created_at` / `updated_at` | string | RFC 3339 UTC timestamps.             |

### `GET /api/v1/projects` — list

```sh
curl -s https://example.com/api/v1/projects \
  -H "Authorization: Bearer $PP_API_KEY"
```

```json
{ "projects": [ { "id": "…", "slug": "marketing", "…": "…" } ] }
```

### `POST /api/v1/projects` — create

Body:

| Field    | Required | Notes                                            |
|----------|----------|--------------------------------------------------|
| `name`   | yes      | Non-empty.                                       |
| `slug`   | yes      | `^[a-z0-9][a-z0-9-]*$`, ≤63 chars, not reserved (`_app`, `api`, `healthz`, `static`, `assets`). |
| `status` | no       | `active` (default) or `unlinked`.                |

A placeholder `index.html` is created so the site serves immediately. Responds
`201` with the project object.

```sh
curl -s -X POST https://example.com/api/v1/projects \
  -H "Authorization: Bearer $PP_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"Marketing site","slug":"marketing"}'
```

### `GET /api/v1/projects/{id}` — fetch one

Responds `200` with the project object, or `404`.

### `PATCH /api/v1/projects/{id}` — update

Send only the fields you want to change (all optional):

| Field        | Notes                                                          |
|--------------|---------------------------------------------------------------|
| `name`       | Non-empty when present.                                        |
| `status`     | `active` or `unlinked`.                                        |
| `index_file` | Must reference an existing file; `""` resets to `index.html`.  |

```sh
curl -s -X PATCH https://example.com/api/v1/projects/V1StGXR8_Z5j \
  -H "Authorization: Bearer $PP_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"status":"unlinked"}'
```

Responds `200` with the updated project.

### `DELETE /api/v1/projects/{id}` — delete

Removes the project **and all its files**. Responds `204 No Content`.

---

## Files

Files live under a project. The path is the slash-separated location within the
site (e.g. `css/app.css`). Paths are validated server-side; traversal (`..`) and
absolute paths are rejected.

### `GET /api/v1/projects/{id}/files` — list

```json
{ "files": [ { "path": "index.html", "size": 128 },
             { "path": "css/app.css", "size": 512 } ] }
```

### `GET /api/v1/projects/{id}/files/{path}` — read a file

Text files are returned as UTF-8 (`encoding: "utf8"`); binary files are
base64-encoded (`encoding: "base64"`).

```sh
curl -s https://example.com/api/v1/projects/V1StGXR8_Z5j/files/index.html \
  -H "Authorization: Bearer $PP_API_KEY"
```

```json
{
  "path": "index.html",
  "size": 128,
  "encoding": "utf8",
  "content": "<!DOCTYPE html>…"
}
```

### `PUT /api/v1/projects/{id}/files/{path}` — create or overwrite

Body:

| Field      | Required | Notes                                             |
|------------|----------|---------------------------------------------------|
| `content`  | yes      | The file contents.                                |
| `encoding` | no       | `utf8` (default) or `base64` (for binary files).  |

Parent directories are created automatically. Responds `201` when the file was
created, `200` when an existing file was overwritten. The response is the file
metadata:

```json
{ "path": "index.html", "size": 143 }
```

Write a text file:

```sh
curl -s -X PUT https://example.com/api/v1/projects/V1StGXR8_Z5j/files/index.html \
  -H "Authorization: Bearer $PP_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"content":"<!DOCTYPE html><title>Hi</title>"}'
```

Write a binary file (e.g. an image), base64-encoded:

```sh
B64=$(base64 < logo.png | tr -d '\n')
curl -s -X PUT https://example.com/api/v1/projects/V1StGXR8_Z5j/files/logo.png \
  -H "Authorization: Bearer $PP_API_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"content\":\"$B64\",\"encoding\":\"base64\"}"
```

### `DELETE /api/v1/projects/{id}/files/{path}` — delete a file

Responds `204 No Content`, or `404` if the file does not exist.

---

## Limits

- Max file content: **50 MiB** (base64 requests may be up to ~2× that on the
  wire).
- Slugs: `^[a-z0-9][a-z0-9-]*$`, ≤63 chars, and not a reserved word.

## A minimal end-to-end example

```sh
export PP="https://example.com"
export PP_API_KEY="pp_…"          # created in Settings → API keys

# 1. Create a project.
ID=$(curl -s -X POST "$PP/api/v1/projects" \
  -H "Authorization: Bearer $PP_API_KEY" -H "Content-Type: application/json" \
  -d '{"name":"Demo","slug":"demo"}' | jq -r .id)

# 2. Upload a landing page.
curl -s -X PUT "$PP/api/v1/projects/$ID/files/index.html" \
  -H "Authorization: Bearer $PP_API_KEY" -H "Content-Type: application/json" \
  -d '{"content":"<!DOCTYPE html><title>Demo</title><h1>Hello</h1>"}'

# 3. Visit the live site at $PP/demo/
```

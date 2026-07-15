# Template Contract (for the UI agent)

This document is the source of truth for the data each server handler passes to
each template. The backend renders templates by **filename** via
`server.renderer.render(w, r, "<file>.html", data)` (see
`internal/server/render.go`). Keep filenames and field names stable, or update
this file and the corresponding handler together.

## Rendering mechanics

- Templates are embedded from `web/templates/*.html` (`web/embed.go`) and parsed
  once at startup into a single `html/template` set. Execute-by-filename.
- `html/template` auto-escapes; do not hand-build HTML from untrusted data.
- Every mutating form (POST/PUT/DELETE) **must** include the CSRF token in a
  hidden input named exactly `csrf_token` (or send header `X-CSRF-Token` for
  htmx requests). The CSRF middleware rejects mutations without a valid token
  with HTTP 403. See `internal/server/middleware.go`.
- A fresh CSRF token is available to authenticated pages via the data struct
  field `CSRFToken`. For htmx, expose it however the layout prefers (e.g. a
  `<meta>` tag or `hx-headers`).

## Handlers → templates (implemented in Phases 0–2)

### `GET /_app/login` → `login.html`
Renders the login form. Data:

| Field       | Type   | Notes                                            |
|-------------|--------|--------------------------------------------------|
| `CSRFToken` | string | Required. Put in hidden `csrf_token` input.      |
| `Error`     | string | Optional. Present + non-empty on failed login.   |

Form POSTs to `/_app/login` with fields:
- `password` (text)
- `csrf_token` (hidden)

On success: session cookie set, redirect `303` to `/_app`.
On failure: re-render `login.html` with `Error` set, HTTP 200.

### `POST /_app/logout`
No template. Clears the session cookie, redirects `303` to `/_app/login`.
Must carry a valid CSRF token.

### `GET /_app/setup` → `setup.html` (standalone, pre-auth)
First-run "set a password" page. There is no `ADMIN_PASSWORD` env var; the admin
password is chosen here on a fresh instance and stored as a bcrypt hash. Data
`setupData`:

| Field       | Type   | Notes                                            |
|-------------|--------|--------------------------------------------------|
| `CSRFToken` | string | Required. Hidden `csrf_token` input.             |
| `Error`     | string | Optional. Present on validation failure.         |

Form POSTs to `/_app/setup` with fields:
- `new_password` (text, required, ≥8 and ≤72 chars)
- `confirm_password` (text, required, must equal `new_password`)
- `csrf_token` (hidden)

Both `GET` and `POST` self-guard: once a password exists they redirect `303` to
`/_app/login` (setup can never reset an existing credential). On success the
hash is stored, a session cookie is issued (auto-login), and the response
redirects `303` to `/_app/`. `GET /_app/login` redirects here while no password
is configured.

### `GET /_app/settings` → `settings.html`
Authenticated account settings; currently a single **change password** form.
Data `settingsData`:

| Field       | Type   | Notes                                            |
|-------------|--------|--------------------------------------------------|
| `CSRFToken` | string | Hidden `csrf_token` input.                       |
| `Flash`     | string | Optional success message (from `?flash=`).       |
| `Error`     | string | Optional error message.                          |

Form POSTs to `/_app/settings/password` with fields:
- `current_password` (text, required; must match the stored hash)
- `new_password` (text, required, ≥8 and ≤72 chars, must differ from current)
- `confirm_password` (text, required, must equal `new_password`)
- `csrf_token` (hidden)

On success: redirect `303` to `/_app/settings?flash=Password+updated.` (the
current session stays valid — the cookie secret is not rotated). On failure:
re-render (HTTP 200) with `Error`; passwords are never echoed back.

## Handlers → templates (implemented in Phases 3 & 5)

All pages below are authenticated (`/_app/*` behind `requireAuth`). Every
mutating form must carry the `csrf_token` hidden input. All page structs expose
a `CSRFToken string`.

Flash/error strings are passed to some pages via the URL query (`?flash=...`,
`?error=...`) after a POST-redirect-GET, and are surfaced as data fields.

### `GET /_app/` and `GET /_app/projects` → `projects.html`
Project list. Data `projectsData`:

| Field       | Type            | Notes                                        |
|-------------|-----------------|----------------------------------------------|
| `CSRFToken` | string          | For the inline logout / unlink / relink / delete forms. |
| `Projects`  | []projectView   | See projectView below. Empty slice if none.  |
| `Flash`     | string          | Optional success message (from `?flash=`).   |
| `Error`     | string          | Optional error message.                      |

`projectView` fields: `ID` (string), `Name` (string), `Slug` (string),
`Status` (string: "active"|"unlinked"), `Active` (bool), `FileCount` (int),
`Size` (int64, bytes), `SizeHuman` (string, e.g. "1.2 KB"), `SiteURL` (string,
e.g. "/demo/"), `UpdatedAt` (time.Time).

Forms on this page (all POST, all need `csrf_token`):
- `/_app/projects/{id}/unlink`, `/_app/projects/{id}/relink`,
  `/_app/projects/{id}/delete`, `/_app/logout`.

### `GET /_app/projects/new` → `project_new.html`
Create form. Data `projectNewData`:

| Field       | Type   | Notes                                             |
|-------------|--------|---------------------------------------------------|
| `CSRFToken` | string | Hidden `csrf_token` input.                        |
| `Name`      | string | Prefilled on validation error.                    |
| `Slug`      | string | Prefilled on validation error.                    |
| `Error`     | string | Present on validation/duplicate-slug failure.     |

Form POSTs (multipart) to `/_app/projects` with fields:
- `name` (text, required), `slug` (text, required, `^[a-z0-9][a-z0-9-]*$`, ≤63),
  `files` (file input, `multiple`, optional; a `.zip` is extracted),
  `csrf_token` (hidden). Form must be `enctype="multipart/form-data"`.

The `files` input is wrapped in a `.dropzone` (`[data-dropzone][data-autofill]`)
enhanced by `app.js`. The `<input>` overlays only the `.dropzone__drop` area;
below it a `[data-dz-list]` container shows the **staged** files. Dropping or
browsing **accumulates** (deduped by base name, newer wins) rather than
replacing, each staged file has a remove button, and the input is kept in sync so
a normal submit uploads the staged set. When exactly one HTML file is staged,
`name`/`slug` autofill from its file name (unless the user has typed there). The
bare input still works without JavaScript (browse + native drop), minus
accumulation and the list. Server side (see below), a lone non-`index.html` HTML
upload is stored as `index.html`.

On success: redirect `303` to the file manager. On failure: re-render (HTTP 200)
with `Error`.

### `GET /_app/projects/{id}` → `project_edit.html`
Rename form (slug is immutable in MVP). Data `projectEditData`:

| Field       | Type        | Notes                              |
|-------------|-------------|------------------------------------|
| `CSRFToken` | string      | Hidden `csrf_token` input.         |
| `Project`   | projectView | The project being edited.          |
| `Flash`     | string      | Optional success message.          |
| `Error`     | string      | Optional error message.            |

Form POSTs to `/_app/projects/{id}` with `name` (text, required), `csrf_token`.

### `GET /_app/projects/{id}/files` and `GET /_app/projects/{id}/files/edit?path=<rel>` → `files.html`
File manager. The `edit` route additionally loads one text file into the editor.
Data `filesData`:

| Field         | Type        | Notes                                            |
|---------------|-------------|--------------------------------------------------|
| `CSRFToken`   | string      | For upload / save / delete forms.                |
| `Project`     | projectView | Owning project.                                  |
| `Files`       | []fileView  | All files (flat). Empty slice if none.           |
| `Editing`     | bool        | True when a file is loaded in the editor.        |
| `EditPath`    | string      | Relative path of the file being edited.          |
| `EditContent` | string      | Current file contents (auto-escaped in textarea).|
| `Flash`       | string      | Optional success message (from `?flash=`).       |
| `Error`       | string      | Optional error message (from `?error=`).         |

`fileView` fields: `Path` (string, slash-separated relative path), `Size`
(int64), `SizeHuman` (string), `Editable` (bool; text extensions only:
html/htm/css/js/txt/md/json/svg/xml/csv).

Forms on this page (all need `csrf_token`):
- Upload → POST (multipart) `/_app/projects/{id}/files`, field `files`
  (`multiple`; `.zip` extracted). `enctype="multipart/form-data"`. The input is
  wrapped in a `.dropzone` (`[data-dropzone]`, no autofill here): drops accumulate
  into a removable staged list before submit (see the create form above).
  Auto-index rule: a **single** non-`index.html` HTML upload is stored as
  `index.html`, but only when the site has no `index.html` yet — an existing one
  is never overwritten (the file keeps its own base name).
- Save edited file → POST `/_app/projects/{id}/files/save` with `path`,
  `content`. Only text-editable extensions are accepted. On success it
  redirects (`303`) back to the **editor** for the same file
  (`/_app/projects/{id}/files/edit?path=<relpath>&flash=File+saved.`) so the
  editor stays open with a success flash rather than closing to the list.
- Delete file → POST `/_app/projects/{id}/files/delete` with `path`.
- Edit link → GET `/_app/projects/{id}/files/edit?path=<relpath>`. This route
  also surfaces `?flash=` / `?error=` query values as `Flash` / `Error`.

## Not yet implemented / UI-agent notes

- `layout.html` exists only as a non-wired stub. The renderer parses all
  templates into one set and executes by filename, so a shared `content` block
  name would collide across pages; each page is currently a standalone document.
  The UI agent must reconcile real layout inheritance with the by-filename
  renderer (unique block names per page, or clone-per-page rendering).

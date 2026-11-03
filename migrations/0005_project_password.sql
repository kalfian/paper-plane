-- 0005_project_password: optional per-site visitor password.
--
-- password_hash holds the bcrypt hash of the site's password, or '' when the
-- site is public. Visitors to a protected site must enter the password (or be
-- signed in as the admin) before any file is served.

ALTER TABLE projects ADD COLUMN password_hash TEXT NOT NULL DEFAULT '';

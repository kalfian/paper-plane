-- 0002_index_file: the filename served as each project's landing page at the
-- site root. Defaults to index.html; a project may point it at any uploaded
-- file (e.g. about.html) so an uploaded page need not be renamed to index.html.

ALTER TABLE projects ADD COLUMN index_file TEXT NOT NULL DEFAULT 'index.html';

-- 0004_api_key_scope: scope each access token to the surfaces it may call.
--
--   all → REST API (/api/v1/*) and MCP (/api/mcp)
--   api → REST API only
--   mcp → MCP only
--
-- Keys created before this migration keep working everywhere ('all').

ALTER TABLE api_keys ADD COLUMN scope TEXT NOT NULL DEFAULT 'all';

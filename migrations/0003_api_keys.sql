-- 0003_api_keys: table for REST API access tokens.
--
-- Only the SHA-256 hash of each key is stored; the plaintext token is shown to
-- the operator exactly once, at creation time. Keys are matched at request time
-- by hashing the presented Bearer token and looking it up by key_hash.

CREATE TABLE IF NOT EXISTS api_keys (
  id           TEXT PRIMARY KEY,          -- nanoid
  name         TEXT NOT NULL,             -- human-friendly label
  key_hash     TEXT NOT NULL UNIQUE,      -- hex-encoded SHA-256 of the plaintext key
  created_at   TIMESTAMP NOT NULL,
  last_used_at TIMESTAMP                  -- NULL until the key is first used
);

CREATE INDEX IF NOT EXISTS idx_api_keys_key_hash ON api_keys(key_hash);

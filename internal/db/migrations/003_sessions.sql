-- Per-session job ownership and quotas (CONVERTFLOW_SESSIONS). Every new column is nullable and
-- unused with sessions off, so existing installations behave exactly as before.

-- Anonymous cookie sessions. Only the SHA-256 digest of the token is stored, never the token.
CREATE TABLE sessions (
  key bytea PRIMARY KEY CHECK (octet_length(key) = 32),
  created_at timestamptz NOT NULL DEFAULT now(),
  last_seen timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_last_seen_idx ON sessions (last_seen);

-- owner: the caller's owner key (cookie session or trusted proxy user), NULL with sessions off.
-- client_key: digest of the client IP (IPv6: /64), only recorded while a per-IP quota is set.
-- input_size: the input's size in bytes (multipart: as received; presigned: as declared, which
-- /complete verifies), for the queued-bytes quota.
ALTER TABLE jobs ADD COLUMN owner bytea;
ALTER TABLE jobs ADD COLUMN client_key bytea;
ALTER TABLE jobs ADD COLUMN input_size bigint;
CREATE INDEX jobs_owner_idx ON jobs (owner, created_at) WHERE owner IS NOT NULL;
CREATE INDEX jobs_owner_active_idx ON jobs (owner)
  WHERE owner IS NOT NULL AND status IN ('uploading','queued','probing','running');
CREATE INDEX jobs_client_active_idx ON jobs (client_key)
  WHERE client_key IS NOT NULL AND status IN ('uploading','queued','probing','running');

-- Append-only record of rate-limited actions (job creation per owner, session creation per IP),
-- counted over the trailing hour and pruned by the janitor. Deleting a job does not remove its
-- row, so delete-and-recreate cannot reset the hourly count.
CREATE TABLE rate_events (
  key bytea NOT NULL,
  at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX rate_events_key_at_idx ON rate_events (key, at);
CREATE INDEX rate_events_at_idx ON rate_events (at);

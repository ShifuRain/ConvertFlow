CREATE TABLE jobs (
  id uuid PRIMARY KEY,
  status text NOT NULL CHECK (status IN ('queued','probing','running','done','failed','canceled')),
  filename text NOT NULL,
  input_path text NOT NULL,
  output_path text NOT NULL DEFAULT '',
  options jsonb NOT NULL,
  progress double precision NOT NULL DEFAULT 0,
  encoder_used text NOT NULL DEFAULT '',
  probe jsonb,
  error text NOT NULL DEFAULT '',
  attempts int NOT NULL DEFAULT 0,
  worker_id text NOT NULL DEFAULT '',
  cancel_requested boolean NOT NULL DEFAULT false,
  heartbeat_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz
);
CREATE INDEX jobs_queue_idx ON jobs (created_at) WHERE status = 'queued';
CREATE INDEX jobs_finished_idx ON jobs (finished_at) WHERE finished_at IS NOT NULL;

CREATE TABLE workers (
  id text PRIMARY KEY,
  encoders text[] NOT NULL,
  seen_at timestamptz NOT NULL DEFAULT now()
);

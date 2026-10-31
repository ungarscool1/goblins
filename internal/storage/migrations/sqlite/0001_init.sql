CREATE TABLE events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  dedup_key TEXT NOT NULL UNIQUE,
  project TEXT NOT NULL,
  subject_key TEXT NOT NULL,
  payload TEXT NOT NULL,
  processed_at BIGINT,
  lease_owner TEXT NOT NULL DEFAULT '',
  lease_until BIGINT NOT NULL DEFAULT 0,
  not_before BIGINT NOT NULL DEFAULT 0,
  received_at BIGINT NOT NULL
);
CREATE INDEX events_pending ON events (processed_at, subject_key, seq);

CREATE TABLE tasks (
  id TEXT PRIMARY KEY,
  project TEXT NOT NULL,
  subject_key TEXT NOT NULL,
  state TEXT NOT NULL,
  version BIGINT NOT NULL,
  paused_until BIGINT NOT NULL DEFAULT 0,
  data TEXT NOT NULL,
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL,
  UNIQUE (project, subject_key)
);

CREATE TABLE transitions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id TEXT NOT NULL REFERENCES tasks (id),
  from_state TEXT NOT NULL,
  to_state TEXT NOT NULL,
  reason TEXT NOT NULL,
  event_seq BIGINT NOT NULL,
  at BIGINT NOT NULL
);
CREATE INDEX transitions_task ON transitions (task_id, id);

CREATE TABLE designs (
  task_id TEXT NOT NULL REFERENCES tasks (id),
  revision INTEGER NOT NULL,
  hash TEXT NOT NULL,
  body TEXT NOT NULL,
  created_at BIGINT NOT NULL,
  PRIMARY KEY (task_id, revision)
);

CREATE TABLE jobs (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks (id),
  project TEXT NOT NULL,
  kind TEXT NOT NULL,
  idempotency_key TEXT NOT NULL UNIQUE,
  state TEXT NOT NULL,
  attempt INTEGER NOT NULL DEFAULT 0,
  max_attempts INTEGER NOT NULL,
  priority INTEGER NOT NULL DEFAULT 0,
  limit_key TEXT NOT NULL DEFAULT '',
  not_before BIGINT NOT NULL DEFAULT 0,
  lease_owner TEXT NOT NULL DEFAULT '',
  lease_until BIGINT NOT NULL DEFAULT 0,
  payload TEXT NOT NULL DEFAULT '',
  checkpoint TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL
);
CREATE INDEX jobs_runnable ON jobs (state, priority, created_at);
CREATE INDEX jobs_task ON jobs (task_id, created_at);

CREATE TABLE transcripts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id TEXT NOT NULL,
  job_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  text TEXT NOT NULL,
  at BIGINT NOT NULL
);
CREATE INDEX transcripts_task ON transcripts (task_id, id);

CREATE TABLE cursors (
  name TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

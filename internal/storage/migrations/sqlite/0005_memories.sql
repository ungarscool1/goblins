CREATE TABLE memories (
  id TEXT PRIMARY KEY,
  namespace TEXT NOT NULL,
  status TEXT NOT NULL,
  updated_at INTEGER NOT NULL,
  record TEXT NOT NULL
);
CREATE INDEX memories_namespace ON memories (namespace);

CREATE TABLE leases (
  resource TEXT PRIMARY KEY,
  holder TEXT NOT NULL,
  acquired_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  data TEXT NOT NULL
);

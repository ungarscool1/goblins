CREATE TABLE leases (
  resource TEXT PRIMARY KEY,
  holder TEXT NOT NULL,
  acquired_at BIGINT NOT NULL,
  expires_at BIGINT NOT NULL,
  data TEXT NOT NULL
);

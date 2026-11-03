CREATE TABLE limit_holds (
  limit_key TEXT PRIMARY KEY,
  held_until BIGINT NOT NULL,
  reason TEXT NOT NULL
);

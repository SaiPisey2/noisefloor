CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS rules (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  alert_name  TEXT    NOT NULL,
  group_name  TEXT    NOT NULL,
  file        TEXT    NOT NULL DEFAULT '',
  line        INTEGER NOT NULL DEFAULT 0,
  expr        TEXT    NOT NULL DEFAULT '',
  expr_hash   TEXT    NOT NULL DEFAULT '',
  for_seconds INTEGER NOT NULL DEFAULT 0,
  labels      TEXT    NOT NULL DEFAULT '{}',
  annotations TEXT    NOT NULL DEFAULT '{}',
  first_seen  INTEGER NOT NULL,
  last_seen   INTEGER NOT NULL,
  expr_changed_at INTEGER NOT NULL DEFAULT 0,
  active      INTEGER NOT NULL DEFAULT 1,
  UNIQUE (group_name, alert_name)
);

CREATE TABLE IF NOT EXISTS episodes (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT,
  rule_id            INTEGER NOT NULL REFERENCES rules(id) ON DELETE CASCADE,
  fingerprint        TEXT    NOT NULL,
  labels             TEXT    NOT NULL DEFAULT '{}',
  started_at         INTEGER NOT NULL,
  ended_at           INTEGER NOT NULL,
  resolution_seconds INTEGER NOT NULL,
  source             TEXT    NOT NULL,
  state              TEXT    NOT NULL DEFAULT 'firing',
  UNIQUE (rule_id, fingerprint, started_at, state)
);

CREATE INDEX IF NOT EXISTS idx_episodes_rule_time ON episodes (rule_id, started_at);
CREATE INDEX IF NOT EXISTS idx_episodes_time      ON episodes (started_at);

CREATE TABLE IF NOT EXISTS silences (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  am_id      TEXT    NOT NULL UNIQUE,
  matchers   TEXT    NOT NULL DEFAULT '[]',
  created_by TEXT    NOT NULL DEFAULT '',
  comment    TEXT    NOT NULL DEFAULT '',
  starts_at  INTEGER NOT NULL,
  ends_at    INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_silences_time ON silences (starts_at, ends_at);

CREATE TABLE IF NOT EXISTS scores (
  rule_id      INTEGER NOT NULL REFERENCES rules(id) ON DELETE CASCADE,
  window_start INTEGER NOT NULL,
  window_end   INTEGER NOT NULL,
  signals      TEXT    NOT NULL DEFAULT '{}',
  noise_score  REAL    NOT NULL,
  verdict      TEXT    NOT NULL,
  confidence   REAL    NOT NULL,
  computed_at  INTEGER NOT NULL,
  PRIMARY KEY (rule_id, window_start, window_end)
);

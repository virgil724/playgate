package db

const schema = `
CREATE TABLE IF NOT EXISTS hosts (
    id          TEXT PRIMARY KEY,
    api_key     TEXT UNIQUE NOT NULL,
    name        TEXT NOT NULL,
    created_at  DATETIME NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS rooms (
    id              TEXT PRIMARY KEY,
    host_id         TEXT NOT NULL REFERENCES hosts(id),
    name            TEXT NOT NULL,
    session_seconds INTEGER NOT NULL DEFAULT 60,
    online          INTEGER NOT NULL DEFAULT 0,
    current_viewer  TEXT,
    kick_requested  INTEGER NOT NULL DEFAULT 0,
    created_at      DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at      DATETIME NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS tokens (
    code        TEXT PRIMARY KEY,
    room_id     TEXT NOT NULL REFERENCES rooms(id),
    host_id     TEXT NOT NULL REFERENCES hosts(id),
    redeemed    INTEGER NOT NULL DEFAULT 0,
    revoked     INTEGER NOT NULL DEFAULT 0,
    viewer_id   TEXT,
    created_at  DATETIME NOT NULL DEFAULT (datetime('now')),
    redeemed_at DATETIME
);

CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,
    room_id     TEXT NOT NULL REFERENCES rooms(id),
    viewer_id   TEXT NOT NULL,
    token_code  TEXT NOT NULL REFERENCES tokens(code),
    jwt         TEXT NOT NULL,
    queue_pos   INTEGER NOT NULL DEFAULT 0,
    active      INTEGER NOT NULL DEFAULT 1,
    created_at  DATETIME NOT NULL DEFAULT (datetime('now')),
    expires_at  DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_rooms_host ON rooms(host_id);
CREATE INDEX IF NOT EXISTS idx_tokens_room ON tokens(room_id);
CREATE INDEX IF NOT EXISTS idx_tokens_code ON tokens(code);
CREATE INDEX IF NOT EXISTS idx_sessions_room ON sessions(room_id);
`

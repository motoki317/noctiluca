CREATE TABLE IF NOT EXISTS events (
    id INTEGER PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('message', 'message_edited', 'reaction')),
    conversation TEXT NOT NULL,
    parent TEXT NOT NULL,
    message_ref TEXT NOT NULL DEFAULT '',
    direct INTEGER NOT NULL DEFAULT 0 CHECK (direct IN (0, 1)),
    sender_id TEXT NOT NULL,
    sender_display_name TEXT NOT NULL,
    sender_kind TEXT NOT NULL CHECK (sender_kind IN ('human', 'bot')),
    text TEXT NOT NULL,
    attachments TEXT NOT NULL CHECK (json_valid(attachments)),
    mentions_me INTEGER NOT NULL CHECK (mentions_me IN (0, 1)),
    received_at DATETIME NOT NULL,
    deleted INTEGER NOT NULL DEFAULT 0 CHECK (deleted IN (0, 1)),
    accepted INTEGER CHECK (accepted IN (0, 1))
);
CREATE INDEX IF NOT EXISTS events_conversation_time ON events(conversation, received_at DESC, id DESC);

CREATE VIRTUAL TABLE IF NOT EXISTS events_fts USING fts5(
    text, content='events', content_rowid='id', tokenize='trigram'
);
CREATE TRIGGER IF NOT EXISTS events_insert AFTER INSERT ON events BEGIN
    INSERT INTO events_fts(rowid, text) VALUES (new.id, new.text);
END;
CREATE TRIGGER IF NOT EXISTS events_delete AFTER DELETE ON events BEGIN
    INSERT INTO events_fts(events_fts, rowid, text) VALUES ('delete', old.id, old.text);
END;
CREATE TRIGGER IF NOT EXISTS events_update AFTER UPDATE OF text ON events BEGIN
    INSERT INTO events_fts(events_fts, rowid, text) VALUES ('delete', old.id, old.text);
    INSERT INTO events_fts(rowid, text) VALUES (new.id, new.text);
END;

CREATE TABLE IF NOT EXISTS sends (
    id INTEGER PRIMARY KEY,
    message_id TEXT NOT NULL,
    conversation TEXT NOT NULL,
    text TEXT NOT NULL,
    attachments TEXT NOT NULL CHECK (json_valid(attachments)),
    sent_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS sends_conversation_time ON sends(conversation, sent_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS model_events (
    id INTEGER PRIMARY KEY,
    session_key TEXT NOT NULL,
    kind TEXT NOT NULL,
    text TEXT NOT NULL,
    tool_call TEXT NOT NULL CHECK (json_valid(tool_call)),
    error TEXT NOT NULL,
    recorded_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS model_events_session_time ON model_events(session_key, recorded_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS sessions (
    key TEXT PRIMARY KEY NOT NULL,
    resume_token TEXT NOT NULL,
    context_json TEXT NOT NULL DEFAULT 'null' CHECK (json_valid(context_json)),
    turn_json TEXT NOT NULL DEFAULT 'null' CHECK (json_valid(turn_json))
);

CREATE TABLE IF NOT EXISTS schedules (
    name TEXT PRIMARY KEY NOT NULL,
    cron TEXT NOT NULL,
    tz TEXT NOT NULL,
    prompt TEXT NOT NULL,
    workdir TEXT NOT NULL,
    actor TEXT NOT NULL DEFAULT 'owner' CHECK (actor IN ('owner', 'member')),
    next_run DATETIME NOT NULL
);



CREATE TABLE IF NOT EXISTS oauth_tokens (
    server TEXT PRIMARY KEY NOT NULL,
    registration_json TEXT NOT NULL DEFAULT 'null' CHECK (json_valid(registration_json)),
    access_token TEXT NOT NULL,
    token_type TEXT NOT NULL,
    refresh_token TEXT NOT NULL,
    expiry DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS consolidation (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    last_run DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS events_time ON events(received_at, id);
CREATE INDEX IF NOT EXISTS sends_time ON sends(sent_at, id);
CREATE INDEX IF NOT EXISTS model_events_time ON model_events(recorded_at, id);

CREATE TABLE IF NOT EXISTS inbox (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    source_key TEXT NOT NULL UNIQUE,
    session_key TEXT NOT NULL,
    actor TEXT NOT NULL CHECK (actor IN ('owner', 'member')),
    payload TEXT NOT NULL CHECK (json_valid(payload)),
    ready_at DATETIME NOT NULL,
    lane TEXT NOT NULL CHECK (lane IN ('conv', 'task')),
    status TEXT NOT NULL CHECK (status IN ('pending', 'dispatched', 'done', 'dead')),
    attempts INTEGER NOT NULL DEFAULT 0,
    retry_at DATETIME NOT NULL,
    deadline DATETIME NOT NULL,
    last_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS inbox_due ON inbox(status, ready_at, retry_at);
CREATE TABLE IF NOT EXISTS watches (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    command TEXT NOT NULL,
    cwd TEXT NOT NULL,
    item TEXT NOT NULL CHECK (json_valid(item)),
    outcome TEXT
);

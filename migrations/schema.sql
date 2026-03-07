PRAGMA journal_mode = WAL;

CREATE TABLE IF NOT EXISTS words (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    word TEXT NOT NULL,
    word_lower TEXT NOT NULL,
    phonetic TEXT DEFAULT '',
    definition TEXT DEFAULT '',
    translation TEXT DEFAULT '',
    pos TEXT DEFAULT '',
    collins INTEGER DEFAULT 0,
    oxford INTEGER DEFAULT 0,
    tag TEXT DEFAULT '',
    bnc INTEGER DEFAULT 0,
    frq INTEGER DEFAULT 0,
    exchange TEXT DEFAULT '',
    detail TEXT DEFAULT '',
    audio TEXT DEFAULT ''
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_words_word_lower_unique ON words(word_lower);
CREATE INDEX IF NOT EXISTS idx_words_word_lower ON words(word_lower);
CREATE INDEX IF NOT EXISTS idx_words_frq ON words(frq DESC);

CREATE VIRTUAL TABLE IF NOT EXISTS words_fts USING fts5(
    word,
    definition,
    translation,
    content='words',
    content_rowid='id'
);

CREATE TRIGGER IF NOT EXISTS words_ai AFTER INSERT ON words BEGIN
    INSERT INTO words_fts(rowid, word, definition, translation)
    VALUES (new.id, new.word, new.definition, new.translation);
END;

CREATE TRIGGER IF NOT EXISTS words_ad AFTER DELETE ON words BEGIN
    INSERT INTO words_fts(words_fts, rowid, word, definition, translation)
    VALUES ('delete', old.id, old.word, old.definition, old.translation);
END;

CREATE TRIGGER IF NOT EXISTS words_au AFTER UPDATE ON words BEGIN
    INSERT INTO words_fts(words_fts, rowid, word, definition, translation)
    VALUES ('delete', old.id, old.word, old.definition, old.translation);
    INSERT INTO words_fts(rowid, word, definition, translation)
    VALUES (new.id, new.word, new.definition, new.translation);
END;

CREATE TABLE IF NOT EXISTS analytics_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id TEXT NOT NULL,
    event_name TEXT NOT NULL,
    user_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    platform TEXT NOT NULL,
    app_version TEXT NOT NULL,
    build TEXT NOT NULL,
    system_language TEXT NOT NULL,
    system_locale TEXT NOT NULL,
    app_language TEXT NOT NULL,
    page_name TEXT DEFAULT '',
    event_time_ms INTEGER NOT NULL,
    duration_ms INTEGER DEFAULT NULL,
    params_json TEXT NOT NULL DEFAULT '{}',
    server_time_ms INTEGER NOT NULL,
    created_at_ms INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_analytics_event_id_unique ON analytics_events(event_id);
CREATE INDEX IF NOT EXISTS idx_analytics_event_time ON analytics_events(event_time_ms DESC);
CREATE INDEX IF NOT EXISTS idx_analytics_event_name_time ON analytics_events(event_name, event_time_ms DESC);
CREATE INDEX IF NOT EXISTS idx_analytics_user_time ON analytics_events(user_id, event_time_ms DESC);
CREATE INDEX IF NOT EXISTS idx_analytics_session_time ON analytics_events(session_id, event_time_ms DESC);

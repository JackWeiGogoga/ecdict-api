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

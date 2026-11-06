-- +migrate Up

CREATE TABLE book_variants (
    id         INTEGER PRIMARY KEY,
    book_id    INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    media_type TEXT NOT NULL CHECK (media_type IN ('ebook', 'audiobook')),
    language   TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'wanted' CHECK (status IN ('wanted', 'downloading', 'imported', 'review')),
    UNIQUE (book_id, media_type, language)
);

-- Do not infer a variant or a language for an existing file or download.
ALTER TABLE book_files ADD COLUMN variant_id INTEGER REFERENCES book_variants(id);
ALTER TABLE book_files ADD COLUMN language TEXT NOT NULL DEFAULT '';
ALTER TABLE book_files ADD COLUMN verification TEXT NOT NULL DEFAULT 'unverified';
ALTER TABLE downloads ADD COLUMN variant_id INTEGER REFERENCES book_variants(id);
ALTER TABLE requests ADD COLUMN languages_json TEXT NOT NULL DEFAULT '["eng"]';

CREATE TABLE variant_rejections (
    id          INTEGER PRIMARY KEY,
    book_id     INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    variant_id  INTEGER NOT NULL REFERENCES book_variants(id),
    download_id INTEGER REFERENCES downloads(id),
    source_path TEXT NOT NULL,
    reason      TEXT NOT NULL,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (book_id, variant_id, source_path)
);

-- +migrate Down

-- Restore the pre-migration backup for a complete rollback. This removes the
-- new metadata and tables, but leaves every existing book_files row and path.
DROP TABLE IF EXISTS variant_rejections;
ALTER TABLE downloads DROP COLUMN variant_id;
ALTER TABLE book_files DROP COLUMN variant_id;
ALTER TABLE book_files DROP COLUMN language;
ALTER TABLE book_files DROP COLUMN verification;
ALTER TABLE requests DROP COLUMN languages_json;
DROP TABLE IF EXISTS book_variants;

package repo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type WordRepository struct {
	db *sql.DB
}

func NewWordRepository(db *sql.DB) *WordRepository {
	return &WordRepository{db: db}
}

func (r *WordRepository) UpsertWord(ctx context.Context, w Word) error {
	_, err := r.db.ExecContext(ctx, `
INSERT INTO words (
    word, word_lower, phonetic, definition, translation, pos,
    collins, oxford, tag, bnc, frq, exchange, detail, audio
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(word_lower) DO UPDATE SET
    word = excluded.word,
    phonetic = excluded.phonetic,
    definition = excluded.definition,
    translation = excluded.translation,
    pos = excluded.pos,
    collins = excluded.collins,
    oxford = excluded.oxford,
    tag = excluded.tag,
    bnc = excluded.bnc,
    frq = excluded.frq,
    exchange = excluded.exchange,
    detail = excluded.detail,
    audio = excluded.audio;
`,
		w.Word,
		strings.ToLower(w.Word),
		w.Phonetic,
		w.Definition,
		w.Translation,
		w.Pos,
		w.Collins,
		w.Oxford,
		w.Tag,
		w.BNC,
		w.FRQ,
		w.Exchange,
		w.Detail,
		w.Audio,
	)
	return err
}

func (r *WordRepository) GetByWord(ctx context.Context, word string) (*Word, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT id, word, phonetic, definition, translation, pos, collins, oxford, tag, bnc, frq, exchange, detail, audio
FROM words
WHERE word_lower = ?
LIMIT 1
`, strings.ToLower(strings.TrimSpace(word)))

	var w Word
	err := row.Scan(
		&w.ID,
		&w.Word,
		&w.Phonetic,
		&w.Definition,
		&w.Translation,
		&w.Pos,
		&w.Collins,
		&w.Oxford,
		&w.Tag,
		&w.BNC,
		&w.FRQ,
		&w.Exchange,
		&w.Detail,
		&w.Audio,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &w, nil
}

func (r *WordRepository) Suggest(ctx context.Context, q string, limit int) ([]Word, error) {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return []Word{}, nil
	}

	rows, err := r.db.QueryContext(ctx, `
SELECT id, word, phonetic, definition, translation, pos, collins, oxford, tag, bnc, frq, exchange, detail, audio
FROM words
WHERE word_lower LIKE ?
ORDER BY frq DESC, LENGTH(word) ASC
LIMIT ?
`, q+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Word, 0, limit)
	for rows.Next() {
		var w Word
		if err := rows.Scan(
			&w.ID,
			&w.Word,
			&w.Phonetic,
			&w.Definition,
			&w.Translation,
			&w.Pos,
			&w.Collins,
			&w.Oxford,
			&w.Tag,
			&w.BNC,
			&w.FRQ,
			&w.Exchange,
			&w.Detail,
			&w.Audio,
		); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (r *WordRepository) Search(ctx context.Context, q string, mode string, page int, pageSize int) ([]Word, int, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return []Word{}, 0, nil
	}

	offset := (page - 1) * pageSize

	switch strings.ToLower(mode) {
	case "fuzzy":
		return r.searchFuzzy(ctx, q, pageSize, offset)
	default:
		return r.searchPrefix(ctx, q, pageSize, offset)
	}
}

func (r *WordRepository) searchPrefix(ctx context.Context, q string, pageSize int, offset int) ([]Word, int, error) {
	keyword := strings.ToLower(q)
	total, err := r.countPrefix(ctx, keyword)
	if err != nil {
		return nil, 0, err
	}

	rows, err := r.db.QueryContext(ctx, `
SELECT id, word, phonetic, definition, translation, pos, collins, oxford, tag, bnc, frq, exchange, detail, audio
FROM words
WHERE word_lower LIKE ? OR translation LIKE ?
ORDER BY CASE WHEN word_lower = ? THEN 0 ELSE 1 END, frq DESC, LENGTH(word) ASC
LIMIT ? OFFSET ?
`, keyword+"%", "%"+q+"%", keyword, pageSize, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	words, err := scanWords(rows, pageSize)
	if err != nil {
		return nil, 0, err
	}

	return words, total, nil
}

func (r *WordRepository) searchFuzzy(ctx context.Context, q string, pageSize int, offset int) ([]Word, int, error) {
	// FTS5 query: tokenize by spaces and append wildcard for prefix match.
	terms := strings.Fields(strings.ToLower(q))
	if len(terms) == 0 {
		return []Word{}, 0, nil
	}
	for i := range terms {
		terms[i] = terms[i] + "*"
	}
	match := strings.Join(terms, " AND ")

	total, err := r.countFuzzy(ctx, match)
	if err != nil {
		return nil, 0, err
	}

	rows, err := r.db.QueryContext(ctx, `
SELECT w.id, w.word, w.phonetic, w.definition, w.translation, w.pos, w.collins, w.oxford, w.tag, w.bnc, w.frq, w.exchange, w.detail, w.audio
FROM words_fts f
JOIN words w ON w.id = f.rowid
WHERE words_fts MATCH ?
ORDER BY bm25(words_fts), w.frq DESC
LIMIT ? OFFSET ?
`, match, pageSize, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	words, err := scanWords(rows, pageSize)
	if err != nil {
		return nil, 0, err
	}

	return words, total, nil
}

func (r *WordRepository) countPrefix(ctx context.Context, keyword string) (int, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT COUNT(1)
FROM words
WHERE word_lower LIKE ? OR translation LIKE ?
`, keyword+"%", "%"+keyword+"%")

	var total int
	if err := row.Scan(&total); err != nil {
		return 0, err
	}

	return total, nil
}

func (r *WordRepository) countFuzzy(ctx context.Context, match string) (int, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT COUNT(1)
FROM words_fts
WHERE words_fts MATCH ?
`, match)

	var total int
	if err := row.Scan(&total); err != nil {
		return 0, err
	}

	return total, nil
}

func scanWords(rows *sql.Rows, capHint int) ([]Word, error) {
	out := make([]Word, 0, capHint)
	for rows.Next() {
		var w Word
		if err := rows.Scan(
			&w.ID,
			&w.Word,
			&w.Phonetic,
			&w.Definition,
			&w.Translation,
			&w.Pos,
			&w.Collins,
			&w.Oxford,
			&w.Tag,
			&w.BNC,
			&w.FRQ,
			&w.Exchange,
			&w.Detail,
			&w.Audio,
		); err != nil {
			return nil, err
		}
		out = append(out, w)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return out, nil
}

func ValidatePagination(page, pageSize int) (int, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	if page > 100000 {
		return 0, 0, fmt.Errorf("page is too large")
	}
	return page, pageSize, nil
}

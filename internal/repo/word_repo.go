package repo

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"
)

const (
	DefaultPageSize = 20
	MaxPageSize     = 100
	MaxPage         = 50
)

type WordRepository struct {
	db *sql.DB
}

func NewWordRepository(db *sql.DB) *WordRepository {
	return &WordRepository{db: db}
}

const wordColumns = `id, word, phonetic, definition, translation, pos, collins, oxford, tag, bnc, frq, exchange, detail, audio`

const upsertSQL = `
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
    audio = excluded.audio`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanWord(s rowScanner) (Word, error) {
	var w Word
	err := s.Scan(&w.ID, &w.Word, &w.Phonetic, &w.Definition, &w.Translation, &w.Pos,
		&w.Collins, &w.Oxford, &w.Tag, &w.BNC, &w.FRQ, &w.Exchange, &w.Detail, &w.Audio)
	return w, err
}

func scanWords(rows *sql.Rows, capHint int) ([]Word, error) {
	defer rows.Close()
	out := make([]Word, 0, capHint)
	for rows.Next() {
		w, err := scanWord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// UpsertWord inserts or updates a single word keyed by its lowercase form.
func (r *WordRepository) UpsertWord(ctx context.Context, w Word) error {
	return r.UpsertWords(ctx, []Word{w})
}

// UpsertWords upserts a batch in one transaction with a prepared statement (used by the importer).
// Rows with an empty word are skipped.
func (r *WordRepository) UpsertWords(ctx context.Context, words []Word) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, upsertSQL)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, w := range words {
		if strings.TrimSpace(w.Word) == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, w.Word, strings.ToLower(w.Word), w.Phonetic, w.Definition,
			w.Translation, w.Pos, w.Collins, w.Oxford, w.Tag, w.BNC, w.FRQ, w.Exchange, w.Detail, w.Audio); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *WordRepository) GetByWord(ctx context.Context, word string) (*Word, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+wordColumns+` FROM words WHERE word_lower = ? LIMIT 1`,
		strings.ToLower(strings.TrimSpace(word)))
	w, err := scanWord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// Suggestion is the light projection returned by Suggest.
type Suggestion struct {
	Word        string `json:"word"`
	Phonetic    string `json:"phonetic"`
	Translation string `json:"translation"`
}

func (r *WordRepository) Suggest(ctx context.Context, q string, limit int) ([]Suggestion, error) {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return []Suggestion{}, nil
	}
	where, args := prefixRange(q)
	rows, err := r.db.QueryContext(ctx, `
SELECT word, phonetic, translation
FROM words
WHERE `+where+`
ORDER BY frq DESC, LENGTH(word) ASC
LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Suggestion, 0, limit)
	for rows.Next() {
		var s Suggestion
		if err := rows.Scan(&s.Word, &s.Phonetic, &s.Translation); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// NormalizeMode maps any mode other than "fuzzy" to "prefix".
func NormalizeMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), "fuzzy") {
		return "fuzzy"
	}
	return "prefix"
}

func (r *WordRepository) Search(ctx context.Context, q string, mode string, page int, pageSize int) ([]Word, int, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return []Word{}, 0, nil
	}
	offset := (page - 1) * pageSize
	if NormalizeMode(mode) == "fuzzy" {
		return r.searchFuzzy(ctx, q, pageSize, offset)
	}
	return r.searchPrefix(ctx, q, pageSize, offset)
}

func (r *WordRepository) searchPrefix(ctx context.Context, q string, pageSize int, offset int) ([]Word, int, error) {
	keyword := strings.ToLower(q)
	rangeWhere, rangeArgs := prefixRange(keyword)
	where := `(` + rangeWhere + `) OR translation LIKE ? ESCAPE '\'`
	args := append(rangeArgs, "%"+EscapeLike(q)+"%")

	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM words WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	queryArgs := append(append([]any{}, args...), keyword, pageSize, offset)
	rows, err := r.db.QueryContext(ctx, `
SELECT `+wordColumns+`
FROM words
WHERE `+where+`
ORDER BY CASE WHEN word_lower = ? THEN 0 ELSE 1 END, frq DESC, LENGTH(word) ASC
LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	words, err := scanWords(rows, pageSize)
	if err != nil {
		return nil, 0, err
	}
	return words, total, nil
}

func (r *WordRepository) searchFuzzy(ctx context.Context, q string, pageSize int, offset int) ([]Word, int, error) {
	match := FTSMatch(q)
	if match == "" {
		return []Word{}, 0, nil
	}

	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM words_fts WHERE words_fts MATCH ?`, match).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := r.db.QueryContext(ctx, `
SELECT w.id, w.word, w.phonetic, w.definition, w.translation, w.pos, w.collins, w.oxford, w.tag, w.bnc, w.frq, w.exchange, w.detail, w.audio
FROM words_fts f
JOIN words w ON w.id = f.rowid
WHERE words_fts MATCH ?
ORDER BY bm25(words_fts), w.frq DESC
LIMIT ? OFFSET ?`, match, pageSize, offset)
	if err != nil {
		return nil, 0, err
	}
	words, err := scanWords(rows, pageSize)
	if err != nil {
		return nil, 0, err
	}
	return words, total, nil
}

// FTSMatch turns user input into an FTS5 query where each whitespace-separated term is a quoted
// prefix phrase, so FTS5 operators and punctuation in user input are treated as plain text.
func FTSMatch(q string) string {
	terms := strings.Fields(strings.ToLower(q))
	for i, t := range terms {
		terms[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"*`
	}
	return strings.Join(terms, " AND ")
}

// EscapeLike escapes LIKE metacharacters for use with ESCAPE '\'.
func EscapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// prefixRange returns an index-friendly condition matching word_lower values starting with prefix.
func prefixRange(prefix string) (string, []any) {
	if upper, ok := PrefixUpperBound(prefix); ok {
		return `word_lower >= ? AND word_lower < ?`, []any{prefix, upper}
	}
	return `word_lower >= ?`, []any{prefix}
}

// PrefixUpperBound returns the smallest string greater than every string with the given prefix
// (under SQLite's BINARY collation, i.e. UTF-8 byte order == code point order).
func PrefixUpperBound(prefix string) (string, bool) {
	runes := []rune(prefix)
	for i := len(runes) - 1; i >= 0; i-- {
		next := runes[i] + 1
		if next >= 0xD800 && next <= 0xDFFF {
			next = 0xE000
		}
		if next <= utf8.MaxRune {
			runes[i] = next
			return string(runes[:i+1]), true
		}
	}
	return "", false
}

// ValidatePagination applies defaults and caps (page <= 50, page_size <= 100).
func ValidatePagination(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if page > MaxPage {
		page = MaxPage
	}
	if pageSize <= 0 {
		pageSize = DefaultPageSize
	}
	if pageSize > MaxPageSize {
		pageSize = MaxPageSize
	}
	return page, pageSize
}

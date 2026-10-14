package repo

import (
	"context"
	"path/filepath"
	"testing"

	"gogoga_dictionary/internal/db"
)

func testRepo(t *testing.T) *WordRepository {
	t.Helper()
	conn, err := db.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := db.ApplySchema(context.Background(), conn, ""); err != nil {
		t.Fatal(err)
	}
	r := NewWordRepository(conn)
	err = r.UpsertWords(context.Background(), []Word{
		{Word: "apple", Translation: "n. 苹果", FRQ: 10},
		{Word: "applet", Translation: "n. 小程序", FRQ: 1},
		{Word: "apply", Translation: "v. 申请", FRQ: 5},
		{Word: "100%", Translation: "percent sign"},
		{Word: "a_b", Translation: "underscore"},
		{Word: "aXb", Translation: "x"},
		{Word: "banana", Translation: "n. 香蕉 100% fruit"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestUpsertAndGet(t *testing.T) {
	r := testRepo(t)
	if err := r.UpsertWord(context.Background(), Word{Word: "Apple", Translation: "updated"}); err != nil {
		t.Fatal(err)
	}
	w, err := r.GetByWord(context.Background(), " APPLE ")
	if err != nil || w == nil || w.Translation != "updated" {
		t.Fatalf("w=%+v err=%v", w, err)
	}
	if w, err := r.GetByWord(context.Background(), "nope"); w != nil || err != nil {
		t.Fatalf("missing word: %v %v", w, err)
	}
}

func TestSearchEscaping(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	for _, q := range []string{`a(*`, `"`, `apple OR`, `NEAR(a b)`, `a:b`, `-x`, `^`, `* *`} {
		if _, _, err := r.Search(ctx, q, "fuzzy", 1, 10); err != nil {
			t.Errorf("fuzzy %q: %v", q, err)
		}
		if _, _, err := r.Search(ctx, q, "prefix", 1, 10); err != nil {
			t.Errorf("prefix %q: %v", q, err)
		}
	}

	items, total, err := r.Search(ctx, "appl", "prefix", 1, 10)
	if err != nil || total != 3 || len(items) != 3 || items[0].Word != "apple" {
		t.Fatalf("prefix appl: total=%d items=%v err=%v", total, items, err)
	}
	items, total, err = r.Search(ctx, "apple", "prefix", 1, 10)
	if err != nil || total != 2 || items[0].Word != "apple" {
		t.Fatalf("exact first: %v %d %v", items, total, err)
	}
	// "_" must be literal: "a_b" must not match "aXb".
	items, _, _ = r.Search(ctx, "a_", "prefix", 1, 10)
	if len(items) != 1 || items[0].Word != "a_b" {
		t.Fatalf("a_: %v", items)
	}
	// "%" in the translation LIKE must be literal.
	items, _, _ = r.Search(ctx, "0%", "prefix", 1, 10)
	if len(items) != 1 || items[0].Word != "banana" {
		t.Fatalf("0%%: %v", items)
	}
	items, _, err = r.Search(ctx, "appl", "fuzzy", 1, 10)
	if err != nil || len(items) != 3 {
		t.Fatalf("fuzzy: %v %v", items, err)
	}
}

func TestSuggest(t *testing.T) {
	r := testRepo(t)
	items, err := r.Suggest(context.Background(), "APP", 2)
	if err != nil || len(items) != 2 || items[0].Word != "apple" || items[1].Word != "apply" {
		t.Fatalf("items=%v err=%v", items, err)
	}
}

func TestHelpers(t *testing.T) {
	if got := FTSMatch(`a(* "b`); got != `"a(*"* AND """b"*` {
		t.Errorf("FTSMatch = %q", got)
	}
	if got := EscapeLike(`50%_\`); got != `50\%\_\\` {
		t.Errorf("EscapeLike = %q", got)
	}
	if got, ok := PrefixUpperBound("abc"); !ok || got != "abd" {
		t.Errorf("upper = %q", got)
	}
	if got, ok := PrefixUpperBound("a\U0010FFFF"); !ok || got != "b" {
		t.Errorf("upper maxrune = %q", got)
	}
	if got, _ := PrefixUpperBound("a퟿"); got != "a" {
		t.Errorf("upper surrogate = %q", got)
	}
	if p, ps := ValidatePagination(999, 1000); p != MaxPage || ps != MaxPageSize {
		t.Errorf("pagination = %d %d", p, ps)
	}
}

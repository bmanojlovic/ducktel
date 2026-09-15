package savedquery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveAndGet(t *testing.T) {
	store := NewStore(t.TempDir())

	q := Query{Name: "slow", SQL: "SELECT 1", Description: "d", Tags: []string{"a", "b"}}
	if err := store.Save(q); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Get("slow")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "slow" || got.SQL != "SELECT 1" || got.Description != "d" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if len(got.Tags) != 2 {
		t.Errorf("tags = %v, want 2", got.Tags)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Error("timestamps should be set on save")
	}
}

func TestSaveIsUpsertNotDuplicate(t *testing.T) {
	store := NewStore(t.TempDir())

	if err := store.Save(Query{Name: "q", SQL: "SELECT 1"}); err != nil {
		t.Fatal(err)
	}
	first, err := store.Get("q")
	if err != nil {
		t.Fatal(err)
	}

	// Saving the same name again must replace, not append.
	time.Sleep(2 * time.Millisecond)
	if err := store.Save(Query{Name: "q", SQL: "SELECT 2"}); err != nil {
		t.Fatal(err)
	}

	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 query after re-save, got %d", len(list))
	}
	if list[0].SQL != "SELECT 2" {
		t.Errorf("SQL = %q, want the updated value", list[0].SQL)
	}

	// CreatedAt is preserved across updates; UpdatedAt advances.
	if !list[0].CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("CreatedAt changed on update: %v -> %v", first.CreatedAt, list[0].CreatedAt)
	}
	if !list[0].UpdatedAt.After(first.UpdatedAt) {
		t.Errorf("UpdatedAt did not advance: %v -> %v", first.UpdatedAt, list[0].UpdatedAt)
	}
}

func TestListIsSortedByName(t *testing.T) {
	store := NewStore(t.TempDir())
	for _, n := range []string{"zeta", "alpha", "mid"} {
		if err := store.Save(Query{Name: n, SQL: "SELECT 1"}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "mid", "zeta"}
	for i, w := range want {
		if got[i].Name != w {
			t.Errorf("position %d = %q, want %q", i, got[i].Name, w)
		}
	}
}

func TestGetMissing(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.Get("nope"); err == nil {
		t.Error("Get on a missing name should error")
	}
}

func TestDelete(t *testing.T) {
	store := NewStore(t.TempDir())
	for _, n := range []string{"a", "b", "c"} {
		if err := store.Save(Query{Name: n, SQL: "SELECT 1"}); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.Delete("b"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 remaining, got %d: %+v", len(got), got)
	}
	for _, q := range got {
		if q.Name == "b" {
			t.Error("deleted query still present")
		}
	}
	// The survivors must be intact, not corrupted by slice reuse.
	names := []string{got[0].Name, got[1].Name}
	if names[0] != "a" || names[1] != "c" {
		t.Errorf("remaining = %v, want [a c]", names)
	}

	if err := store.Delete("b"); err == nil {
		t.Error("deleting a missing name should error")
	}
}

// TestLoadHandlesMissingFile covers the first-run path: no file yet is not an
// error, and the store reports an empty list.
func TestLoadHandlesMissingFile(t *testing.T) {
	store := NewStore(t.TempDir())
	got, err := store.List()
	if err != nil {
		t.Fatalf("List on empty dir: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no queries, got %d", len(got))
	}
}

// TestLoadRejectsCorruptFile verifies a malformed store surfaces an error
// rather than silently reporting no queries (which would look like data loss).
func TestLoadRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "saved_queries.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := NewStore(dir)
	if _, err := store.List(); err == nil {
		t.Error("List on a corrupt file should error, not silently return empty")
	}
}

// TestSaveCreatesMissingDir verifies the data dir is created on demand.
func TestSaveCreatesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	store := NewStore(dir)
	if err := store.Save(Query{Name: "q", SQL: "SELECT 1"}); err != nil {
		t.Fatalf("Save into a missing dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "saved_queries.json")); err != nil {
		t.Errorf("file not written: %v", err)
	}
}

// TestSavedFileIsReadableJSON guards the on-disk format, which agents and
// humans both inspect directly.
func TestSavedFileIsReadableJSON(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Save(Query{Name: "q", SQL: "SELECT 1", Tags: []string{"x"}}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "saved_queries.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded []Query
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("on-disk JSON is not parseable: %v", err)
	}
	if len(decoded) != 1 || decoded[0].Name != "q" {
		t.Errorf("decoded = %+v", decoded)
	}
}

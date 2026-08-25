package db

import (
	"path/filepath"
	"testing"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := New(dbPath)
	if err != nil {
		t.Fatalf("failed to create test database: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("failed to close test database: %v", err)
		}
	})

	return database
}

func TestGetAll_ReturnsInsertedRows(t *testing.T) {
	database := newTestDB(t)

	texts := []string{"Believe in yourself", "Keep going", "You got this"}
	for _, text := range texts {
		if _, err := database.Insert(text); err != nil {
			t.Fatalf("failed to insert motivation %q: %v", text, err)
		}
	}

	motivations, err := database.GetAll()
	if err != nil {
		t.Fatalf("GetAll returned error: %v", err)
	}

	if len(motivations) != len(texts) {
		t.Fatalf("expected %d motivations, got %d", len(texts), len(motivations))
	}

	found := make(map[string]bool)
	for _, m := range motivations {
		found[m.Text] = true
	}
	for _, text := range texts {
		if !found[text] {
			t.Errorf("expected inserted text %q to be present in GetAll results", text)
		}
	}
}

func TestDelete_ExistingRow(t *testing.T) {
	database := newTestDB(t)

	id, err := database.Insert("Delete me")
	if err != nil {
		t.Fatalf("failed to insert motivation: %v", err)
	}
	if _, err := database.Insert("Keep me"); err != nil {
		t.Fatalf("failed to insert motivation: %v", err)
	}

	countBefore, err := database.Count()
	if err != nil {
		t.Fatalf("Count returned error: %v", err)
	}

	deleted, err := database.Delete(id)
	if err != nil {
		t.Fatalf("Delete returned unexpected error: %v", err)
	}
	if !deleted {
		t.Fatalf("expected Delete to return true for existing id %d", id)
	}

	countAfter, err := database.Count()
	if err != nil {
		t.Fatalf("Count returned error: %v", err)
	}
	if countAfter != countBefore-1 {
		t.Errorf("expected count to decrement by 1, got before=%d after=%d", countBefore, countAfter)
	}

	motivations, err := database.GetAll()
	if err != nil {
		t.Fatalf("GetAll returned error: %v", err)
	}
	for _, m := range motivations {
		if m.ID == id {
			t.Errorf("expected deleted id %d to be absent from GetAll results", id)
		}
	}
}

func TestDelete_NonexistentID(t *testing.T) {
	database := newTestDB(t)

	deleted, err := database.Delete(999999)
	if err != nil {
		t.Fatalf("expected nil error for nonexistent id, got: %v", err)
	}
	if deleted {
		t.Errorf("expected Delete to return false for nonexistent id")
	}
}

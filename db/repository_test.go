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

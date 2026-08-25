package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/codegoalie/random-motivation/db"
	"github.com/labstack/echo/v4"
)

// newTestDB creates a fresh temp-file-backed database for a handler test,
// matching the style used in db/repository_test.go.
func newTestDB(t *testing.T) *db.DB {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.New(dbPath)
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

func TestMotivationQueue_NextCyclesAndWraps(t *testing.T) {
	q := NewMotivationQueue([]db.Motivation{
		{ID: 1, Text: "one"},
		{ID: 2, Text: "two"},
		{ID: 3, Text: "three"},
	})

	first, err := q.Next()
	if err != nil {
		t.Fatalf("Next returned unexpected error: %v", err)
	}

	if _, err := q.Next(); err != nil {
		t.Fatalf("Next returned unexpected error: %v", err)
	}
	if _, err := q.Next(); err != nil {
		t.Fatalf("Next returned unexpected error: %v", err)
	}

	fourth, err := q.Next()
	if err != nil {
		t.Fatalf("Next returned unexpected error: %v", err)
	}

	if fourth != first {
		t.Errorf("expected queue to wrap back to first entry %q, got %q", first, fourth)
	}
}

func TestMotivationQueue_NextOnEmptyQueueErrors(t *testing.T) {
	q := NewMotivationQueue(nil)

	if _, err := q.Next(); err == nil {
		t.Fatal("expected Next on empty queue to return an error, got nil")
	}
}

func TestMotivationQueue_AddAppendsAndAppearsInCycle(t *testing.T) {
	q := NewMotivationQueue([]db.Motivation{
		{ID: 1, Text: "one"},
		{ID: 2, Text: "two"},
	})
	q.Add(3, "three")

	seen := make(map[string]bool)
	for i := 0; i < 4; i++ {
		text, err := q.Next()
		if err != nil {
			t.Fatalf("Next returned unexpected error: %v", err)
		}
		seen[text] = true
	}

	if !seen["three"] {
		t.Errorf("expected added entry %q to appear in cycle, got %v", "three", seen)
	}
}

func TestMotivationQueue_RemoveDropsEntryPermanently(t *testing.T) {
	q := NewMotivationQueue([]db.Motivation{
		{ID: 1, Text: "one"},
		{ID: 2, Text: "two"},
		{ID: 3, Text: "three"},
	})

	if removed := q.Remove(2); !removed {
		t.Fatal("expected Remove to return true for existing id")
	}

	for i := 0; i < 6; i++ {
		text, err := q.Next()
		if err != nil {
			t.Fatalf("Next returned unexpected error: %v", err)
		}
		if text == "two" {
			t.Errorf("expected removed entry %q to never be returned, got it on iteration %d", "two", i)
		}
	}
}

func TestMotivationQueue_RemoveKeepsCurrentPosConsistent(t *testing.T) {
	t.Run("removing entry at current position", func(t *testing.T) {
		q := NewMotivationQueue([]db.Motivation{
			{ID: 1, Text: "one"},
			{ID: 2, Text: "two"},
			{ID: 3, Text: "three"},
		})

		// Advance so currentPos points at index 1 (id 2).
		if _, err := q.Next(); err != nil {
			t.Fatalf("Next returned unexpected error: %v", err)
		}

		if removed := q.Remove(2); !removed {
			t.Fatal("expected Remove to return true for existing id")
		}

		for i := 0; i < 4; i++ {
			text, err := q.Next()
			if err != nil {
				t.Fatalf("Next returned unexpected error: %v", err)
			}
			if text == "two" {
				t.Errorf("expected removed entry to never be returned, got it on iteration %d", i)
			}
		}
	})

	t.Run("removing last element", func(t *testing.T) {
		q := NewMotivationQueue([]db.Motivation{
			{ID: 1, Text: "one"},
			{ID: 2, Text: "two"},
			{ID: 3, Text: "three"},
		})

		// Advance to the last index (index 2, id 3).
		if _, err := q.Next(); err != nil {
			t.Fatalf("Next returned unexpected error: %v", err)
		}
		if _, err := q.Next(); err != nil {
			t.Fatalf("Next returned unexpected error: %v", err)
		}

		if removed := q.Remove(3); !removed {
			t.Fatal("expected Remove to return true for existing id")
		}

		for i := 0; i < 4; i++ {
			text, err := q.Next()
			if err != nil {
				t.Fatalf("Next returned unexpected error: %v", err)
			}
			if text == "three" {
				t.Errorf("expected removed entry to never be returned, got it on iteration %d", i)
			}
		}
	})

	t.Run("removing the only element", func(t *testing.T) {
		q := NewMotivationQueue([]db.Motivation{
			{ID: 1, Text: "only"},
		})

		if removed := q.Remove(1); !removed {
			t.Fatal("expected Remove to return true for existing id")
		}

		if _, err := q.Next(); err == nil {
			t.Fatal("expected Next on now-empty queue to return an error, got nil")
		}
	})
}

func TestMotivationQueue_RemoveNonexistentIDReturnsFalse(t *testing.T) {
	q := NewMotivationQueue([]db.Motivation{
		{ID: 1, Text: "one"},
		{ID: 2, Text: "two"},
		{ID: 3, Text: "three"},
	})

	if removed := q.Remove(999); removed {
		t.Fatal("expected Remove to return false for nonexistent id")
	}

	seen := make(map[string]bool)
	for i := 0; i < 6; i++ {
		text, err := q.Next()
		if err != nil {
			t.Fatalf("Next returned unexpected error: %v", err)
		}
		seen[text] = true
	}

	for _, want := range []string{"one", "two", "three"} {
		if !seen[want] {
			t.Errorf("expected entry %q to still be present after failed Remove, got %v", want, seen)
		}
	}
}

func TestListMotivations_WithRows(t *testing.T) {
	database := newTestDB(t)

	texts := []string{"Believe in yourself", "Keep going"}
	for _, text := range texts {
		if _, err := database.Insert(text); err != nil {
			t.Fatalf("failed to insert motivation %q: %v", text, err)
		}
	}

	queue := NewMotivationQueue(nil)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/motivations", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set("db", database)
	c.Set("queue", queue)

	if err := listMotivations(c); err != nil {
		t.Fatalf("listMotivations returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}

	var got []db.Motivation
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal response body: %v", err)
	}

	if len(got) != len(texts) {
		t.Fatalf("expected %d motivations, got %d", len(texts), len(got))
	}

	found := make(map[string]bool)
	for _, m := range got {
		found[m.Text] = true
	}
	for _, text := range texts {
		if !found[text] {
			t.Errorf("expected text %q to be present in response, got %v", text, got)
		}
	}
}

func TestListMotivations_NoRowsReturnsEmptyArray(t *testing.T) {
	database := newTestDB(t)
	queue := NewMotivationQueue(nil)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/motivations", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set("db", database)
	c.Set("queue", queue)

	if err := listMotivations(c); err != nil {
		t.Fatalf("listMotivations returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}

	body := strings.TrimSpace(rec.Body.String())
	if body != "[]" {
		t.Errorf("expected body to be exactly %q, got %q", "[]", body)
	}
}

func TestDeleteMotivation_HappyPath(t *testing.T) {
	database := newTestDB(t)

	id, err := database.Insert("Delete me")
	if err != nil {
		t.Fatalf("failed to insert motivation: %v", err)
	}
	if _, err := database.Insert("Keep me"); err != nil {
		t.Fatalf("failed to insert motivation: %v", err)
	}

	motivations, err := database.GetAll()
	if err != nil {
		t.Fatalf("GetAll returned error: %v", err)
	}
	queue := NewMotivationQueue(motivations)

	e := echo.New()
	req := httptest.NewRequest(http.MethodDelete, "/motivation/"+itoa(id), nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set("db", database)
	c.Set("queue", queue)
	c.SetParamNames("id")
	c.SetParamValues(itoa(id))

	if err := deleteMotivation(c); err != nil {
		t.Fatalf("deleteMotivation returned error: %v", err)
	}

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d", http.StatusNoContent, rec.Code)
	}

	count, err := database.Count()
	if err != nil {
		t.Fatalf("Count returned error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 remaining motivation, got %d", count)
	}

	remaining, err := database.GetAll()
	if err != nil {
		t.Fatalf("GetAll returned error: %v", err)
	}
	for _, m := range remaining {
		if m.ID == id {
			t.Errorf("expected deleted id %d to be absent from GetAll results", id)
		}
	}

	for i := 0; i < 6; i++ {
		text, err := queue.Next()
		if err != nil {
			t.Fatalf("queue.Next returned unexpected error: %v", err)
		}
		if text == "Delete me" {
			t.Errorf("expected deleted motivation to never be returned by queue, got it on iteration %d", i)
		}
	}
}

func TestDeleteMotivation_UnknownID(t *testing.T) {
	database := newTestDB(t)
	queue := NewMotivationQueue(nil)

	e := echo.New()
	req := httptest.NewRequest(http.MethodDelete, "/motivation/999999", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set("db", database)
	c.Set("queue", queue)
	c.SetParamNames("id")
	c.SetParamValues("999999")

	if err := deleteMotivation(c); err != nil {
		t.Fatalf("deleteMotivation returned error: %v", err)
	}

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, rec.Code)
	}
}

func TestDeleteMotivation_NonNumericID(t *testing.T) {
	database := newTestDB(t)
	queue := NewMotivationQueue(nil)

	e := echo.New()
	req := httptest.NewRequest(http.MethodDelete, "/motivation/abc", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set("db", database)
	c.Set("queue", queue)
	c.SetParamNames("id")
	c.SetParamValues("abc")

	if err := deleteMotivation(c); err != nil {
		t.Fatalf("deleteMotivation returned error: %v", err)
	}

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
	}
}

// itoa formats an int64 id as a decimal string for building request paths
// and echo param values in tests.
func itoa(id int64) string {
	return strconv.FormatInt(id, 10)
}

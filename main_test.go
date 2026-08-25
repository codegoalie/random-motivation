package main

import (
	"testing"

	"github.com/codegoalie/random-motivation/db"
)

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

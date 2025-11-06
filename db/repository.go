package db

import (
	"database/sql"
	"fmt"
)

// Motivation represents a motivational quote
type Motivation struct {
	ID        int64
	Text      string
	CreatedAt string
}

// MotivationRepository defines the interface for motivation data access
type MotivationRepository interface {
	GetRandom() (string, error)
	Insert(text string) (int64, error)
	GetAll() ([]Motivation, error)
	Count() (int, error)
}

// GetRandom returns a random motivation from the database
func (db *DB) GetRandom() (string, error) {
	var text string
	err := db.QueryRow(`
		SELECT text FROM motivations
		ORDER BY RANDOM()
		LIMIT 1
	`).Scan(&text)

	if err == sql.ErrNoRows {
		return "", fmt.Errorf("no motivations found")
	}
	if err != nil {
		return "", fmt.Errorf("failed to get random motivation: %w", err)
	}

	return text, nil
}

// Insert adds a new motivation to the database
func (db *DB) Insert(text string) (int64, error) {
	result, err := db.Exec(`
		INSERT INTO motivations (text)
		VALUES (?)
	`, text)

	if err != nil {
		return 0, fmt.Errorf("failed to insert motivation: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to get last insert id: %w", err)
	}

	return id, nil
}

// GetAll returns all motivations from the database
func (db *DB) GetAll() ([]Motivation, error) {
	rows, err := db.Query(`
		SELECT id, text, created_at
		FROM motivations
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to get all motivations: %w", err)
	}
	defer rows.Close()

	var motivations []Motivation
	for rows.Next() {
		var m Motivation
		if err := rows.Scan(&m.ID, &m.Text, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan motivation: %w", err)
		}
		motivations = append(motivations, m)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating motivations: %w", err)
	}

	return motivations, nil
}

// Count returns the total number of motivations
func (db *DB) Count() (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM motivations`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count motivations: %w", err)
	}
	return count, nil
}

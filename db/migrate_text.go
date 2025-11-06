package db

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// MigrateFromTextFile migrates motivations from a text file to the database
func (db *DB) MigrateFromTextFile(filePath string) error {
	// Check if the file exists
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return nil // File doesn't exist, nothing to migrate
	}

	// Check if database already has data
	count, err := db.Count()
	if err != nil {
		return fmt.Errorf("failed to check database count: %w", err)
	}

	if count > 0 {
		// Database already has data, skip migration
		return nil
	}

	// Read the text file
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open text file: %w", err)
	}
	defer file.Close()

	// Parse and insert each line
	scanner := bufio.NewScanner(file)
	lineCount := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			_, err := db.Insert(line)
			if err != nil {
				return fmt.Errorf("failed to insert motivation: %w", err)
			}
			lineCount++
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading text file: %w", err)
	}

	// Rename the original file as backup
	if lineCount > 0 {
		backupPath := filePath + ".backup"
		if err := os.Rename(filePath, backupPath); err != nil {
			// Log warning but don't fail - migration succeeded
			fmt.Printf("Warning: failed to rename %s to %s: %v\n", filePath, backupPath, err)
		} else {
			fmt.Printf("Migrated %d motivations from %s to database\n", lineCount, filePath)
			fmt.Printf("Original file backed up as %s\n", backupPath)
		}
	}

	return nil
}

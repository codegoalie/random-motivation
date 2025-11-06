package db

const createMotivationsTable = `
CREATE TABLE IF NOT EXISTS motivations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    text TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_motivations_created_at ON motivations(created_at);
`

// migrate runs the database migrations
func (db *DB) migrate() error {
	_, err := db.Exec(createMotivationsTable)
	if err != nil {
		return err
	}
	return nil
}

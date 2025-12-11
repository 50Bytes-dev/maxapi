package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/jmoiron/sqlx"
)

type Migration struct {
	ID      int
	Name    string
	UpSQL   string
	DownSQL string
}

// Migrations for MaxAPI
var migrations = []Migration{
	{
		ID:    1,
		Name:  "initial_schema_max",
		UpSQL: initialSchemaMaxSQL,
	},
	{
		ID:    2,
		Name:  "add_s3_support",
		UpSQL: addS3SupportSQL,
	},
	{
		ID:    3,
		Name:  "add_message_history",
		UpSQL: addMessageHistorySQL,
	},
}

// Initial schema for MaxAPI
const initialSchemaMaxSQL = `
-- PostgreSQL version
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'users') THEN
        CREATE TABLE users (
            id TEXT PRIMARY KEY,
            name TEXT NOT NULL,
            token TEXT NOT NULL,
            webhook TEXT NOT NULL DEFAULT '',
            max_user_id BIGINT,
            auth_token TEXT DEFAULT '',
            device_id TEXT DEFAULT '',
            temp_token TEXT DEFAULT '',
            connected INTEGER DEFAULT 0,
            expiration INTEGER,
            events TEXT NOT NULL DEFAULT '',
            proxy_url TEXT DEFAULT '',
            history INTEGER DEFAULT 0
        );
        CREATE INDEX idx_users_max_user_id ON users(max_user_id);
        CREATE INDEX idx_users_token ON users(token);
    END IF;
END $$;
`

const addS3SupportSQL = `
-- PostgreSQL version
DO $$
BEGIN
    -- Add S3 configuration columns if they don't exist
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 's3_enabled') THEN
        ALTER TABLE users ADD COLUMN s3_enabled BOOLEAN DEFAULT FALSE;
    END IF;
    
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 's3_endpoint') THEN
        ALTER TABLE users ADD COLUMN s3_endpoint TEXT DEFAULT '';
    END IF;
    
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 's3_region') THEN
        ALTER TABLE users ADD COLUMN s3_region TEXT DEFAULT '';
    END IF;
    
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 's3_bucket') THEN
        ALTER TABLE users ADD COLUMN s3_bucket TEXT DEFAULT '';
    END IF;
    
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 's3_access_key') THEN
        ALTER TABLE users ADD COLUMN s3_access_key TEXT DEFAULT '';
    END IF;
    
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 's3_secret_key') THEN
        ALTER TABLE users ADD COLUMN s3_secret_key TEXT DEFAULT '';
    END IF;
    
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 's3_path_style') THEN
        ALTER TABLE users ADD COLUMN s3_path_style BOOLEAN DEFAULT TRUE;
    END IF;
    
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 's3_public_url') THEN
        ALTER TABLE users ADD COLUMN s3_public_url TEXT DEFAULT '';
    END IF;
    
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'media_delivery') THEN
        ALTER TABLE users ADD COLUMN media_delivery TEXT DEFAULT 'base64';
    END IF;
    
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 's3_retention_days') THEN
        ALTER TABLE users ADD COLUMN s3_retention_days INTEGER DEFAULT 30;
    END IF;
END $$;
`

const addMessageHistorySQL = `
-- PostgreSQL version
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'message_history') THEN
        CREATE TABLE message_history (
            id SERIAL PRIMARY KEY,
            user_id TEXT NOT NULL,
            chat_id TEXT NOT NULL,
            sender_id TEXT NOT NULL,
            message_id TEXT NOT NULL,
            timestamp TIMESTAMP NOT NULL,
            message_type TEXT NOT NULL,
            text_content TEXT,
            media_link TEXT,
            reply_to_id TEXT,
            UNIQUE(user_id, message_id)
        );
        CREATE INDEX idx_message_history_user_chat_timestamp ON message_history (user_id, chat_id, timestamp DESC);
    END IF;
END $$;
`

// GenerateRandomID creates a random string ID
func GenerateRandomID() (string, error) {
	bytes := make([]byte, 16) // 128 bits
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("failed to generate random ID: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}

// Initialize the database with migrations
func initializeSchema(db *sqlx.DB) error {
	// Create migrations table if it doesn't exist
	if err := createMigrationsTable(db); err != nil {
		return fmt.Errorf("failed to create migrations table: %w", err)
	}

	// Get already applied migrations
	applied, err := getAppliedMigrations(db)
	if err != nil {
		return fmt.Errorf("failed to get applied migrations: %w", err)
	}

	// Apply missing migrations
	for _, migration := range migrations {
		if _, ok := applied[migration.ID]; !ok {
			if err := applyMigration(db, migration); err != nil {
				return fmt.Errorf("failed to apply migration %d: %w", migration.ID, err)
			}
		}
	}

	return nil
}

func createMigrationsTable(db *sqlx.DB) error {
	var tableExists bool
	var err error

	switch db.DriverName() {
	case "postgres":
		err = db.Get(&tableExists, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables 
				WHERE table_name = 'migrations'
			)`)
	case "sqlite":
		err = db.Get(&tableExists, `
			SELECT EXISTS (
				SELECT 1 FROM sqlite_master 
				WHERE type='table' AND name='migrations'
			)`)
	default:
		return fmt.Errorf("unsupported database driver: %s", db.DriverName())
	}

	if err != nil {
		return fmt.Errorf("failed to check migrations table existence: %w", err)
	}

	if tableExists {
		return nil
	}

	_, err = db.Exec(`
		CREATE TABLE migrations (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`)
	if err != nil {
		return fmt.Errorf("failed to create migrations table: %w", err)
	}

	return nil
}

func getAppliedMigrations(db *sqlx.DB) (map[int]struct{}, error) {
	applied := make(map[int]struct{})
	var rows []struct {
		ID   int    `db:"id"`
		Name string `db:"name"`
	}

	err := db.Select(&rows, "SELECT id, name FROM migrations ORDER BY id ASC")
	if err != nil {
		return nil, fmt.Errorf("failed to query applied migrations: %w", err)
	}

	for _, row := range rows {
		applied[row.ID] = struct{}{}
	}

	return applied, nil
}

func applyMigration(db *sqlx.DB, migration Migration) error {
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	// Apply migration based on database type
		if db.DriverName() == "sqlite" {
		err = applySQLiteMigration(tx, migration)
	} else {
		_, err = tx.Exec(migration.UpSQL)
	}

	if err != nil {
		return fmt.Errorf("failed to execute migration SQL: %w", err)
	}

	// Record the migration
	if _, err = tx.Exec(`
        INSERT INTO migrations (id, name) 
        VALUES ($1, $2)`, migration.ID, migration.Name); err != nil {
		return fmt.Errorf("failed to record migration: %w", err)
	}

	return tx.Commit()
}

// applySQLiteMigration handles SQLite-specific migration logic
func applySQLiteMigration(tx *sqlx.Tx, migration Migration) error {
	var err error

	switch migration.ID {
	case 1:
		// Initial schema for SQLite
			err = createTableIfNotExistsSQLite(tx, "users", `
                CREATE TABLE users (
                    id TEXT PRIMARY KEY,
                    name TEXT NOT NULL,
                    token TEXT NOT NULL,
                    webhook TEXT NOT NULL DEFAULT '',
				max_user_id INTEGER,
				auth_token TEXT DEFAULT '',
				device_id TEXT DEFAULT '',
				temp_token TEXT DEFAULT '',
				connected INTEGER DEFAULT 0,
                    expiration INTEGER,
                    events TEXT NOT NULL DEFAULT '',
				proxy_url TEXT DEFAULT '',
				history INTEGER DEFAULT 0
                )`)
		if err == nil {
			_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_users_max_user_id ON users(max_user_id)`)
		}
		if err == nil {
			_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_users_token ON users(token)`)
		}

	case 2:
		// S3 support columns for SQLite
			err = addColumnIfNotExistsSQLite(tx, "users", "s3_enabled", "BOOLEAN DEFAULT 0")
			if err == nil {
				err = addColumnIfNotExistsSQLite(tx, "users", "s3_endpoint", "TEXT DEFAULT ''")
			}
			if err == nil {
				err = addColumnIfNotExistsSQLite(tx, "users", "s3_region", "TEXT DEFAULT ''")
			}
			if err == nil {
				err = addColumnIfNotExistsSQLite(tx, "users", "s3_bucket", "TEXT DEFAULT ''")
			}
			if err == nil {
				err = addColumnIfNotExistsSQLite(tx, "users", "s3_access_key", "TEXT DEFAULT ''")
			}
			if err == nil {
				err = addColumnIfNotExistsSQLite(tx, "users", "s3_secret_key", "TEXT DEFAULT ''")
			}
			if err == nil {
				err = addColumnIfNotExistsSQLite(tx, "users", "s3_path_style", "BOOLEAN DEFAULT 1")
			}
			if err == nil {
				err = addColumnIfNotExistsSQLite(tx, "users", "s3_public_url", "TEXT DEFAULT ''")
			}
			if err == nil {
				err = addColumnIfNotExistsSQLite(tx, "users", "media_delivery", "TEXT DEFAULT 'base64'")
			}
			if err == nil {
				err = addColumnIfNotExistsSQLite(tx, "users", "s3_retention_days", "INTEGER DEFAULT 30")
			}

	case 3:
		// Message history table for SQLite
			err = createTableIfNotExistsSQLite(tx, "message_history", `
				CREATE TABLE message_history (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					user_id TEXT NOT NULL,
				chat_id TEXT NOT NULL,
				sender_id TEXT NOT NULL,
					message_id TEXT NOT NULL,
					timestamp DATETIME NOT NULL,
					message_type TEXT NOT NULL,
					text_content TEXT,
					media_link TEXT,
				reply_to_id TEXT,
					UNIQUE(user_id, message_id)
				)`)
			if err == nil {
				_, err = tx.Exec(`
					CREATE INDEX IF NOT EXISTS idx_message_history_user_chat_timestamp 
				ON message_history (user_id, chat_id, timestamp DESC)`)
			}

	default:
		// For any future migrations, try to execute the SQL directly
		_, err = tx.Exec(migration.UpSQL)
	}

	return err
}

func createTableIfNotExistsSQLite(tx *sqlx.Tx, tableName, createSQL string) error {
	var exists int
	err := tx.Get(&exists, `
        SELECT COUNT(*) FROM sqlite_master
        WHERE type='table' AND name=?`, tableName)
	if err != nil {
		return err
	}

	if exists == 0 {
		_, err = tx.Exec(createSQL)
		return err
	}
	return nil
}

func addColumnIfNotExistsSQLite(tx *sqlx.Tx, tableName, columnName, columnDef string) error {
	var exists int
	err := tx.Get(&exists, `
        SELECT COUNT(*) FROM pragma_table_info(?)
        WHERE name = ?`, tableName, columnName)
	if err != nil {
		return fmt.Errorf("failed to check column existence: %w", err)
	}

	if exists == 0 {
		_, err = tx.Exec(fmt.Sprintf(
			"ALTER TABLE %s ADD COLUMN %s %s",
			tableName, columnName, columnDef))
		if err != nil {
			return fmt.Errorf("failed to add column: %w", err)
		}
	}
	return nil
}

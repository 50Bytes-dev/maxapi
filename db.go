package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"
)

type DatabaseConfig struct {
	Type     string
	Host     string
	Port     string
	User     string
	Password string
	Name     string
	Path     string
	SSLMode  string
}

func InitializeDatabase(exPath string) (*sqlx.DB, error) {
	config := getDatabaseConfig(exPath)

	if config.Type == "postgres" {
		return initializePostgres(config)
	}
	return initializeSQLite(config)
}

func getDatabaseConfig(exPath string) DatabaseConfig {
	dbUser := os.Getenv("DB_USER")
	dbPassword := os.Getenv("DB_PASSWORD")
	dbName := os.Getenv("DB_NAME")
	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	dbSSL := os.Getenv("DB_SSLMODE")

	sslMode := dbSSL
	if dbSSL == "true" {
		sslMode = "require"
	} else if dbSSL == "false" || dbSSL == "" {
		sslMode = "disable"
	}

	if dbUser != "" && dbPassword != "" && dbName != "" && dbHost != "" && dbPort != "" {
		return DatabaseConfig{
			Type:     "postgres",
			Host:     dbHost,
			Port:     dbPort,
			User:     dbUser,
			Password: dbPassword,
			Name:     dbName,
			SSLMode:  sslMode,
		}
	}

	return DatabaseConfig{
		Type: "sqlite",
		Path: filepath.Join(exPath, "dbdata"),
	}
}

func initializePostgres(config DatabaseConfig) (*sqlx.DB, error) {
	dsn := fmt.Sprintf(
		"user=%s password=%s dbname=%s host=%s port=%s sslmode=%s",
		config.User, config.Password, config.Name, config.Host, config.Port, config.SSLMode,
	)

	db, err := sqlx.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open postgres connection: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping postgres database: %w", err)
	}

	return db, nil
}

func initializeSQLite(config DatabaseConfig) (*sqlx.DB, error) {
	if err := os.MkdirAll(config.Path, 0751); err != nil {
		return nil, fmt.Errorf("could not create dbdata directory: %w", err)
	}

	dbPath := filepath.Join(config.Path, "users.db")
	db, err := sqlx.Open("sqlite", dbPath+"?_pragma=foreign_keys(1)&_busy_timeout=3000")
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping sqlite database: %w", err)
	}

	return db, nil
}

// HistoryMessage represents a message in history (MAX messenger)
type HistoryMessage struct {
	ID              int       `json:"id" db:"id"`
	UserID          string    `json:"user_id" db:"user_id"`
	ChatID          string    `json:"chat_id" db:"chat_id"`
	SenderID        string    `json:"sender_id" db:"sender_id"`
	MessageID       string    `json:"message_id" db:"message_id"`
	Timestamp       time.Time `json:"timestamp" db:"timestamp"`
	MessageType     string    `json:"message_type" db:"message_type"`
	TextContent     string    `json:"text_content" db:"text_content"`
	MediaLink       string    `json:"media_link" db:"media_link"`
	ReplyToID       string    `json:"reply_to_id,omitempty" db:"reply_to_id"`
}

func (s *server) saveMessageToHistory(userID, chatID, senderID, messageID, messageType, textContent, mediaLink, replyToID string) error {
	query := `INSERT INTO message_history (user_id, chat_id, sender_id, message_id, timestamp, message_type, text_content, media_link, reply_to_id)
              VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	if s.db.DriverName() == "sqlite" {
		query = `INSERT INTO message_history (user_id, chat_id, sender_id, message_id, timestamp, message_type, text_content, media_link, reply_to_id)
                 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	}
	_, err := s.db.Exec(query, userID, chatID, senderID, messageID, time.Now(), messageType, textContent, mediaLink, replyToID)
	if err != nil {
		return fmt.Errorf("failed to save message to history: %w", err)
	}
	return nil
}

func (s *server) trimMessageHistory(userID, chatID string, limit int) error {
	var query string
	if s.db.DriverName() == "postgres" {
		query = `
            DELETE FROM message_history
            WHERE id IN (
                SELECT id FROM message_history
                WHERE user_id = $1 AND chat_id = $2
                ORDER BY timestamp DESC
                OFFSET $3
            )`
	} else { // sqlite
		query = `
            DELETE FROM message_history
            WHERE id IN (
                SELECT id FROM message_history
                WHERE user_id = ? AND chat_id = ?
                ORDER BY timestamp DESC
                LIMIT -1 OFFSET ?
            )`
	}

	_, err := s.db.Exec(query, userID, chatID, limit)
	if err != nil {
		return fmt.Errorf("failed to trim message history: %w", err)
	}
	return nil
}

func (s *server) getMessageHistory(userID, chatID string, limit int) ([]HistoryMessage, error) {
	var messages []HistoryMessage
	var query string
	
	if s.db.DriverName() == "postgres" {
		query = `
            SELECT id, user_id, chat_id, sender_id, message_id, timestamp, message_type, 
                   COALESCE(text_content, '') as text_content, 
                   COALESCE(media_link, '') as media_link,
                   COALESCE(reply_to_id, '') as reply_to_id
            FROM message_history
            WHERE user_id = $1 AND chat_id = $2
            ORDER BY timestamp DESC
            LIMIT $3`
	} else {
		query = `
            SELECT id, user_id, chat_id, sender_id, message_id, timestamp, message_type, 
                   COALESCE(text_content, '') as text_content, 
                   COALESCE(media_link, '') as media_link,
                   COALESCE(reply_to_id, '') as reply_to_id
            FROM message_history
            WHERE user_id = ? AND chat_id = ?
            ORDER BY timestamp DESC
            LIMIT ?`
	}
	
	err := s.db.Select(&messages, query, userID, chatID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get message history: %w", err)
	}
	return messages, nil
}

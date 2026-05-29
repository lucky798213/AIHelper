package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"AIHelper/internal/llm"

	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	db *sql.DB
}

type sessionKeyMeta struct {
	AgentID       string
	ParentAgentID string
	Channel       string
	PeerID        string
}

func NewSQLiteStore(path string) (*SQLiteStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("sqlite session store path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create sqlite session directory: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite session store: %w", err)
	}
	db.SetMaxOpenConns(1)

	store := &SQLiteStore{db: db}
	if err := store.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	db := s.db
	s.db = nil
	return db.Close()
}

func (s *SQLiteStore) Load(ctx context.Context, sessionKey string) ([]llm.Message, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if s == nil || s.db == nil {
		return nil, errors.New("sqlite session store is closed")
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT role, name, content, reasoning_content, tool_call_id, tool_calls_json
		FROM session_messages
		WHERE session_key = ?
		ORDER BY seq ASC
	`, sessionKey)
	if err != nil {
		return nil, fmt.Errorf("load session messages: %w", err)
	}
	defer rows.Close()

	var messages []llm.Message
	for rows.Next() {
		var message llm.Message
		var toolCallsJSON string
		if err := rows.Scan(
			&message.Role,
			&message.Name,
			&message.Content,
			&message.ReasoningContent,
			&message.ToolCallID,
			&toolCallsJSON,
		); err != nil {
			return nil, fmt.Errorf("scan session message: %w", err)
		}
		if strings.TrimSpace(toolCallsJSON) != "" && strings.TrimSpace(toolCallsJSON) != "[]" {
			if err := json.Unmarshal([]byte(toolCallsJSON), &message.ToolCalls); err != nil {
				return nil, fmt.Errorf("decode tool calls: %w", err)
			}
		}
		messages = append(messages, copyMessage(message))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session messages: %w", err)
	}
	return messages, nil
}

func (s *SQLiteStore) Append(ctx context.Context, sessionKey string, message llm.Message) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if s == nil || s.db == nil {
		return errors.New("sqlite session store is closed")
	}

	toolCallsJSON := "[]"
	if len(message.ToolCalls) > 0 {
		raw, err := json.Marshal(message.ToolCalls)
		if err != nil {
			return fmt.Errorf("encode tool calls: %w", err)
		}
		toolCallsJSON = string(raw)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin append session message: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	meta := parseSessionKey(sessionKey)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (
			session_key, agent_id, parent_agent_id, channel, peer_id, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_key) DO UPDATE SET
			agent_id = CASE WHEN excluded.agent_id != '' THEN excluded.agent_id ELSE sessions.agent_id END,
			parent_agent_id = CASE WHEN excluded.parent_agent_id != '' THEN excluded.parent_agent_id ELSE sessions.parent_agent_id END,
			channel = CASE WHEN excluded.channel != '' THEN excluded.channel ELSE sessions.channel END,
			peer_id = CASE WHEN excluded.peer_id != '' THEN excluded.peer_id ELSE sessions.peer_id END,
			updated_at = excluded.updated_at
	`, sessionKey, meta.AgentID, meta.ParentAgentID, meta.Channel, meta.PeerID, now, now); err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}

	var seq int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(seq), 0) + 1
		FROM session_messages
		WHERE session_key = ?
	`, sessionKey).Scan(&seq); err != nil {
		return fmt.Errorf("next session message seq: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO session_messages (
			session_key, seq, role, name, content, reasoning_content, tool_call_id, tool_calls_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, sessionKey, seq, message.Role, message.Name, message.Content, message.ReasoningContent, message.ToolCallID, toolCallsJSON, now); err != nil {
		return fmt.Errorf("insert session message: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit append session message: %w", err)
	}
	tx = nil
	return nil
}

func (s *SQLiteStore) Replace(ctx context.Context, sessionKey string, messages []llm.Message) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if s == nil || s.db == nil {
		return errors.New("sqlite session store is closed")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin replace session messages: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := upsertSessionTx(ctx, tx, sessionKey, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_messages WHERE session_key = ?`, sessionKey); err != nil {
		return fmt.Errorf("delete old session messages: %w", err)
	}
	for i, message := range messages {
		toolCallsJSON := "[]"
		if len(message.ToolCalls) > 0 {
			raw, err := json.Marshal(message.ToolCalls)
			if err != nil {
				return fmt.Errorf("encode tool calls: %w", err)
			}
			toolCallsJSON = string(raw)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO session_messages (
				session_key, seq, role, name, content, reasoning_content, tool_call_id, tool_calls_json, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, sessionKey, i+1, message.Role, message.Name, message.Content, message.ReasoningContent, message.ToolCallID, toolCallsJSON, now); err != nil {
			return fmt.Errorf("insert replacement session message: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit replace session messages: %w", err)
	}
	tx = nil
	return nil
}

func (s *SQLiteStore) List(ctx context.Context) ([]SessionMeta, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if s == nil || s.db == nil {
		return nil, errors.New("sqlite session store is closed")
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT
			s.session_key,
			s.agent_id,
			s.parent_agent_id,
			s.channel,
			s.peer_id,
			s.created_at,
			s.updated_at,
			COUNT(m.id) AS message_count
		FROM sessions s
		LEFT JOIN session_messages m ON m.session_key = s.session_key
		GROUP BY s.session_key
		ORDER BY s.updated_at DESC, s.session_key ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var metas []SessionMeta
	for rows.Next() {
		var meta SessionMeta
		var createdAt string
		var updatedAt string
		if err := rows.Scan(
			&meta.SessionKey,
			&meta.AgentID,
			&meta.ParentAgentID,
			&meta.Channel,
			&meta.PeerID,
			&createdAt,
			&updatedAt,
			&meta.MessageCount,
		); err != nil {
			return nil, fmt.Errorf("scan session metadata: %w", err)
		}
		meta.CreatedAt = parseTime(createdAt)
		meta.UpdatedAt = parseTime(updatedAt)
		metas = append(metas, meta)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session metadata: %w", err)
	}
	return metas, nil
}

func (s *SQLiteStore) Delete(ctx context.Context, sessionKey string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if s == nil || s.db == nil {
		return errors.New("sqlite session store is closed")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE session_key = ?`, sessionKey)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Touch(ctx context.Context, sessionKey string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if s == nil || s.db == nil {
		return errors.New("sqlite session store is closed")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin touch session: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := upsertSessionTx(ctx, tx, sessionKey, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit touch session: %w", err)
	}
	tx = nil
	return nil
}

func (s *SQLiteStore) ClaimInboundMessage(ctx context.Context, receipt InboundReceipt, staleAfter time.Duration) (ClaimResult, error) {
	select {
	case <-ctx.Done():
		return ClaimResult{}, ctx.Err()
	default:
	}
	if s == nil || s.db == nil {
		return ClaimResult{}, errors.New("sqlite session store is closed")
	}

	normalized, _, err := normalizeInboundReceipt(receipt)
	if err != nil {
		return ClaimResult{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ClaimResult{}, fmt.Errorf("begin claim inbound message: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	nowTime := time.Now().UTC()
	now := nowTime.Format(time.RFC3339Nano)
	var status string
	var attempts int
	var updatedAtRaw string
	err = tx.QueryRowContext(ctx, `
		SELECT status, attempts, updated_at
		FROM inbound_message_receipts
		WHERE channel = ? AND account_id = ? AND message_id = ?
	`, normalized.Channel, normalized.AccountID, normalized.MessageID).Scan(&status, &attempts, &updatedAtRaw)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO inbound_message_receipts (
				channel, account_id, message_id, session_key, status, attempts, last_error, created_at, updated_at, completed_at
			) VALUES (?, ?, ?, ?, ?, 1, '', ?, ?, '')
		`, normalized.Channel, normalized.AccountID, normalized.MessageID, normalized.SessionKey, InboundStatusProcessing, now, now); err != nil {
			return ClaimResult{}, fmt.Errorf("insert inbound message receipt: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return ClaimResult{}, fmt.Errorf("commit claim inbound message: %w", err)
		}
		tx = nil
		return ClaimResult{Claimed: true, Status: InboundStatusProcessing, Attempts: 1}, nil
	}
	if err != nil {
		return ClaimResult{}, fmt.Errorf("load inbound message receipt: %w", err)
	}

	if status == InboundStatusCompleted {
		return ClaimResult{Duplicate: true, Status: status, Attempts: attempts}, nil
	}
	if status == InboundStatusProcessing {
		updatedAt := parseTime(updatedAtRaw)
		if staleAfter <= 0 || updatedAt.IsZero() || nowTime.Sub(updatedAt) < staleAfter {
			return ClaimResult{Duplicate: true, Status: status, Attempts: attempts}, nil
		}
	}

	attempts++
	if _, err := tx.ExecContext(ctx, `
		UPDATE inbound_message_receipts
		SET session_key = ?, status = ?, attempts = ?, last_error = '', updated_at = ?, completed_at = ''
		WHERE channel = ? AND account_id = ? AND message_id = ?
	`, normalized.SessionKey, InboundStatusProcessing, attempts, now, normalized.Channel, normalized.AccountID, normalized.MessageID); err != nil {
		return ClaimResult{}, fmt.Errorf("update inbound message receipt claim: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ClaimResult{}, fmt.Errorf("commit claim inbound message: %w", err)
	}
	tx = nil
	return ClaimResult{Claimed: true, Status: InboundStatusProcessing, Attempts: attempts}, nil
}

func (s *SQLiteStore) CompleteInboundMessage(ctx context.Context, channel, accountID, messageID string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if s == nil || s.db == nil {
		return errors.New("sqlite session store is closed")
	}

	receipt, _, err := normalizeInboundReceipt(InboundReceipt{Channel: channel, AccountID: accountID, MessageID: messageID})
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO inbound_message_receipts (
			channel, account_id, message_id, session_key, status, attempts, last_error, created_at, updated_at, completed_at
		) VALUES (?, ?, ?, '', ?, 0, '', ?, ?, ?)
		ON CONFLICT(channel, account_id, message_id) DO UPDATE SET
			status = excluded.status,
			last_error = '',
			updated_at = excluded.updated_at,
			completed_at = excluded.completed_at
	`, receipt.Channel, receipt.AccountID, receipt.MessageID, InboundStatusCompleted, now, now, now); err != nil {
		return fmt.Errorf("complete inbound message receipt: %w", err)
	}
	return nil
}

func (s *SQLiteStore) FailInboundMessage(ctx context.Context, channel, accountID, messageID string, errText string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if s == nil || s.db == nil {
		return errors.New("sqlite session store is closed")
	}

	receipt, _, err := normalizeInboundReceipt(InboundReceipt{Channel: channel, AccountID: accountID, MessageID: messageID})
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO inbound_message_receipts (
			channel, account_id, message_id, session_key, status, attempts, last_error, created_at, updated_at, completed_at
		) VALUES (?, ?, ?, '', ?, 0, ?, ?, ?, '')
		ON CONFLICT(channel, account_id, message_id) DO UPDATE SET
			status = excluded.status,
			last_error = excluded.last_error,
			updated_at = excluded.updated_at,
			completed_at = ''
	`, receipt.Channel, receipt.AccountID, receipt.MessageID, InboundStatusFailed, strings.TrimSpace(errText), now, now); err != nil {
		return fmt.Errorf("fail inbound message receipt: %w", err)
	}
	return nil
}

func (s *SQLiteStore) init(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		return fmt.Errorf("enable sqlite foreign keys: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS sessions (
			session_key TEXT PRIMARY KEY,
			agent_id TEXT NOT NULL DEFAULT '',
			parent_agent_id TEXT NOT NULL DEFAULT '',
			channel TEXT NOT NULL DEFAULT '',
			peer_id TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("create sessions table: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS session_messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_key TEXT NOT NULL,
			seq INTEGER NOT NULL,
			role TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL DEFAULT '',
			reasoning_content TEXT NOT NULL DEFAULT '',
			tool_call_id TEXT NOT NULL DEFAULT '',
			tool_calls_json TEXT NOT NULL DEFAULT '[]',
			created_at TEXT NOT NULL,
			UNIQUE(session_key, seq),
			FOREIGN KEY(session_key) REFERENCES sessions(session_key) ON DELETE CASCADE
		)
	`); err != nil {
		return fmt.Errorf("create session_messages table: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_session_messages_session_seq
		ON session_messages(session_key, seq)
	`); err != nil {
		return fmt.Errorf("create session_messages index: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS inbound_message_receipts (
			channel TEXT NOT NULL DEFAULT '',
			account_id TEXT NOT NULL DEFAULT '',
			message_id TEXT NOT NULL,
			session_key TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'processing',
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			completed_at TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(channel, account_id, message_id)
		)
	`); err != nil {
		return fmt.Errorf("create inbound_message_receipts table: %w", err)
	}
	return nil
}

func upsertSessionTx(ctx context.Context, tx *sql.Tx, sessionKey string, now string) error {
	meta := parseSessionKey(sessionKey)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (
			session_key, agent_id, parent_agent_id, channel, peer_id, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_key) DO UPDATE SET
			agent_id = CASE WHEN excluded.agent_id != '' THEN excluded.agent_id ELSE sessions.agent_id END,
			parent_agent_id = CASE WHEN excluded.parent_agent_id != '' THEN excluded.parent_agent_id ELSE sessions.parent_agent_id END,
			channel = CASE WHEN excluded.channel != '' THEN excluded.channel ELSE sessions.channel END,
			peer_id = CASE WHEN excluded.peer_id != '' THEN excluded.peer_id ELSE sessions.peer_id END,
			updated_at = excluded.updated_at
	`, sessionKey, meta.AgentID, meta.ParentAgentID, meta.Channel, meta.PeerID, now, now); err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}
	return nil
}

func parseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err == nil {
		return parsed
	}
	parsed, _ = time.Parse(time.RFC3339, value)
	return parsed
}

func parseSessionKey(sessionKey string) sessionKeyMeta {
	parts := strings.Split(sessionKey, ":")
	if len(parts) >= 3 && parts[0] == "agent" && parts[2] == "main" {
		return sessionKeyMeta{
			AgentID: parts[1],
			Channel: "main",
			PeerID:  "main",
		}
	}
	if len(parts) >= 4 && parts[0] == "agent" && parts[2] == "peer" {
		return sessionKeyMeta{
			AgentID: parts[1],
			Channel: "peer",
			PeerID:  strings.Join(parts[3:], ":"),
		}
	}
	if len(parts) >= 7 && parts[0] == "agent" && parts[2] == "account" && parts[5] == "peer" {
		return sessionKeyMeta{
			AgentID: parts[1],
			Channel: parts[4],
			PeerID:  strings.Join(parts[6:], ":"),
		}
	}
	if len(parts) >= 5 && parts[0] == "agent" && parts[3] == "direct" {
		return sessionKeyMeta{
			AgentID: parts[1],
			Channel: parts[2],
			PeerID:  strings.Join(parts[4:], ":"),
		}
	}
	if len(parts) >= 5 && parts[0] == "agent" && parts[3] == "session" {
		return sessionKeyMeta{
			AgentID: parts[1],
			Channel: parts[2],
			PeerID:  "session:" + strings.Join(parts[4:], ":"),
		}
	}
	if len(parts) >= 6 && parts[0] == "agent" && parts[2] == "parent" {
		return sessionKeyMeta{
			AgentID:       parts[1],
			ParentAgentID: parts[3],
			Channel:       parts[4],
			PeerID:        strings.Join(parts[5:], ":"),
		}
	}
	return sessionKeyMeta{}
}

// Package storage implements the SQLite-backed persistence layer.
package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"

	"github.com/secureNqwer/zerolink/core"
)

// SQLiteStorage implements core.Storage.
type SQLiteStorage struct {
	db       *sql.DB
	mediaDir string
	log      *zap.Logger
}

// NewSQLiteStorage opens (or creates) the SQLite database and runs migrations.
func NewSQLiteStorage(dbPath, mediaDir string, log *zap.Logger) (*SQLiteStorage, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(mediaDir, 0o700); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_foreign_keys=on&_cache_size=-65536", dbPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open: %w", err)
	}
	db.SetMaxOpenConns(1)

	s := &SQLiteStorage{db: db, mediaDir: mediaDir, log: log}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("storage: migrate: %w", err)
	}
	return s, nil
}

// ─── Schema ───────────────────────────────────────────────────────────────────

func (s *SQLiteStorage) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS chats (
			id                  TEXT PRIMARY KEY,
			type                INTEGER NOT NULL,
			name                TEXT,
			description         TEXT,
			avatar_hash         TEXT,
			members             TEXT NOT NULL,
			network_id          TEXT NOT NULL,
			encrypted           INTEGER NOT NULL DEFAULT 1,
			disappear_after_ns  INTEGER NOT NULL DEFAULT 0,
			default_permissions INTEGER NOT NULL DEFAULT 3,
			created_at          INTEGER NOT NULL,
			updated_at          INTEGER NOT NULL
		)`,

		`CREATE TABLE IF NOT EXISTS peers (
			node_id      TEXT NOT NULL,
			fingerprint  TEXT NOT NULL,
			display_name TEXT NOT NULL,
			avatar_hash  TEXT,
			status       INTEGER NOT NULL DEFAULT 0,
			status_text  TEXT,
			public_key   BLOB NOT NULL,
			sign_key     BLOB NOT NULL,
			last_seen    INTEGER NOT NULL,
			networks     TEXT,
			is_blocked   INTEGER NOT NULL DEFAULT 0,
			is_contact   INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (node_id, fingerprint)
		)`,

		`CREATE TABLE IF NOT EXISTS messages (
			id             TEXT PRIMARY KEY,
			chat_id        TEXT NOT NULL,
			sender_node    TEXT NOT NULL,
			sender_fp      TEXT NOT NULL,
			recipient_node TEXT,
			recipient_fp   TEXT,
			type           INTEGER NOT NULL,
			payload        BLOB NOT NULL,
			meta           TEXT NOT NULL,
			status         INTEGER NOT NULL DEFAULT 0,
			sent_at        INTEGER NOT NULL,
			edited_at      INTEGER,
			reply_to       TEXT,
			expires_at     INTEGER,
			FOREIGN KEY (chat_id) REFERENCES chats(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_chat_time ON messages(chat_id, sent_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_status    ON messages(status)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_expires   ON messages(expires_at) WHERE expires_at IS NOT NULL`,

		// FTS5 for local full-text search on decrypted plaintext (device-only).
		`CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
			id UNINDEXED,
			chat_id UNINDEXED,
			body,
			tokenize = 'unicode61'
		)`,

		`CREATE TABLE IF NOT EXISTS reactions (
			msg_id     TEXT NOT NULL,
			node_id    TEXT NOT NULL,
			fingerprint TEXT NOT NULL,
			emoji      TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (msg_id, node_id, fingerprint, emoji)
		)`,

		`CREATE TABLE IF NOT EXISTS read_receipts (
			msg_id      TEXT NOT NULL,
			node_id     TEXT NOT NULL,
			fingerprint TEXT NOT NULL,
			read_at     INTEGER NOT NULL,
			PRIMARY KEY (msg_id, node_id, fingerprint)
		)`,

		`CREATE TABLE IF NOT EXISTS contact_requests (
			id           TEXT PRIMARY KEY,
			node_id      TEXT NOT NULL,
			fingerprint  TEXT NOT NULL,
			display_name TEXT NOT NULL,
			sign_key     BLOB NOT NULL,
			dh_key       BLOB NOT NULL,
			message      TEXT,
			status       INTEGER NOT NULL DEFAULT 0,
			received_at  INTEGER NOT NULL
		)`,

		`CREATE TABLE IF NOT EXISTS sticker_packs (
			id           TEXT PRIMARY KEY,
			name         TEXT NOT NULL,
			author       TEXT,
			stickers     TEXT NOT NULL,
			installed_at INTEGER NOT NULL
		)`,

		`CREATE TABLE IF NOT EXISTS media_meta (
			hash       TEXT PRIMARY KEY,
			mime_type  TEXT,
			file_name  TEXT,
			size       INTEGER,
			last_used  INTEGER NOT NULL
		)`,

		`CREATE TABLE IF NOT EXISTS pending_queue (
			id         TEXT PRIMARY KEY,
			chat_id    TEXT NOT NULL,
			payload    BLOB NOT NULL,
			attempt    INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			next_try   INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_pending_next_try ON pending_queue(next_try)`,
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migrate: %w (stmt: %.60s)", err, stmt)
		}
	}
	return tx.Commit()
}

// ─── Messages ────────────────────────────────────────────────────────────────

func (s *SQLiteStorage) SaveMessage(msg *core.Message) error {
	meta, err := json.Marshal(msg.Metadata)
	if err != nil {
		return err
	}
	var recipNode, recipFP sql.NullString
	if msg.RecipientID != nil {
		recipNode = sql.NullString{String: string(msg.RecipientID.NodeID), Valid: true}
		recipFP   = sql.NullString{String: msg.RecipientID.Fingerprint, Valid: true}
	}
	var editedAt, expiresAt sql.NullInt64
	if msg.EditedAt != nil {
		editedAt = sql.NullInt64{Int64: msg.EditedAt.UnixNano(), Valid: true}
	}
	if msg.ExpiresAt != nil {
		expiresAt = sql.NullInt64{Int64: msg.ExpiresAt.UnixNano(), Valid: true}
	}
	// ── FIX: replyTo is TEXT (MessageID), not an integer ──
	var replyTo sql.NullString
	if msg.ReplyTo != nil {
		replyTo = sql.NullString{String: string(*msg.ReplyTo), Valid: true}
	}

	_, err = s.db.Exec(`
		INSERT OR REPLACE INTO messages
			(id, chat_id, sender_node, sender_fp, recipient_node, recipient_fp,
			 type, payload, meta, status, sent_at, edited_at, reply_to, expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		string(msg.ID), string(msg.ChatID),
		string(msg.SenderID.NodeID), msg.SenderID.Fingerprint,
		recipNode, recipFP,
		uint8(msg.Type), msg.Payload, string(meta),
		uint8(msg.Status), msg.SentAt.UnixNano(),
		editedAt, replyTo, expiresAt,
	)
	return err
}

func (s *SQLiteStorage) GetMessage(id core.MessageID) (*core.Message, error) {
	row := s.db.QueryRow(`
		SELECT id, chat_id, sender_node, sender_fp, recipient_node, recipient_fp,
		       type, payload, meta, status, sent_at, edited_at, reply_to, expires_at
		FROM messages WHERE id = ?`, string(id))
	return scanMessage(row)
}

func (s *SQLiteStorage) GetMessages(chatID core.ChatID, limit, offset int) ([]*core.Message, error) {
	rows, err := s.db.Query(`
		SELECT id, chat_id, sender_node, sender_fp, recipient_node, recipient_fp,
		       type, payload, meta, status, sent_at, edited_at, reply_to, expires_at
		FROM messages
		WHERE chat_id = ? AND (expires_at IS NULL OR expires_at > ?)
		ORDER BY sent_at DESC LIMIT ? OFFSET ?`,
		string(chatID), time.Now().UnixNano(), limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (s *SQLiteStorage) UpdateMessageStatus(id core.MessageID, status core.DeliveryStatus) error {
	_, err := s.db.Exec(`UPDATE messages SET status = ? WHERE id = ?`, uint8(status), string(id))
	return err
}

func (s *SQLiteStorage) DeleteMessage(id core.MessageID) error {
	s.db.Exec(`DELETE FROM messages_fts WHERE id = ?`, string(id))
	_, err := s.db.Exec(`DELETE FROM messages WHERE id = ?`, string(id))
	return err
}

// DeleteExpiredMessages removes messages whose expires_at is in the past.
// Returns the number of deleted rows.
func (s *SQLiteStorage) DeleteExpiredMessages() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM messages WHERE expires_at IS NOT NULL AND expires_at <= ?`,
		time.Now().UnixNano())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *SQLiteStorage) SearchMessages(chatID core.ChatID, query string) ([]*core.Message, error) {
	rows, err := s.db.Query(`
		SELECT m.id, m.chat_id, m.sender_node, m.sender_fp,
		       m.recipient_node, m.recipient_fp, m.type, m.payload, m.meta,
		       m.status, m.sent_at, m.edited_at, m.reply_to, m.expires_at
		FROM messages m
		JOIN messages_fts f ON m.id = f.id
		WHERE f.chat_id = ? AND messages_fts MATCH ?
		ORDER BY m.sent_at DESC LIMIT 200`,
		string(chatID), query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// IndexMessageText inserts/replaces a plaintext entry in the FTS table.
// Called by the engine after decryption so only local plaintext is indexed.
func (s *SQLiteStorage) IndexMessageText(id core.MessageID, plaintext string) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO messages_fts(id, chat_id, body)
		 SELECT id, chat_id, ? FROM messages WHERE id = ?`,
		plaintext, string(id))
	return err
}

// ─── Reactions ────────────────────────────────────────────────────────────────

func (s *SQLiteStorage) SaveReaction(msgID core.MessageID, r core.ReactionEntry) error {
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO reactions (msg_id, node_id, fingerprint, emoji, created_at)
		VALUES (?,?,?,?,?)`,
		string(msgID), string(r.PeerID.NodeID), r.PeerID.Fingerprint,
		r.Emoji, r.CreatedAt.UnixNano())
	return err
}

func (s *SQLiteStorage) DeleteReaction(msgID core.MessageID, peerID core.PeerID, emoji string) error {
	_, err := s.db.Exec(`
		DELETE FROM reactions WHERE msg_id = ? AND node_id = ? AND fingerprint = ? AND emoji = ?`,
		string(msgID), string(peerID.NodeID), peerID.Fingerprint, emoji)
	return err
}

func (s *SQLiteStorage) GetReactions(msgID core.MessageID) (map[string][]core.PeerID, error) {
	rows, err := s.db.Query(`
		SELECT emoji, node_id, fingerprint FROM reactions WHERE msg_id = ? ORDER BY emoji, created_at`,
		string(msgID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string][]core.PeerID)
	for rows.Next() {
		var emoji, nodeID, fp string
		if err := rows.Scan(&emoji, &nodeID, &fp); err != nil {
			return nil, err
		}
		result[emoji] = append(result[emoji], core.PeerID{NodeID: core.NodeID(nodeID), Fingerprint: fp})
	}
	return result, rows.Err()
}

// ─── Read receipts ────────────────────────────────────────────────────────────

func (s *SQLiteStorage) SaveReadReceipt(msgID core.MessageID, peerID core.PeerID) error {
	_, err := s.db.Exec(`
		INSERT OR IGNORE INTO read_receipts (msg_id, node_id, fingerprint, read_at)
		VALUES (?,?,?,?)`,
		string(msgID), string(peerID.NodeID), peerID.Fingerprint, time.Now().UnixNano())
	return err
}

func (s *SQLiteStorage) GetReaders(msgID core.MessageID) ([]core.PeerID, error) {
	rows, err := s.db.Query(`
		SELECT node_id, fingerprint FROM read_receipts WHERE msg_id = ? ORDER BY read_at`,
		string(msgID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var peers []core.PeerID
	for rows.Next() {
		var node, fp string
		if err := rows.Scan(&node, &fp); err != nil {
			return nil, err
		}
		peers = append(peers, core.PeerID{NodeID: core.NodeID(node), Fingerprint: fp})
	}
	return peers, rows.Err()
}

// ─── Chats ────────────────────────────────────────────────────────────────────

func (s *SQLiteStorage) SaveChat(chat *core.Chat) error {
	members, _ := json.Marshal(chat.Members)
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO chats
			(id, type, name, description, avatar_hash, members, network_id,
			 encrypted, disappear_after_ns, default_permissions, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		string(chat.ID), uint8(chat.Type), chat.Name, chat.Description,
		chat.AvatarHash, string(members), string(chat.NetworkID),
		boolToInt(chat.Encrypted),
		int64(chat.DisappearAfter),
		uint32(chat.DefaultPermissions),
		chat.CreatedAt.UnixNano(), chat.UpdatedAt.UnixNano(),
	)
	return err
}

func (s *SQLiteStorage) GetChat(id core.ChatID) (*core.Chat, error) {
	row := s.db.QueryRow(`
		SELECT id, type, name, description, avatar_hash, members, network_id,
		       encrypted, disappear_after_ns, default_permissions, created_at, updated_at
		FROM chats WHERE id = ?`, string(id))
	return scanChat(row)
}

func (s *SQLiteStorage) ListChats() ([]*core.Chat, error) {
	rows, err := s.db.Query(`
		SELECT id, type, name, description, avatar_hash, members, network_id,
		       encrypted, disappear_after_ns, default_permissions, created_at, updated_at
		FROM chats ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chats []*core.Chat
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, err
		}
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

func (s *SQLiteStorage) DeleteChat(id core.ChatID) error {
	_, err := s.db.Exec(`DELETE FROM chats WHERE id = ?`, string(id))
	return err
}

// ─── Peers ────────────────────────────────────────────────────────────────────

func (s *SQLiteStorage) SavePeer(peer *core.Peer) error {
	networks, _ := json.Marshal(peer.Networks)
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO peers
			(node_id, fingerprint, display_name, avatar_hash, status, status_text,
			 public_key, sign_key, last_seen, networks, is_blocked, is_contact)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		string(peer.ID.NodeID), peer.ID.Fingerprint,
		peer.DisplayName, peer.AvatarHash,
		uint8(peer.Status), peer.StatusText,
		peer.PublicKey, peer.SignKey,
		peer.LastSeen.UnixNano(), string(networks),
		boolToInt(peer.IsBlocked), boolToInt(peer.IsContact),
	)
	return err
}

func (s *SQLiteStorage) GetPeer(id core.PeerID) (*core.Peer, error) {
	row := s.db.QueryRow(`
		SELECT node_id, fingerprint, display_name, avatar_hash, status, status_text,
		       public_key, sign_key, last_seen, networks, is_blocked, is_contact
		FROM peers WHERE node_id = ? AND fingerprint = ?`,
		string(id.NodeID), id.Fingerprint)
	return scanPeer(row)
}

func (s *SQLiteStorage) ListPeers() ([]*core.Peer, error) {
	rows, err := s.db.Query(`
		SELECT node_id, fingerprint, display_name, avatar_hash, status, status_text,
		       public_key, sign_key, last_seen, networks, is_blocked, is_contact
		FROM peers ORDER BY display_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var peers []*core.Peer
	for rows.Next() {
		p, err := scanPeer(rows)
		if err != nil {
			return nil, err
		}
		peers = append(peers, p)
	}
	return peers, rows.Err()
}

func (s *SQLiteStorage) DeletePeer(id core.PeerID) error {
	_, err := s.db.Exec(`DELETE FROM peers WHERE node_id = ? AND fingerprint = ?`,
		string(id.NodeID), id.Fingerprint)
	return err
}

// ─── Contact requests ─────────────────────────────────────────────────────────

func (s *SQLiteStorage) SaveContactRequest(req *core.ContactRequest) error {
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO contact_requests
			(id, node_id, fingerprint, display_name, sign_key, dh_key, message, status, received_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		req.ID, string(req.FromPeer.NodeID), req.FromPeer.Fingerprint,
		req.DisplayName, req.SignKey, req.DHKey,
		req.Message, uint8(req.Status), req.ReceivedAt.UnixNano())
	return err
}

func (s *SQLiteStorage) GetContactRequest(id string) (*core.ContactRequest, error) {
	row := s.db.QueryRow(`
		SELECT id, node_id, fingerprint, display_name, sign_key, dh_key, message, status, received_at
		FROM contact_requests WHERE id = ?`, id)
	return scanContactRequest(row)
}

func (s *SQLiteStorage) ListContactRequests(status core.ContactRequestStatus) ([]*core.ContactRequest, error) {
	rows, err := s.db.Query(`
		SELECT id, node_id, fingerprint, display_name, sign_key, dh_key, message, status, received_at
		FROM contact_requests WHERE status = ? ORDER BY received_at DESC`, uint8(status))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reqs []*core.ContactRequest
	for rows.Next() {
		r, err := scanContactRequest(rows)
		if err != nil {
			return nil, err
		}
		reqs = append(reqs, r)
	}
	return reqs, rows.Err()
}

// ─── Sticker packs ────────────────────────────────────────────────────────────

func (s *SQLiteStorage) SaveStickerPack(pack *core.StickerPack) error {
	stickers, _ := json.Marshal(pack.Stickers)
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO sticker_packs (id, name, author, stickers, installed_at)
		VALUES (?,?,?,?,?)`,
		pack.ID, pack.Name, pack.Author, string(stickers), pack.InstalledAt.UnixNano())
	return err
}

func (s *SQLiteStorage) GetStickerPack(id string) (*core.StickerPack, error) {
	row := s.db.QueryRow(`SELECT id, name, author, stickers, installed_at FROM sticker_packs WHERE id = ?`, id)
	var packID, name, author, stickersJSON string
	var installedAt int64
	err := row.Scan(&packID, &name, &author, &stickersJSON, &installedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var stickers []core.Sticker
	json.Unmarshal([]byte(stickersJSON), &stickers)
	return &core.StickerPack{
		ID: packID, Name: name, Author: author,
		Stickers:    stickers,
		InstalledAt: time.Unix(0, installedAt),
	}, nil
}

func (s *SQLiteStorage) ListStickerPacks() ([]*core.StickerPack, error) {
	rows, err := s.db.Query(`SELECT id, name, author, stickers, installed_at FROM sticker_packs ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var packs []*core.StickerPack
	for rows.Next() {
		var id, name, author, stickersJSON string
		var installedAt int64
		if err := rows.Scan(&id, &name, &author, &stickersJSON, &installedAt); err != nil {
			return nil, err
		}
		var stickers []core.Sticker
		json.Unmarshal([]byte(stickersJSON), &stickers)
		packs = append(packs, &core.StickerPack{
			ID: id, Name: name, Author: author,
			Stickers:    stickers,
			InstalledAt: time.Unix(0, installedAt),
		})
	}
	return packs, rows.Err()
}

func (s *SQLiteStorage) DeleteStickerPack(id string) error {
	_, err := s.db.Exec(`DELETE FROM sticker_packs WHERE id = ?`, id)
	return err
}

// ─── Media ────────────────────────────────────────────────────────────────────

func (s *SQLiteStorage) SaveMedia(hash string, data []byte) error {
	path := s.mediaPath(hash)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO media_meta (hash, size, last_used) VALUES (?,?,?)`,
		hash, int64(len(data)), time.Now().UnixNano())
	return err
}

func (s *SQLiteStorage) GetMedia(hash string) ([]byte, error) {
	// Update last_used for LRU tracking
	s.db.Exec(`UPDATE media_meta SET last_used = ? WHERE hash = ?`, time.Now().UnixNano(), hash)
	return os.ReadFile(s.mediaPath(hash))
}

func (s *SQLiteStorage) DeleteMedia(hash string) error {
	os.Remove(s.mediaPath(hash))
	_, err := s.db.Exec(`DELETE FROM media_meta WHERE hash = ?`, hash)
	return err
}

// PruneMediaCache removes least-recently-used media until total size ≤ maxBytes.
// Returns bytes freed.
func (s *SQLiteStorage) PruneMediaCache(maxBytes int64) (int64, error) {
	// Get total size
	var total sql.NullInt64
	s.db.QueryRow(`SELECT SUM(size) FROM media_meta`).Scan(&total)
	if !total.Valid || total.Int64 <= maxBytes {
		return 0, nil
	}

	// Fetch LRU order
	rows, err := s.db.Query(`SELECT hash, size FROM media_meta ORDER BY last_used ASC`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var freed int64
	current := total.Int64
	for rows.Next() && current > maxBytes {
		var hash string
		var size int64
		if err := rows.Scan(&hash, &size); err != nil {
			continue
		}
		if err := s.DeleteMedia(hash); err == nil {
			freed += size
			current -= size
		}
	}
	return freed, nil
}

func (s *SQLiteStorage) mediaPath(hash string) string {
	if len(hash) < 4 {
		return filepath.Join(s.mediaDir, hash)
	}
	return filepath.Join(s.mediaDir, hash[:2], hash[2:4], hash)
}

// ─── Pending queue ────────────────────────────────────────────────────────────

func (s *SQLiteStorage) EnqueuePending(msg *core.Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT OR IGNORE INTO pending_queue (id, chat_id, payload, attempt, created_at, next_try)
		VALUES (?,?,?,0,?,?)`,
		string(msg.ID), string(msg.ChatID), data,
		time.Now().UnixNano(), time.Now().UnixNano())
	return err
}

func (s *SQLiteStorage) DequeuePending(limit int) ([]*core.Message, error) {
	rows, err := s.db.Query(`
		SELECT payload FROM pending_queue
		WHERE next_try <= ?
		ORDER BY created_at ASC LIMIT ?`,
		time.Now().UnixNano(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []*core.Message
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var msg core.Message
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		msgs = append(msgs, &msg)
	}
	return msgs, rows.Err()
}

func (s *SQLiteStorage) RemovePending(id core.MessageID) error {
	_, err := s.db.Exec(`DELETE FROM pending_queue WHERE id = ?`, string(id))
	return err
}

func (s *SQLiteStorage) Close() error { return s.db.Close() }

// ─── Scan helpers ─────────────────────────────────────────────────────────────

type scanner interface {
	Scan(dest ...interface{}) error
}

func scanMessage(row scanner) (*core.Message, error) {
	var (
		id, chatID, senderNode, senderFP   string
		recipNode, recipFP                 sql.NullString
		msgType, status                    uint8
		payload                            []byte
		metaJSON                           string
		sentAt                             int64
		editedAt, expiresAt                sql.NullInt64
		replyTo                            sql.NullString // ── FIXED: was sql.NullInt64
	)
	err := row.Scan(&id, &chatID, &senderNode, &senderFP,
		&recipNode, &recipFP, &msgType, &payload, &metaJSON,
		&status, &sentAt, &editedAt, &replyTo, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var meta core.MessageMeta
	json.Unmarshal([]byte(metaJSON), &meta)

	msg := &core.Message{
		ID:       core.MessageID(id),
		ChatID:   core.ChatID(chatID),
		SenderID: core.PeerID{NodeID: core.NodeID(senderNode), Fingerprint: senderFP},
		Type:     core.MessageType(msgType),
		Payload:  payload,
		Metadata: meta,
		Status:   core.DeliveryStatus(status),
		SentAt:   time.Unix(0, sentAt),
	}
	if recipNode.Valid {
		rid := core.PeerID{NodeID: core.NodeID(recipNode.String), Fingerprint: recipFP.String}
		msg.RecipientID = &rid
	}
	if editedAt.Valid {
		t := time.Unix(0, editedAt.Int64)
		msg.EditedAt = &t
	}
	if replyTo.Valid {
		rid := core.MessageID(replyTo.String)
		msg.ReplyTo = &rid
	}
	if expiresAt.Valid {
		t := time.Unix(0, expiresAt.Int64)
		msg.ExpiresAt = &t
	}
	return msg, nil
}

func scanMessages(rows *sql.Rows) ([]*core.Message, error) {
	var msgs []*core.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func scanChat(row scanner) (*core.Chat, error) {
	var (
		id, name, desc, avatarHash, networkID string
		chatType, encrypted                   int
		membersJSON                           sql.NullString
		disappearAfterNS                      int64
		defaultPerms                          uint32
		createdAt, updatedAt                  int64
	)
	err := row.Scan(&id, &chatType, &name, &desc, &avatarHash,
		&membersJSON, &networkID, &encrypted,
		&disappearAfterNS, &defaultPerms,
		&createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var members []core.ChatMember
	if membersJSON.Valid {
		json.Unmarshal([]byte(membersJSON.String), &members)
	}
	return &core.Chat{
		ID:                 core.ChatID(id),
		Type:               core.ChatType(chatType),
		Name:               name,
		Description:        desc,
		AvatarHash:         avatarHash,
		Members:            members,
		NetworkID:          core.NetworkID(networkID),
		Encrypted:          encrypted == 1,
		DisappearAfter:     time.Duration(disappearAfterNS),
		DefaultPermissions: core.Permission(defaultPerms),
		CreatedAt:          time.Unix(0, createdAt),
		UpdatedAt:          time.Unix(0, updatedAt),
	}, nil
}

func scanPeer(row scanner) (*core.Peer, error) {
	var (
		nodeID, fp, name, avatarHash, statusText string
		status, isBlocked, isContact             uint8
		pubKey, signKey                          []byte
		lastSeen                                 int64
		networksJSON                             sql.NullString
	)
	err := row.Scan(&nodeID, &fp, &name, &avatarHash, &status, &statusText,
		&pubKey, &signKey, &lastSeen, &networksJSON, &isBlocked, &isContact)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var networks []core.NetworkID
	if networksJSON.Valid {
		json.Unmarshal([]byte(networksJSON.String), &networks)
	}
	return &core.Peer{
		ID:          core.PeerID{NodeID: core.NodeID(nodeID), Fingerprint: fp},
		DisplayName: name,
		AvatarHash:  avatarHash,
		Status:      core.PeerStatus(status),
		StatusText:  statusText,
		PublicKey:   pubKey,
		SignKey:     signKey,
		LastSeen:    time.Unix(0, lastSeen),
		Networks:    networks,
		IsBlocked:   isBlocked == 1,
		IsContact:   isContact == 1,
	}, nil
}

func scanContactRequest(row scanner) (*core.ContactRequest, error) {
	var (
		id, nodeID, fp, displayName, message string
		signKey, dhKey                       []byte
		status                               uint8
		receivedAt                           int64
	)
	err := row.Scan(&id, &nodeID, &fp, &displayName, &signKey, &dhKey, &message, &status, &receivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &core.ContactRequest{
		ID:          id,
		FromPeer:    core.PeerID{NodeID: core.NodeID(nodeID), Fingerprint: fp},
		DisplayName: displayName,
		SignKey:     signKey,
		DHKey:       dhKey,
		Message:     message,
		Status:      core.ContactRequestStatus(status),
		ReceivedAt:  time.Unix(0, receivedAt),
	}, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

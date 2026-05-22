// Package core defines all fundamental types, interfaces and the event system.
// Every subsystem depends only on this package – it has zero internal imports.
package core

import (
	"context"
	"encoding/json"
	"io"
	"time"
)

// ─── Identity ────────────────────────────────────────────────────────────────

type NodeID    string
type NetworkID string

type PeerID struct {
	NodeID      NodeID `json:"node_id"`
	Fingerprint string `json:"fingerprint"`
}

func (p PeerID) String() string { return string(p.NodeID) + ":" + p.Fingerprint }
func (p PeerID) IsZero() bool   { return p.NodeID == "" && p.Fingerprint == "" }

// ─── Messages ────────────────────────────────────────────────────────────────

type MessageType uint8

const (
	MsgText        MessageType = 0x01
	MsgImage       MessageType = 0x02
	MsgAudio       MessageType = 0x03
	MsgVideo       MessageType = 0x04
	MsgFile        MessageType = 0x05
	MsgSticker     MessageType = 0x06
	MsgLocation    MessageType = 0x07
	MsgReaction    MessageType = 0x08
	MsgEdit        MessageType = 0x09
	MsgDelete      MessageType = 0x0A
	MsgRead        MessageType = 0x0B
	MsgDelivered   MessageType = 0x0C
	MsgTyping      MessageType = 0x0D
	MsgForward     MessageType = 0x0E
	// Group system messages
	MsgMemberAdded   MessageType = 0x11
	MsgMemberRemoved MessageType = 0x12
	MsgMemberLeft    MessageType = 0x13
	MsgGroupRenamed  MessageType = 0x14
	MsgAdminChanged  MessageType = 0x15
	// Call signalling
	MsgCallSignal  MessageType = 0x20
	MsgCallEnd     MessageType = 0x21
	// Contact requests
	MsgContactRequest MessageType = 0x30
	MsgContactAccept  MessageType = 0x31
	MsgContactDecline MessageType = 0x32
	// Key management
	MsgKeyRotation    MessageType = 0x40
	MsgSenderKeyDist  MessageType = 0x41
	// Sync
	MsgSync    MessageType = 0x50
	MsgSyncAck MessageType = 0x51
	// Control
	MsgPing         MessageType = 0xF0
	MsgPong         MessageType = 0xF1
	MsgHandshake    MessageType = 0xF2
	MsgHandshakeAck MessageType = 0xF3
)

type DeliveryStatus uint8

const (
	StatusPending   DeliveryStatus = iota
	StatusSent
	StatusDelivered
	StatusRead
	StatusFailed
)

type ChatID    string
type MessageID string

// Mention is an @-mention embedded in a message.
type Mention struct {
	PeerID      PeerID `json:"peer_id"`
	DisplayName string `json:"display_name"`
	Offset      int    `json:"offset"` // byte offset in text where mention starts
	Length      int    `json:"length"` // byte length of the mention token
}

// ReactionEntry holds one peer's reaction on a message.
type ReactionEntry struct {
	PeerID    PeerID    `json:"peer_id"`
	Emoji     string    `json:"emoji"`
	CreatedAt time.Time `json:"created_at"`
}

// ForwardInfo carries origin metadata when a message is forwarded.
type ForwardInfo struct {
	OriginalSenderID   PeerID    `json:"original_sender_id"`
	OriginalSenderName string    `json:"original_sender_name"`
	OriginalChatID     ChatID    `json:"original_chat_id"`
	OriginalMsgID      MessageID `json:"original_msg_id"`
	OriginalSentAt     time.Time `json:"original_sent_at"`
}

// MessageMeta carries non-encrypted auxiliary metadata.
type MessageMeta struct {
	MimeType    string            `json:"mime,omitempty"`
	FileName    string            `json:"file_name,omitempty"`
	FileSize    int64             `json:"file_size,omitempty"`
	Duration    time.Duration     `json:"duration,omitempty"`
	Width       int               `json:"width,omitempty"`
	Height      int               `json:"height,omitempty"`
	Thumbnail   []byte            `json:"thumb,omitempty"`
	MediaHash   string            `json:"media_hash,omitempty"`
	// Sticker pack reference
	StickerPackID string `json:"sticker_pack_id,omitempty"`
	StickerID     string `json:"sticker_id,omitempty"`
	// Mentions parsed from text
	Mentions    []Mention         `json:"mentions,omitempty"`
	// Forward chain
	ForwardInfo *ForwardInfo      `json:"forward_info,omitempty"`
	Extra       map[string]string `json:"extra,omitempty"`
}

// Message is the canonical in-memory representation of a chat message.
type Message struct {
	ID          MessageID      `json:"id"`
	ChatID      ChatID         `json:"chat_id"`
	SenderID    PeerID         `json:"sender_id"`
	RecipientID *PeerID        `json:"recipient_id,omitempty"`
	Type        MessageType    `json:"type"`
	Payload     []byte         `json:"payload"`
	Metadata    MessageMeta    `json:"meta"`
	Status      DeliveryStatus `json:"status"`
	SentAt      time.Time      `json:"sent_at"`
	EditedAt    *time.Time     `json:"edited_at,omitempty"`
	ReplyTo     *MessageID     `json:"reply_to,omitempty"`
	ExpiresAt   *time.Time     `json:"expires_at,omitempty"`
}

// ─── Groups / Chats ───────────────────────────────────────────────────────────

type ChatType uint8

const (
	ChatDirect  ChatType = iota
	ChatGroup
	ChatChannel
)

// Permission bit flags for chat members.
type Permission uint32

const (
	PermSendMessages   Permission = 1 << 0
	PermSendMedia      Permission = 1 << 1
	PermAddMembers     Permission = 1 << 2
	PermRemoveMembers  Permission = 1 << 3
	PermChangeInfo     Permission = 1 << 4
	PermPinMessages    Permission = 1 << 5
	PermManageAdmins   Permission = 1 << 6
	PermAll            Permission = 0xFFFFFFFF
	PermDefault        Permission = PermSendMessages | PermSendMedia
)

// MemberRole in a group chat.
type MemberRole uint8

const (
	RoleMember MemberRole = iota
	RoleAdmin
	RoleOwner
)

// ChatMember describes one member of a group chat.
type ChatMember struct {
	PeerID      PeerID     `json:"peer_id"`
	Role        MemberRole `json:"role"`
	Permissions Permission `json:"permissions"`
	JoinedAt    time.Time  `json:"joined_at"`
	InvitedBy   *PeerID    `json:"invited_by,omitempty"`
}

// Chat holds the state for a conversation.
type Chat struct {
	ID          ChatID       `json:"id"`
	Type        ChatType     `json:"type"`
	Name        string       `json:"name,omitempty"`
	Description string       `json:"description,omitempty"`
	AvatarHash  string       `json:"avatar_hash,omitempty"`
	Members     []ChatMember `json:"members"`
	NetworkID   NetworkID    `json:"network_id"`
	Encrypted   bool         `json:"encrypted"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	// Disappearing messages default TTL (0 = disabled)
	DisappearAfter time.Duration `json:"disappear_after,omitempty"`
	// Default permissions for non-admin members
	DefaultPermissions Permission `json:"default_permissions"`
}

// MemberPeerIDs is a convenience helper.
func (c *Chat) MemberPeerIDs() []PeerID {
	out := make([]PeerID, len(c.Members))
	for i, m := range c.Members {
		out[i] = m.PeerID
	}
	return out
}

// FindMember returns the ChatMember for a given PeerID or nil.
func (c *Chat) FindMember(id PeerID) *ChatMember {
	for i := range c.Members {
		if c.Members[i].PeerID.String() == id.String() {
			return &c.Members[i]
		}
	}
	return nil
}

// ─── Peers ────────────────────────────────────────────────────────────────────

type PeerStatus uint8

const (
	PeerOffline   PeerStatus = iota
	PeerOnline
	PeerAway
	PeerBusy
	PeerInvisible
)

// Peer holds contact information for a known user.
type Peer struct {
	ID          PeerID     `json:"id"`
	DisplayName string     `json:"display_name"`
	AvatarHash  string     `json:"avatar_hash,omitempty"`
	Status      PeerStatus `json:"status"`
	StatusText  string     `json:"status_text,omitempty"`
	PublicKey   []byte     `json:"public_key"` // X25519 DH key
	SignKey     []byte     `json:"sign_key"`   // Ed25519 identity key
	LastSeen    time.Time  `json:"last_seen"`
	Networks    []NetworkID `json:"networks"`
	IsBlocked   bool       `json:"is_blocked,omitempty"`
	IsContact   bool       `json:"is_contact"` // accepted contact
}

// ─── Contact Requests ─────────────────────────────────────────────────────────

// ContactRequestStatus tracks the state of a contact request.
type ContactRequestStatus uint8

const (
	ContactRequestPending  ContactRequestStatus = iota
	ContactRequestAccepted
	ContactRequestDeclined
)

// ContactRequest is sent when a stranger initiates contact.
type ContactRequest struct {
	ID          string               `json:"id"`
	FromPeer    PeerID               `json:"from_peer"`
	DisplayName string               `json:"display_name"`
	SignKey     []byte               `json:"sign_key"`
	DHKey       []byte               `json:"dh_key"`
	Message     string               `json:"message,omitempty"` // optional greeting
	Status      ContactRequestStatus `json:"status"`
	ReceivedAt  time.Time            `json:"received_at"`
}

// ─── Sticker Packs ────────────────────────────────────────────────────────────

// StickerPack groups stickers under a named pack.
type StickerPack struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Author    string    `json:"author,omitempty"`
	Stickers  []Sticker `json:"stickers"`
	InstalledAt time.Time `json:"installed_at"`
}

// Sticker is a single sticker within a pack.
type Sticker struct {
	ID        string `json:"id"`
	PackID    string `json:"pack_id"`
	MediaHash string `json:"media_hash"` // points to local/remote media store
	Emoji     string `json:"emoji,omitempty"`
}

// ─── Calls ────────────────────────────────────────────────────────────────────

type CallType  uint8
type CallState uint8

const (
	CallVoice       CallType = iota
	CallVideo
	CallScreenShare
)

const (
	CallIdle       CallState = iota
	CallRinging
	CallConnecting
	CallActive
	CallHeld
	CallEnded
	CallDeclined
	CallMissed
	CallFailed
)

// CallQuality carries RTCP-derived quality metrics published periodically.
type CallQuality struct {
	SessionID   string        `json:"session_id"`
	RTT         time.Duration `json:"rtt"`          // round-trip time
	PacketLoss  float64       `json:"packet_loss"`  // 0.0–1.0
	Jitter      time.Duration `json:"jitter"`
	AudioBitrate int          `json:"audio_bitrate_bps"`
	VideoBitrate int          `json:"video_bitrate_bps"`
	Timestamp   time.Time     `json:"timestamp"`
}

// CallSession represents an ongoing or historical call.
type CallSession struct {
	ID           string     `json:"id"`
	ChatID       ChatID     `json:"chat_id"`
	Type         CallType   `json:"type"`
	State        CallState  `json:"state"`
	Initiator    PeerID     `json:"initiator"`
	Participants []PeerID   `json:"participants"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	Duration     time.Duration `json:"duration,omitempty"`
}

// ─── Packet wire format ───────────────────────────────────────────────────────

type PacketFlags uint8

const (
	FlagCompressed PacketFlags = 1 << 0
	FlagEncrypted  PacketFlags = 1 << 1
	FlagReliable   PacketFlags = 1 << 2
	FlagFragment   PacketFlags = 1 << 3
	FlagMedia      PacketFlags = 1 << 4
)

const PacketMagic   uint32 = 0x4D534752 // "MSGR"
const PacketVersion uint8  = 1

type Packet struct {
	Magic      uint32
	Version    uint8
	Type       MessageType
	Flags      PacketFlags
	TTL        uint8
	PacketID   [16]byte
	SenderNode [5]byte
	Timestamp  int64
	Body       []byte
	HMAC       [32]byte
}

// ─── Transfer progress ────────────────────────────────────────────────────────

// TransferDirection indicates upload or download.
type TransferDirection uint8

const (
	TransferUpload   TransferDirection = iota
	TransferDownload
)

// TransferState is the lifecycle of a file transfer.
type TransferState uint8

const (
	TransferPending   TransferState = iota
	TransferActive
	TransferPaused
	TransferCompleted
	TransferFailed
	TransferCancelled
)

// TransferProgress is published on the EventBus during media transfers.
type TransferProgress struct {
	TransferID  string            `json:"transfer_id"`
	MessageID   MessageID         `json:"message_id"`
	ChatID      ChatID            `json:"chat_id"`
	Direction   TransferDirection `json:"direction"`
	State       TransferState     `json:"state"`
	BytesTotal  int64             `json:"bytes_total"`
	BytesDone   int64             `json:"bytes_done"`
	Percent     float64           `json:"percent"`
	SpeedBps    int64             `json:"speed_bps"`
	Error       string            `json:"error,omitempty"`
}

// ─── Network diagnostics ─────────────────────────────────────────────────────

// NetworkStats holds runtime diagnostic counters.
type NetworkStats struct {
	MessagesSent      uint64 `json:"messages_sent"`
	MessagesReceived  uint64 `json:"messages_received"`
	BytesSent         uint64 `json:"bytes_sent"`
	BytesReceived     uint64 `json:"bytes_received"`
	EncryptErrors     uint64 `json:"encrypt_errors"`
	DecryptErrors     uint64 `json:"decrypt_errors"`
	RetransmitCount   uint64 `json:"retransmit_count"`
	ActiveSessions    int    `json:"active_sessions"`
	ActiveCalls       int    `json:"active_calls"`
	ConnectedPeers    int    `json:"connected_peers"`
	ServerConnected   bool   `json:"server_connected"`
}

// ─── Core Interfaces ──────────────────────────────────────────────────────────

// Transport abstracts the ZeroTier (or any other) network layer.
type Transport interface {
	Send(ctx context.Context, to NodeID, pkt *Packet) error
	Broadcast(ctx context.Context, network NetworkID, pkt *Packet) error
	Recv() <-chan *IncomingPacket
	JoinNetwork(ctx context.Context, id NetworkID) error
	LeaveNetwork(ctx context.Context, id NetworkID) error
	NodeID() NodeID
	Networks() []NetworkID
	PeerReachable(id NodeID) bool
	Start(ctx context.Context) error
	Stop() error
}

type IncomingPacket struct {
	From       NodeID
	Pkt        *Packet
	ReceivedAt time.Time
}

// Crypto provides all cryptographic operations.
type Crypto interface {
	GenerateIdentity() (interface{}, error)
	LoadIdentity(data []byte) (interface{}, error)
	DeriveSessionKey(local interface{}, remotePub []byte) ([]byte, error)
	Encrypt(key, plaintext, aad []byte) ([]byte, error)
	Decrypt(key, ciphertext, aad []byte) ([]byte, error)
	Sign(identity interface{}, msg []byte) ([]byte, error)
	Verify(publicKey, msg, sig []byte) bool
	HMAC(key, data []byte) []byte
	DeriveKey(secret, salt, info []byte, length int) ([]byte, error)
}

// Storage handles all persistence.
type Storage interface {
	// Messages
	SaveMessage(msg *Message) error
	GetMessage(id MessageID) (*Message, error)
	GetMessages(chatID ChatID, limit, offset int) ([]*Message, error)
	UpdateMessageStatus(id MessageID, status DeliveryStatus) error
	DeleteMessage(id MessageID) error
	DeleteExpiredMessages() (int64, error)
	SearchMessages(chatID ChatID, query string) ([]*Message, error)
	IndexMessageText(id MessageID, plaintext string) error

	// Reactions
	SaveReaction(msgID MessageID, r ReactionEntry) error
	DeleteReaction(msgID MessageID, peerID PeerID, emoji string) error
	GetReactions(msgID MessageID) (map[string][]PeerID, error)

	// Read receipts
	SaveReadReceipt(msgID MessageID, peerID PeerID) error
	GetReaders(msgID MessageID) ([]PeerID, error)

	// Chats
	SaveChat(chat *Chat) error
	GetChat(id ChatID) (*Chat, error)
	ListChats() ([]*Chat, error)
	DeleteChat(id ChatID) error

	// Peers / contacts
	SavePeer(peer *Peer) error
	GetPeer(id PeerID) (*Peer, error)
	ListPeers() ([]*Peer, error)
	DeletePeer(id PeerID) error

	// Contact requests
	SaveContactRequest(req *ContactRequest) error
	GetContactRequest(id string) (*ContactRequest, error)
	ListContactRequests(status ContactRequestStatus) ([]*ContactRequest, error)

	// Sticker packs
	SaveStickerPack(pack *StickerPack) error
	GetStickerPack(id string) (*StickerPack, error)
	ListStickerPacks() ([]*StickerPack, error)
	DeleteStickerPack(id string) error

	// Media
	SaveMedia(hash string, data []byte) error
	GetMedia(hash string) ([]byte, error)
	DeleteMedia(hash string) error
	PruneMediaCache(maxBytes int64) (int64, error)

	// Pending retry queue
	EnqueuePending(msg *Message) error
	DequeuePending(limit int) ([]*Message, error)
	RemovePending(id MessageID) error

	Close() error
}

// MediaProcessor handles media encoding/decoding.
type MediaProcessor interface {
	ProcessImage(data []byte, opts interface{}) (*ProcessedMedia, error)
	ProcessAudio(data []byte, opts interface{}) (*ProcessedMedia, error)
	ProcessVideo(data []byte, opts interface{}) (*ProcessedMedia, error)
	ProcessFile(name string, data []byte) (*ProcessedMedia, error)
	ProcessSticker(data []byte) (*ProcessedMedia, error)
}

// ProcessedMedia is returned by every MediaProcessor method.
type ProcessedMedia struct {
	Hash      string
	Data      []byte
	Thumbnail []byte
	MimeType  string
	FileName  string
	Size      int64
	Width     int
	Height    int
	Duration  time.Duration
}

// TransferManager handles async file transfers with progress reporting.
type TransferManager interface {
	// Send enqueues an upload and returns a transfer ID.
	Send(ctx context.Context, chatID ChatID, msgID MessageID, data []byte, mimeType string) (string, error)
	// Receive downloads media for a message (e.g. from server CDN or peer).
	Receive(ctx context.Context, chatID ChatID, msgID MessageID, mediaHash string) (string, error)
	// Cancel cancels an in-progress transfer.
	Cancel(transferID string) error
	// Pause pauses an in-progress transfer.
	Pause(transferID string) error
	// Resume resumes a paused transfer.
	Resume(transferID string) error
	// Status returns the current progress of a transfer.
	Status(transferID string) (*TransferProgress, error)
}

// CallManager manages WebRTC-based voice/video calls.
type CallManager interface {
	InitiateCall(ctx context.Context, chatID ChatID, ctype CallType) (*CallSession, error)
	AcceptCall(ctx context.Context, sessionID string) error
	DeclineCall(ctx context.Context, sessionID string) error
	HangUp(ctx context.Context, sessionID string) error
	HandleSignal(ctx context.Context, from PeerID, signal []byte) error
	SetAudioEnabled(sessionID string, enabled bool) error
	SetVideoEnabled(sessionID string, enabled bool) error
	SetScreenShare(sessionID string, enabled bool) error
	ActiveCalls() []*CallSession
}

// EventBus is the internal pub-sub system.
type EventBus interface {
	Publish(event Event)
	Subscribe(id string, types ...EventType) <-chan Event
	Unsubscribe(id string)
}

// ─── Events ───────────────────────────────────────────────────────────────────

type EventType string

const (
	EvtMessageReceived   EventType = "message.received"
	EvtMessageSent       EventType = "message.sent"
	EvtMessageDelivered  EventType = "message.delivered"
	EvtMessageRead       EventType = "message.read"
	EvtMessageFailed     EventType = "message.failed"
	EvtMessageEdited     EventType = "message.edited"
	EvtMessageDeleted    EventType = "message.deleted"
	EvtMessageExpired    EventType = "message.expired"
	EvtReactionChanged   EventType = "message.reaction_changed"
	EvtPeerOnline        EventType = "peer.online"
	EvtPeerOffline       EventType = "peer.offline"
	EvtPeerStatusChanged EventType = "peer.status_changed"
	EvtPeerTyping        EventType = "peer.typing"        // Data: TypingEvent
	EvtPeerKeyChanged    EventType = "peer.key_changed"   // Data: PeerID – warn UI!
	EvtContactRequest    EventType = "contact.request"    // Data: *ContactRequest
	EvtContactAccepted   EventType = "contact.accepted"
	EvtContactDeclined   EventType = "contact.declined"
	EvtCallIncoming      EventType = "call.incoming"
	EvtCallAccepted      EventType = "call.accepted"
	EvtCallDeclined      EventType = "call.declined"
	EvtCallConnected     EventType = "call.connected"
	EvtCallEnded         EventType = "call.ended"
	EvtCallFailed        EventType = "call.failed"
	EvtCallQuality       EventType = "call.quality"       // Data: *CallQuality
	EvtTransferProgress  EventType = "transfer.progress"  // Data: *TransferProgress
	EvtNetworkJoined     EventType = "network.joined"
	EvtNetworkLeft       EventType = "network.left"
	EvtNetworkConnLost   EventType = "network.conn_lost"
	EvtNetworkConnRestored EventType = "network.conn_restored"
	EvtServerConnected   EventType = "server.connected"
	EvtServerDisconnected EventType = "server.disconnected"
	EvtSyncStarted       EventType = "sync.started"
	EvtSyncCompleted     EventType = "sync.completed"
	EvtKeyRotated        EventType = "key.rotated"
	EvtGroupMemberAdded  EventType = "group.member_added"
	EvtGroupMemberRemoved EventType = "group.member_removed"
	EvtGroupMemberLeft   EventType = "group.member_left"
	EvtError             EventType = "error"
)

// TypingEvent carries the chatID alongside the typing peer.
type TypingEvent struct {
	PeerID PeerID `json:"peer_id"`
	ChatID ChatID `json:"chat_id"`
}

// Event carries data emitted on the bus.
type Event struct {
	Type      EventType   `json:"type"`
	Timestamp time.Time   `json:"ts"`
	Data      interface{} `json:"data"`
}

// ─── Relay / Server Protocol ───────────────────────────────────────────────────

type RelayCommand string

const (
	CmdAuth          RelayCommand = "auth"
	CmdAuthOK        RelayCommand = "auth_ok"
	CmdRelay         RelayCommand = "relay"
	CmdSync          RelayCommand = "sync"
	CmdSyncResp      RelayCommand = "sync_resp"
	CmdPresence      RelayCommand = "presence"
	CmdPing          RelayCommand = "ping"
	CmdPong          RelayCommand = "pong"
	CmdError         RelayCommand = "error"
	CmdRegister      RelayCommand = "register"
	CmdLogin         RelayCommand = "login"
	CmdListPeers     RelayCommand = "list_peers"
	CmdUpdateProfile RelayCommand = "update_profile"
	CmdHandshake     RelayCommand = "handshake"
	CmdHandshakeAck  RelayCommand = "handshake_ack"
)

type RelayFrame struct {
	Cmd       RelayCommand    `json:"cmd"`
	ID        string          `json:"id,omitempty"`
	PeerID    string          `json:"peer_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	Timestamp int64           `json:"ts"`
}

type AuthPayload struct {
	NodeID      string `json:"node_id"`
	Fingerprint string `json:"fingerprint"`
	SignPub     []byte `json:"sign_pub"`
	Signature   []byte `json:"sig"`
	Timestamp   int64  `json:"ts"`
}

// ─── Config ───────────────────────────────────────────────────────────────────

// TransportMode controls how messages are delivered.
type TransportMode string

const (
	TransportP2P    TransportMode = "p2p"     // direct P2P via ZeroTier only
	TransportServer TransportMode = "server"  // through relay server only
	TransportBoth   TransportMode = "both"    // both P2P and server (default)
)

type Config struct {
	// ZeroTier
	ZeroTierPort    uint16      `json:"zt_port"`
	ZeroTierDataDir string      `json:"zt_data_dir"`
	Networks        []NetworkID `json:"networks"`

	// Identity
	IdentityFile string `json:"identity_file"`

	// Storage
	DBPath          string `json:"db_path"`
	MediaDir        string `json:"media_dir"`
	MaxMediaCacheMB int    `json:"max_media_cache_mb"`

	// Networking
	ListenPort      uint16        `json:"listen_port"`
	ServerAddresses []string      `json:"server_addresses"`
	UseServer       bool          `json:"use_server"`
	TransportMode   TransportMode `json:"transport_mode"`

	// Security
	E2EEnabled         bool          `json:"e2e_enabled"`
	RotateKeysEvery    time.Duration `json:"rotate_keys_every"`
	RotateAfterNMsgs   uint64        `json:"rotate_after_n_msgs"`
	RequireContactAccept bool        `json:"require_contact_accept"`

	// Media
	MaxImageWidthPx  int `json:"max_image_width"`
	MaxVideoHeightPx int `json:"max_video_height"`
	AudioBitrateKbps int `json:"audio_bitrate_kbps"`
	VideoBitrateKbps int `json:"video_bitrate_kbps"`

	// Reliability
	MaxRetries    int           `json:"max_retries"`
	RetryInterval time.Duration `json:"retry_interval"`
	MessageTTL    time.Duration `json:"message_ttl"`
	PingInterval  time.Duration `json:"ping_interval"`

	// Disappearing messages global default (0 = off)
	DisappearAfter time.Duration `json:"disappear_after"`

	// Logging
	LogLevel string `json:"log_level"`
	LogFile  string `json:"log_file,omitempty"`
}

func DefaultConfig() *Config {
	return &Config{
		ZeroTierPort:       9993,
		ZeroTierDataDir:    "./zt",
		ListenPort:         7777,
		DBPath:             "./messenger.db",
		MediaDir:           "./media",
		MaxMediaCacheMB:    512,
		E2EEnabled:         true,
		RotateKeysEvery:    7 * 24 * time.Hour,
		RotateAfterNMsgs:   100,
		RequireContactAccept: true,
		MaxImageWidthPx:    2048,
		MaxVideoHeightPx:   1080,
		AudioBitrateKbps:   64,
		VideoBitrateKbps:   1500,
		MaxRetries:         5,
		RetryInterval:      5 * time.Second,
		MessageTTL:         7 * 24 * time.Hour,
		PingInterval:       30 * time.Second,
		LogLevel:           "info",
		TransportMode:      TransportBoth,
		IdentityFile:       "identity.json",
	}
}

// ─── Messenger (top-level façade) ─────────────────────────────────────────────

// Messenger is the root interface exposed to the UI layer.
type Messenger interface {
	// Lifecycle
	Start(ctx context.Context) error
	Stop() error
	Config() *Config
	Stats() *NetworkStats

	// Identity & profile
	LocalPeer() *Peer
	SetDisplayName(name string) error
	SetAvatar(imgData []byte) error
	SetStatus(status PeerStatus, text string) error
	SafetyNumber(peerID PeerID) (string, error)

	// Networks
	JoinNetwork(id NetworkID) error
	LeaveNetwork(id NetworkID) error
	ListNetworks() []NetworkID

	// Contacts
	GetPeer(id PeerID) (*Peer, error)
	ListPeers() ([]*Peer, error)
	BlockPeer(id PeerID) error
	UnblockPeer(id PeerID) error
	SendContactRequest(ctx context.Context, nodeID NodeID, greeting string) error
	AcceptContactRequest(ctx context.Context, requestID string) error
	DeclineContactRequest(ctx context.Context, requestID string) error
	ListContactRequests() ([]*ContactRequest, error)

	// Chats
	CreateChat(name string, members []PeerID) (*Chat, error)
	GetChat(id ChatID) (*Chat, error)
	ListChats() ([]*Chat, error)
	DeleteChat(id ChatID) error
	LeaveChat(ctx context.Context, chatID ChatID) error
	AddChatMember(ctx context.Context, chatID ChatID, peer PeerID) error
	RemoveChatMember(ctx context.Context, chatID ChatID, peer PeerID) error
	SetMemberRole(ctx context.Context, chatID ChatID, peer PeerID, role MemberRole) error
	SetChatPermissions(ctx context.Context, chatID ChatID, perms Permission) error
	SetDisappearAfter(ctx context.Context, chatID ChatID, d time.Duration) error

	// Messaging
	SendText(ctx context.Context, chatID ChatID, text string, replyTo *MessageID) (*Message, error)
	SendImage(ctx context.Context, chatID ChatID, data []byte, caption string) (*Message, error)
	SendAudio(ctx context.Context, chatID ChatID, data []byte) (*Message, error)
	SendVideo(ctx context.Context, chatID ChatID, data []byte, caption string) (*Message, error)
	SendFile(ctx context.Context, chatID ChatID, name string, data []byte) (*Message, error)
	SendLocation(ctx context.Context, chatID ChatID, lat, lon float64) (*Message, error)
	SendSticker(ctx context.Context, chatID ChatID, packID, stickerID string) (*Message, error)
	ForwardMessage(ctx context.Context, msgID MessageID, toChatID ChatID) (*Message, error)
	SendTyping(ctx context.Context, chatID ChatID) error
	EditMessage(ctx context.Context, msgID MessageID, newText string) error
	DeleteMessage(ctx context.Context, msgID MessageID, forEveryone bool) error
	AddReaction(ctx context.Context, msgID MessageID, emoji string) error
	RemoveReaction(ctx context.Context, msgID MessageID, emoji string) error
	GetReactions(msgID MessageID) (map[string][]PeerID, error)
	GetMessageReaders(msgID MessageID) ([]PeerID, error)
	GetMessages(chatID ChatID, limit, offset int) ([]*Message, error)
	SearchMessages(chatID ChatID, query string) ([]*Message, error)
	MarkRead(ctx context.Context, chatID ChatID, upTo MessageID) error

	// Sticker packs
	InstallStickerPack(pack *StickerPack) error
	UninstallStickerPack(packID string) error
	ListStickerPacks() ([]*StickerPack, error)

	// Transfers
	Transfers() TransferManager

	// Calls
	Calls() CallManager

	// Events
	Events() EventBus

	// Sync
	SyncSince(ctx context.Context, since time.Time) error

	// Key management
	RotateKeys(ctx context.Context) error
	SendHandshake(ctx context.Context, to NodeID) error
}

// Ensure io is referenced (used by implementations).
var _ io.Reader = nil

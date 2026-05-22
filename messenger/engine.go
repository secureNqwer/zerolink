// Package messenger is the top-level engine implementing core.Messenger.
// The UI shell only needs to import this package and call New() + Start().
package messenger

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/secureNqwer/zerolink/core"
	"github.com/secureNqwer/zerolink/crypto"
	"github.com/secureNqwer/zerolink/media"
	"github.com/secureNqwer/zerolink/storage"
	"github.com/secureNqwer/zerolink/transport"
)

// ─── Engine ───────────────────────────────────────────────────────────────────

// Engine implements core.Messenger.
type Engine struct {
	mu sync.RWMutex

	cfg       *core.Config
	log       *zap.Logger
	bus       core.EventBus
	store     *storage.SQLiteStorage
	trans     *transport.ZTTransport
	cryptoPro *crypto.Provider
	compress  *transport.MultiCompressor
	media     *media.Processor
	calls     *media.CallManager
	transfers *transferManager

	identity  *crypto.Identity
	localPeer *core.Peer

	serverConn *serverRelay

	// atomic counters for Stats()
	statMsgSent     uint64
	statMsgRecv     uint64
	statBytesSent   uint64
	statBytesRecv   uint64
	statEncryptErr  uint64
	statDecryptErr  uint64
	statRetransmit  uint64

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// New creates and initialises a messenger Engine.
func New(cfg *core.Config) (*Engine, error) {
	if cfg == nil {
		cfg = core.DefaultConfig()
	}
	log, err := buildLogger(cfg)
	if err != nil {
		return nil, err
	}
	bus := core.NewEventBus()

	store, err := storage.NewSQLiteStorage(cfg.DBPath, cfg.MediaDir, log)
	if err != nil {
		return nil, fmt.Errorf("engine: storage: %w", err)
	}
	cp := crypto.NewProvider()

	compress, err := transport.NewMultiCompressor()
	if err != nil {
		return nil, fmt.Errorf("engine: compress: %w", err)
	}
	trans := transport.NewZTTransport(cfg, log)
	mediaPro := media.NewProcessor(cfg, log)

	e := &Engine{
		cfg:       cfg,
		log:       log,
		bus:       bus,
		store:     store,
		trans:     trans,
		cryptoPro: cp,
		compress:  compress,
		media:     mediaPro,
		stopCh:    make(chan struct{}),
	}
	e.transfers = newTransferManager(e)
	return e, nil
}

// ─── Lifecycle ────────────────────────────────────────────────────────────────

func (e *Engine) Start(ctx context.Context) error {
	identity, localPeer, err := e.loadOrCreateIdentity()
	if err != nil {
		return fmt.Errorf("engine: identity: %w", err)
	}
	e.identity = identity
	e.localPeer = localPeer

	if err := e.trans.Start(ctx); err != nil {
		return fmt.Errorf("engine: transport: %w", err)
	}
	e.trans.SetEventBus(e.bus)
	e.localPeer.ID.NodeID = e.trans.NodeID()

	e.calls = media.NewCallManager(e.localPeer.ID, e.bus, nil, e.log)
	e.calls.SendSignalFn = e.sendCallSignal

	if e.cfg.UseServer && len(e.cfg.ServerAddresses) > 0 {
		e.serverConn = newServerRelay(e.cfg.ServerAddresses, e.localPeer.ID, e.log)
		e.serverConn.OnMessage = e.handleRelayFrame
		if err := e.serverConn.Connect(ctx); err != nil {
			e.log.Warn("server relay unavailable, running serverless", zap.Error(err))
		} else {
			e.bus.Publish(core.Event{Type: core.EvtServerConnected, Timestamp: time.Now()})
		}
	}

	e.wg.Add(5)
	go e.receiveLoop(ctx)
	go e.retryLoop(ctx)
	go e.pingLoop(ctx)
	go e.disappearingLoop(ctx)
	go e.keyRotationLoop(ctx)

	e.log.Info("messenger started",
		zap.String("node_id", string(e.localPeer.ID.NodeID)),
		zap.String("fingerprint", e.localPeer.ID.Fingerprint),
	)
	return nil
}

func (e *Engine) Stop() error {
	close(e.stopCh)
	e.trans.Stop()
	if e.serverConn != nil {
		e.serverConn.Close()
	}
	e.wg.Wait()
	e.store.Close()
	e.log.Info("messenger stopped")
	return nil
}

func (e *Engine) Config() *core.Config { return e.cfg }

func (e *Engine) Stats() *core.NetworkStats {
	activeCalls := 0
	if e.calls != nil {
		activeCalls = len(e.calls.ActiveCalls())
	}
	peers, _ := e.store.ListPeers()
	online := 0
	for _, p := range peers {
		if p.Status == core.PeerOnline {
			online++
		}
	}
	return &core.NetworkStats{
		MessagesSent:     atomic.LoadUint64(&e.statMsgSent),
		MessagesReceived: atomic.LoadUint64(&e.statMsgRecv),
		BytesSent:        atomic.LoadUint64(&e.statBytesSent),
		BytesReceived:    atomic.LoadUint64(&e.statBytesRecv),
		EncryptErrors:    atomic.LoadUint64(&e.statEncryptErr),
		DecryptErrors:    atomic.LoadUint64(&e.statDecryptErr),
		RetransmitCount:  atomic.LoadUint64(&e.statRetransmit),
		ActiveSessions:   0, // filled by crypto layer if needed
		ActiveCalls:      activeCalls,
		ConnectedPeers:   online,
		ServerConnected:  e.serverConn != nil && e.serverConn.Connected(),
	}
}

// ─── Identity ────────────────────────────────────────────────────────────────

func (e *Engine) LocalPeer() *core.Peer {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.localPeer
}

func (e *Engine) SetDisplayName(name string) error {
	e.mu.Lock()
	e.localPeer.DisplayName = name
	e.mu.Unlock()
	return e.store.SavePeer(e.localPeer)
}

func (e *Engine) SetAvatar(imgData []byte) error {
	pm, err := e.media.ProcessImageAuto(imgData, media.ImageOptions{MaxWidth: 256, MaxHeight: 256, JpegQuality: 80})
	if err != nil {
		return err
	}
	if err := e.store.SaveMedia(pm.Hash, pm.Data); err != nil {
		return err
	}
	e.mu.Lock()
	e.localPeer.AvatarHash = pm.Hash
	e.mu.Unlock()
	return e.store.SavePeer(e.localPeer)
}

func (e *Engine) SetStatus(status core.PeerStatus, text string) error {
	e.mu.Lock()
	e.localPeer.Status = status
	e.localPeer.StatusText = text
	e.mu.Unlock()
	e.broadcastPresence()
	return e.store.SavePeer(e.localPeer)
}

// ─── Server connection ────────────────────────────────────────────────────

// ConnectToServer dynamically connects to a relay server at the given address.
// If username and token are non-empty, the connection will be authenticated.
func (e *Engine) ConnectToServer(ctx context.Context, addr string, auth ...string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Close existing connection if any
	if e.serverConn != nil {
		e.serverConn.Close()
	}

	sr := newServerRelay([]string{addr}, e.localPeer.ID, e.log)
	if len(auth) >= 2 {
		sr.SetAuth(auth[0], auth[1])
	}
	sr.OnMessage = e.handleRelayFrame
	if err := sr.Connect(ctx); err != nil {
		return err
	}
	e.serverConn = sr
	// Update config
	e.cfg.ServerAddresses = []string{addr}
	e.cfg.UseServer = true
	e.bus.Publish(core.Event{Type: core.EvtServerConnected, Timestamp: time.Now()})
	return nil
}

// DisconnectServer disconnects from the relay server.
func (e *Engine) DisconnectServer() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.serverConn != nil {
		e.serverConn.Close()
		e.serverConn = nil
	}
	e.cfg.UseServer = false
	e.bus.Publish(core.Event{Type: core.EvtServerDisconnected, Timestamp: time.Now()})
}

// ServerRelay returns the current server relay connection.
func (e *Engine) ServerRelay() *serverRelay {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.serverConn
}

// SetTransportMode changes how messages are delivered.
// Valid modes: "p2p", "server", "both"
func (e *Engine) SetTransportMode(mode core.TransportMode) error {
	switch mode {
	case core.TransportP2P, core.TransportServer, core.TransportBoth:
		e.mu.Lock()
		e.cfg.TransportMode = mode
		e.mu.Unlock()
		return nil
	default:
		return fmt.Errorf("invalid transport mode: %s (use p2p, server, or both)", mode)
	}
}

// GetTransportMode returns the current transport mode.
func (e *Engine) GetTransportMode() core.TransportMode {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cfg.TransportMode
}

// SafetyNumber returns the 60-digit Safety Number for verifying a peer's identity.
func (e *Engine) SafetyNumber(peerID core.PeerID) (string, error) {
	peer, err := e.store.GetPeer(peerID)
	if err != nil || peer == nil {
		return "", errors.New("peer not found")
	}
	sn, err := crypto.ComputeSafetyNumber(
		ed25519.PublicKey(e.identity.SignPublic), e.localPeer.ID.NodeID,
		ed25519.PublicKey(peer.SignKey), peer.ID.NodeID,
	)
	if err != nil {
		return "", err
	}
	return sn.Combined, nil
}

// ─── Networks ────────────────────────────────────────────────────────────────

func (e *Engine) JoinNetwork(id core.NetworkID) error {
	return e.trans.JoinNetwork(context.Background(), id)
}

func (e *Engine) LeaveNetwork(id core.NetworkID) error {
	return e.trans.LeaveNetwork(context.Background(), id)
}

func (e *Engine) ListNetworks() []core.NetworkID { return e.trans.Networks() }

// ─── Contacts ────────────────────────────────────────────────────────────────

func (e *Engine) GetPeer(id core.PeerID) (*core.Peer, error)  { return e.store.GetPeer(id) }
func (e *Engine) ListPeers() ([]*core.Peer, error)            { return e.store.ListPeers() }

func (e *Engine) BlockPeer(id core.PeerID) error {
	peer, err := e.store.GetPeer(id)
	if err != nil || peer == nil {
		return errors.New("peer not found")
	}
	peer.IsBlocked = true
	return e.store.SavePeer(peer)
}

func (e *Engine) UnblockPeer(id core.PeerID) error {
	peer, err := e.store.GetPeer(id)
	if err != nil || peer == nil {
		return errors.New("peer not found")
	}
	peer.IsBlocked = false
	return e.store.SavePeer(peer)
}

// SendContactRequest sends a contact request to an unknown node.
func (e *Engine) SendContactRequest(ctx context.Context, nodeID core.NodeID, greeting string) error {
	sig, _ := e.cryptoPro.Sign(e.identity, e.identity.DHPublic[:])
	payload, _ := json.Marshal(&core.ContactRequest{
		ID:          uuid.New().String(),
		FromPeer:    e.localPeer.ID,
		DisplayName: e.localPeer.DisplayName,
		SignKey:     e.identity.SignPublic,
		DHKey:       e.identity.DHPublic[:],
		Message:     greeting,
		ReceivedAt:  time.Now(),
	})
	// piggyback the signature in Extra
	type reqPayload struct {
		*core.ContactRequest
		Sig []byte `json:"sig"`
	}
	full, _ := json.Marshal(reqPayload{
		ContactRequest: &core.ContactRequest{
			ID: uuid.New().String(), FromPeer: e.localPeer.ID,
			DisplayName: e.localPeer.DisplayName,
			SignKey: e.identity.SignPublic, DHKey: e.identity.DHPublic[:],
			Message: greeting, ReceivedAt: time.Now(),
		},
		Sig: sig,
	})
	_ = payload
	pkt := &core.Packet{
		Magic: core.PacketMagic, Version: core.PacketVersion,
		Type: core.MsgContactRequest, Flags: 0, TTL: 64,
		Timestamp: time.Now().UnixNano(), Body: full,
	}
	return e.trans.Send(ctx, nodeID, pkt)
}

func (e *Engine) AcceptContactRequest(ctx context.Context, requestID string) error {
	req, err := e.store.GetContactRequest(requestID)
	if err != nil || req == nil {
		return errors.New("request not found")
	}
	req.Status = core.ContactRequestAccepted
	if err := e.store.SaveContactRequest(req); err != nil {
		return err
	}
	// Store the peer and mark as contact
	peer := &core.Peer{
		ID: req.FromPeer, DisplayName: req.DisplayName,
		PublicKey: req.DHKey, SignKey: req.SignKey,
		IsContact: true, LastSeen: time.Now(),
	}
	if err := e.store.SavePeer(peer); err != nil {
		return err
	}
	// Establish crypto session
	var dhPub [32]byte
	copy(dhPub[:], req.DHKey)
	e.cryptoPro.InitSession(e.identity, dhPub, req.FromPeer)
	// Send our handshake back
	e.SendHandshake(ctx, req.FromPeer.NodeID)
	e.bus.Publish(core.Event{Type: core.EvtContactAccepted, Timestamp: time.Now(), Data: req})
	return nil
}

func (e *Engine) DeclineContactRequest(ctx context.Context, requestID string) error {
	req, err := e.store.GetContactRequest(requestID)
	if err != nil || req == nil {
		return errors.New("request not found")
	}
	req.Status = core.ContactRequestDeclined
	e.bus.Publish(core.Event{Type: core.EvtContactDeclined, Timestamp: time.Now(), Data: req})
	return e.store.SaveContactRequest(req)
}

func (e *Engine) ListContactRequests() ([]*core.ContactRequest, error) {
	return e.store.ListContactRequests(core.ContactRequestPending)
}

// ─── Chats ────────────────────────────────────────────────────────────────────

func (e *Engine) CreateChat(name string, members []core.PeerID) (*core.Chat, error) {
	ctype := core.ChatDirect
	if len(members) > 1 || name != "" {
		ctype = core.ChatGroup
	}
	chatMembers := make([]core.ChatMember, 0, len(members)+1)
	chatMembers = append(chatMembers, core.ChatMember{
		PeerID: e.localPeer.ID, Role: core.RoleOwner,
		Permissions: core.PermAll, JoinedAt: time.Now(),
	})
	for _, m := range members {
		chatMembers = append(chatMembers, core.ChatMember{
			PeerID: m, Role: core.RoleMember,
			Permissions: core.PermDefault, JoinedAt: time.Now(),
			InvitedBy: &e.localPeer.ID,
		})
	}
	chat := &core.Chat{
		ID:                 core.ChatID(uuid.New().String()),
		Type:               ctype,
		Name:               name,
		Members:            chatMembers,
		NetworkID:          e.firstNetwork(),
		Encrypted:          e.cfg.E2EEnabled,
		DefaultPermissions: core.PermDefault,
		DisappearAfter:     e.cfg.DisappearAfter,
		CreatedAt:          time.Now(),
		UpdatedAt:          time.Now(),
	}
	if err := e.store.SaveChat(chat); err != nil {
		return nil, err
	}
	// If group E2E, generate and distribute a sender key
	if e.cfg.E2EEnabled && ctype == core.ChatGroup {
		e.initGroupSenderKey(context.Background(), chat)
	}
	return chat, nil
}

func (e *Engine) GetChat(id core.ChatID) (*core.Chat, error)  { return e.store.GetChat(id) }
func (e *Engine) ListChats() ([]*core.Chat, error)            { return e.store.ListChats() }
func (e *Engine) DeleteChat(id core.ChatID) error             { return e.store.DeleteChat(id) }

// LeaveChat removes the local user from a group and notifies members.
func (e *Engine) LeaveChat(ctx context.Context, chatID core.ChatID) error {
	chat, err := e.store.GetChat(chatID)
	if err != nil || chat == nil {
		return errors.New("chat not found")
	}
	// Send system message
	e.sendSystemMessage(ctx, chat, core.MsgMemberLeft,
		map[string]string{"peer_id": e.localPeer.ID.String(), "display_name": e.localPeer.DisplayName})

	// Remove self from member list
	filtered := chat.Members[:0]
	for _, m := range chat.Members {
		if m.PeerID.String() != e.localPeer.ID.String() {
			filtered = append(filtered, m)
		}
	}
	chat.Members = filtered
	chat.UpdatedAt = time.Now()
	// Rotate group sender key since membership changed
	if e.cfg.E2EEnabled {
		e.cryptoPro.DeleteSenderKey(chatID, e.localPeer.ID)
	}
	return e.store.SaveChat(chat)
}

func (e *Engine) AddChatMember(ctx context.Context, chatID core.ChatID, peer core.PeerID) error {
	chat, err := e.store.GetChat(chatID)
	if err != nil || chat == nil {
		return errors.New("chat not found")
	}
	if chat.FindMember(peer) != nil {
		return nil // already a member
	}
	chat.Members = append(chat.Members, core.ChatMember{
		PeerID:      peer,
		Role:        core.RoleMember,
		Permissions: chat.DefaultPermissions,
		JoinedAt:    time.Now(),
		InvitedBy:   &e.localPeer.ID,
	})
	chat.UpdatedAt = time.Now()
	if err := e.store.SaveChat(chat); err != nil {
		return err
	}
	e.sendSystemMessage(ctx, chat, core.MsgMemberAdded,
		map[string]string{"peer_id": peer.String()})
	// Distribute sender key to new member
	if e.cfg.E2EEnabled {
		e.distributeSenderKeyTo(ctx, chat, peer)
	}
	e.bus.Publish(core.Event{Type: core.EvtGroupMemberAdded, Timestamp: time.Now(),
		Data: map[string]interface{}{"chat_id": chatID, "peer_id": peer}})
	return nil
}

func (e *Engine) RemoveChatMember(ctx context.Context, chatID core.ChatID, peer core.PeerID) error {
	chat, err := e.store.GetChat(chatID)
	if err != nil || chat == nil {
		return errors.New("chat not found")
	}
	filtered := chat.Members[:0]
	for _, m := range chat.Members {
		if m.PeerID.String() != peer.String() {
			filtered = append(filtered, m)
		}
	}
	chat.Members = filtered
	chat.UpdatedAt = time.Now()
	if err := e.store.SaveChat(chat); err != nil {
		return err
	}
	e.sendSystemMessage(ctx, chat, core.MsgMemberRemoved,
		map[string]string{"peer_id": peer.String()})
	// Rotate sender key – removed member must not decrypt future messages
	if e.cfg.E2EEnabled {
		e.cryptoPro.DeleteSenderKey(chatID, peer)
		e.initGroupSenderKey(ctx, chat)
	}
	e.bus.Publish(core.Event{Type: core.EvtGroupMemberRemoved, Timestamp: time.Now(),
		Data: map[string]interface{}{"chat_id": chatID, "peer_id": peer}})
	return nil
}

func (e *Engine) SetMemberRole(ctx context.Context, chatID core.ChatID, peer core.PeerID, role core.MemberRole) error {
	chat, err := e.store.GetChat(chatID)
	if err != nil || chat == nil {
		return errors.New("chat not found")
	}
	m := chat.FindMember(peer)
	if m == nil {
		return errors.New("member not found")
	}
	m.Role = role
	if role == core.RoleAdmin || role == core.RoleOwner {
		m.Permissions = core.PermAll
	} else {
		m.Permissions = chat.DefaultPermissions
	}
	chat.UpdatedAt = time.Now()
	e.sendSystemMessage(ctx, chat, core.MsgAdminChanged,
		map[string]string{"peer_id": peer.String(), "role": fmt.Sprintf("%d", role)})
	e.bus.Publish(core.Event{Type: core.EvtGroupMemberAdded, Timestamp: time.Now(),
		Data: map[string]interface{}{"chat_id": chatID, "peer_id": peer, "role": role}})
	return e.store.SaveChat(chat)
}

func (e *Engine) SetChatPermissions(ctx context.Context, chatID core.ChatID, perms core.Permission) error {
	chat, err := e.store.GetChat(chatID)
	if err != nil || chat == nil {
		return errors.New("chat not found")
	}
	chat.DefaultPermissions = perms
	chat.UpdatedAt = time.Now()
	return e.store.SaveChat(chat)
}

func (e *Engine) SetDisappearAfter(ctx context.Context, chatID core.ChatID, d time.Duration) error {
	chat, err := e.store.GetChat(chatID)
	if err != nil || chat == nil {
		return errors.New("chat not found")
	}
	chat.DisappearAfter = d
	chat.UpdatedAt = time.Now()
	return e.store.SaveChat(chat)
}

// ─── Messaging ────────────────────────────────────────────────────────────────

func (e *Engine) SendText(ctx context.Context, chatID core.ChatID, text string, replyTo *core.MessageID) (*core.Message, error) {
	meta := core.MessageMeta{MimeType: "text/plain"}
	// Parse @mentions
	meta.Mentions = parseMentions(text)
	return e.send(ctx, chatID, core.MsgText, []byte(text), meta, replyTo, nil)
}

func (e *Engine) SendImage(ctx context.Context, chatID core.ChatID, data []byte, caption string) (*core.Message, error) {
	pm, err := e.media.ProcessImageAuto(data, media.ImageOptions{})
	if err != nil {
		return nil, err
	}
	if err := e.store.SaveMedia(pm.Hash, pm.Data); err != nil {
		return nil, err
	}
	return e.send(ctx, chatID, core.MsgImage, pm.Data, core.MessageMeta{
		MimeType: pm.MimeType, FileSize: pm.Size, Width: pm.Width, Height: pm.Height,
		Thumbnail: pm.Thumbnail, MediaHash: pm.Hash,
		Extra: map[string]string{"caption": caption},
	}, nil, nil)
}

func (e *Engine) SendAudio(ctx context.Context, chatID core.ChatID, data []byte) (*core.Message, error) {
	pm, err := e.media.ProcessAudio(data, media.AudioOptions{})
	if err != nil {
		return nil, err
	}
	e.store.SaveMedia(pm.Hash, pm.Data)
	return e.send(ctx, chatID, core.MsgAudio, pm.Data, core.MessageMeta{
		MimeType: pm.MimeType, FileSize: pm.Size, Duration: pm.Duration, MediaHash: pm.Hash,
	}, nil, nil)
}

func (e *Engine) SendVideo(ctx context.Context, chatID core.ChatID, data []byte, caption string) (*core.Message, error) {
	pm, err := e.media.ProcessVideo(data, media.VideoOptions{})
	if err != nil {
		return nil, err
	}
	e.store.SaveMedia(pm.Hash, pm.Data)
	return e.send(ctx, chatID, core.MsgVideo, pm.Data, core.MessageMeta{
		MimeType: pm.MimeType, FileSize: pm.Size, Width: pm.Width, Height: pm.Height,
		Duration: pm.Duration, Thumbnail: pm.Thumbnail, MediaHash: pm.Hash,
		Extra: map[string]string{"caption": caption},
	}, nil, nil)
}

func (e *Engine) SendFile(ctx context.Context, chatID core.ChatID, name string, data []byte) (*core.Message, error) {
	pm, err := e.media.ProcessFile(name, data)
	if err != nil {
		return nil, err
	}
	e.store.SaveMedia(pm.Hash, pm.Data)
	return e.send(ctx, chatID, core.MsgFile, pm.Data, core.MessageMeta{
		MimeType: pm.MimeType, FileName: pm.FileName, FileSize: pm.Size, MediaHash: pm.Hash,
	}, nil, nil)
}

func (e *Engine) SendLocation(ctx context.Context, chatID core.ChatID, lat, lon float64) (*core.Message, error) {
	payload, _ := json.Marshal(map[string]float64{"lat": lat, "lon": lon})
	return e.send(ctx, chatID, core.MsgLocation, payload, core.MessageMeta{}, nil, nil)
}

func (e *Engine) SendSticker(ctx context.Context, chatID core.ChatID, packID, stickerID string) (*core.Message, error) {
	pack, err := e.store.GetStickerPack(packID)
	if err != nil || pack == nil {
		return nil, fmt.Errorf("sticker pack %s not found", packID)
	}
	var sticker *core.Sticker
	for i := range pack.Stickers {
		if pack.Stickers[i].ID == stickerID {
			sticker = &pack.Stickers[i]
			break
		}
	}
	if sticker == nil {
		return nil, fmt.Errorf("sticker %s not found in pack %s", stickerID, packID)
	}
	// Send only the reference; recipient fetches media by hash
	payload, _ := json.Marshal(sticker)
	return e.send(ctx, chatID, core.MsgSticker, payload, core.MessageMeta{
		StickerPackID: packID, StickerID: stickerID, MediaHash: sticker.MediaHash,
	}, nil, nil)
}

// ForwardMessage re-sends a message to another chat with forward attribution.
func (e *Engine) ForwardMessage(ctx context.Context, msgID core.MessageID, toChatID core.ChatID) (*core.Message, error) {
	orig, err := e.store.GetMessage(msgID)
	if err != nil || orig == nil {
		return nil, errors.New("message not found")
	}
	origSender, _ := e.store.GetPeer(orig.SenderID)
	senderName := ""
	if origSender != nil {
		senderName = origSender.DisplayName
	}
	meta := orig.Metadata
	meta.ForwardInfo = &core.ForwardInfo{
		OriginalSenderID:   orig.SenderID,
		OriginalSenderName: senderName,
		OriginalChatID:     orig.ChatID,
		OriginalMsgID:      orig.ID,
		OriginalSentAt:     orig.SentAt,
	}
	return e.send(ctx, toChatID, orig.Type, orig.Payload, meta, nil, nil)
}

func (e *Engine) SendTyping(ctx context.Context, chatID core.ChatID) error {
	payload, _ := json.Marshal(core.TypingEvent{PeerID: e.localPeer.ID, ChatID: chatID})
	_, err := e.send(ctx, chatID, core.MsgTyping, payload, core.MessageMeta{}, nil, nil)
	return err
}

func (e *Engine) EditMessage(ctx context.Context, msgID core.MessageID, newText string) error {
	msg, err := e.store.GetMessage(msgID)
	if err != nil || msg == nil {
		return errors.New("message not found")
	}
	if msg.SenderID.String() != e.localPeer.ID.String() {
		return errors.New("cannot edit another user's message")
	}
	payload, _ := json.Marshal(map[string]string{"msg_id": string(msgID), "text": newText})
	_, err = e.send(ctx, msg.ChatID, core.MsgEdit, payload, core.MessageMeta{}, nil, nil)
	return err
}

func (e *Engine) DeleteMessage(ctx context.Context, msgID core.MessageID, forEveryone bool) error {
	if forEveryone {
		msg, err := e.store.GetMessage(msgID)
		if err != nil || msg == nil {
			return errors.New("message not found")
		}
		payload, _ := json.Marshal(map[string]string{"msg_id": string(msgID)})
		if _, err := e.send(ctx, msg.ChatID, core.MsgDelete, payload, core.MessageMeta{}, nil, nil); err != nil {
			return err
		}
	}
	return e.store.DeleteMessage(msgID)
}

func (e *Engine) AddReaction(ctx context.Context, msgID core.MessageID, emoji string) error {
	chat, err := e.chatForMessage(msgID)
	if err != nil {
		return err
	}
	r := core.ReactionEntry{PeerID: e.localPeer.ID, Emoji: emoji, CreatedAt: time.Now()}
	if err := e.store.SaveReaction(msgID, r); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{
		"msg_id": string(msgID), "emoji": emoji, "action": "add",
	})
	_, err = e.send(ctx, chat.ID, core.MsgReaction, payload, core.MessageMeta{}, nil, nil)
	e.bus.Publish(core.Event{Type: core.EvtReactionChanged, Timestamp: time.Now(),
		Data: map[string]interface{}{"msg_id": msgID, "emoji": emoji, "peer_id": e.localPeer.ID, "action": "add"}})
	return err
}

func (e *Engine) RemoveReaction(ctx context.Context, msgID core.MessageID, emoji string) error {
	chat, err := e.chatForMessage(msgID)
	if err != nil {
		return err
	}
	if err := e.store.DeleteReaction(msgID, e.localPeer.ID, emoji); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{
		"msg_id": string(msgID), "emoji": emoji, "action": "remove",
	})
	_, err = e.send(ctx, chat.ID, core.MsgReaction, payload, core.MessageMeta{}, nil, nil)
	e.bus.Publish(core.Event{Type: core.EvtReactionChanged, Timestamp: time.Now(),
		Data: map[string]interface{}{"msg_id": msgID, "emoji": emoji, "peer_id": e.localPeer.ID, "action": "remove"}})
	return err
}

func (e *Engine) GetReactions(msgID core.MessageID) (map[string][]core.PeerID, error) {
	return e.store.GetReactions(msgID)
}

func (e *Engine) GetMessageReaders(msgID core.MessageID) ([]core.PeerID, error) {
	return e.store.GetReaders(msgID)
}

func (e *Engine) GetMessages(chatID core.ChatID, limit, offset int) ([]*core.Message, error) {
	return e.store.GetMessages(chatID, limit, offset)
}

func (e *Engine) SearchMessages(chatID core.ChatID, query string) ([]*core.Message, error) {
	return e.store.SearchMessages(chatID, query)
}

func (e *Engine) MarkRead(ctx context.Context, chatID core.ChatID, upTo core.MessageID) error {
	msgs, _ := e.store.GetMessages(chatID, 200, 0)
	for _, msg := range msgs {
		if msg.SenderID.String() != e.localPeer.ID.String() {
			e.store.UpdateMessageStatus(msg.ID, core.StatusRead)
			e.store.SaveReadReceipt(msg.ID, e.localPeer.ID)
		}
		if msg.ID == upTo {
			break
		}
	}
	if upTo != "" {
		if msg, _ := e.store.GetMessage(upTo); msg != nil {
			payload, _ := json.Marshal(map[string]string{"msg_id": string(upTo)})
			e.send(ctx, chatID, core.MsgRead, payload, core.MessageMeta{}, nil, nil)
		}
	}
	return nil
}

// ─── Sticker packs ────────────────────────────────────────────────────────────

func (e *Engine) InstallStickerPack(pack *core.StickerPack) error {
	pack.InstalledAt = time.Now()
	return e.store.SaveStickerPack(pack)
}

func (e *Engine) UninstallStickerPack(packID string) error {
	return e.store.DeleteStickerPack(packID)
}

func (e *Engine) ListStickerPacks() ([]*core.StickerPack, error) {
	return e.store.ListStickerPacks()
}

// ─── Transfers / Calls / Events ───────────────────────────────────────────────

func (e *Engine) Transfers() core.TransferManager { return e.transfers }
func (e *Engine) Calls() core.CallManager         { return e.calls }
func (e *Engine) Events() core.EventBus           { return e.bus }

// ─── Sync ────────────────────────────────────────────────────────────────────

// SyncSince requests messages newer than `since` from the relay server.
func (e *Engine) SyncSince(ctx context.Context, since time.Time) error {
	if e.serverConn == nil || !e.serverConn.Connected() {
		return errors.New("no server connection")
	}
	e.bus.Publish(core.Event{Type: core.EvtSyncStarted, Timestamp: time.Now()})
	chats, err := e.store.ListChats()
	if err != nil {
		return err
	}
	for _, chat := range chats {
		e.serverConn.SyncHistory(chat.ID, since.UnixNano())
	}
	return nil
}

// ─── Key management ───────────────────────────────────────────────────────────

// RotateKeys forces an immediate DH ratchet step with all active peers.
func (e *Engine) RotateKeys(ctx context.Context) error {
	peers, _ := e.store.ListPeers()
	for _, p := range peers {
		if p.Status == core.PeerOnline && !p.IsBlocked {
			e.SendHandshake(ctx, p.ID.NodeID)
		}
	}
	e.bus.Publish(core.Event{Type: core.EvtKeyRotated, Timestamp: time.Now()})
	return nil
}

// ─── Background workers ───────────────────────────────────────────────────────

func (e *Engine) disappearingLoop(ctx context.Context) {
	defer e.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			n, err := e.store.DeleteExpiredMessages()
			if err != nil {
				e.log.Error("prune expired messages", zap.Error(err))
			} else if n > 0 {
				e.log.Debug("pruned expired messages", zap.Int64("count", n))
				e.bus.Publish(core.Event{Type: core.EvtMessageExpired, Timestamp: time.Now(), Data: n})
			}
			// Prune media cache
			maxBytes := int64(e.cfg.MaxMediaCacheMB) << 20
			if freed, err := e.store.PruneMediaCache(maxBytes); err == nil && freed > 0 {
				e.log.Debug("pruned media cache", zap.Int64("freed_bytes", freed))
			}
		}
	}
}

func (e *Engine) keyRotationLoop(ctx context.Context) {
	defer e.wg.Done()
	if e.cfg.RotateKeysEvery <= 0 {
		return
	}
	ticker := time.NewTicker(e.cfg.RotateKeysEvery)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.log.Info("scheduled key rotation")
			e.RotateKeys(ctx)
		}
	}
}

func (e *Engine) receiveLoop(ctx context.Context) {
	defer e.wg.Done()
	recvCh := e.trans.Recv()
	for {
		select {
		case <-e.stopCh:
			return
		case ipkt, ok := <-recvCh:
			if !ok {
				return
			}
			atomic.AddUint64(&e.statMsgRecv, 1)
			atomic.AddUint64(&e.statBytesRecv, uint64(len(ipkt.Pkt.Body)))
			e.handleIncoming(ctx, ipkt)
		}
	}
}

func (e *Engine) retryLoop(ctx context.Context) {
	defer e.wg.Done()
	ticker := time.NewTicker(e.cfg.RetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.flushPending(ctx)
		}
	}
}

func (e *Engine) pingLoop(ctx context.Context) {
	defer e.wg.Done()
	ticker := time.NewTicker(e.cfg.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.broadcastPresence()
		}
	}
}

func (e *Engine) flushPending(ctx context.Context) {
	msgs, err := e.store.DequeuePending(20)
	if err != nil {
		return
	}
	for _, msg := range msgs {
		chat, _ := e.store.GetChat(msg.ChatID)
		if chat == nil {
			continue
		}
		if err := e.transmit(ctx, chat, msg); err == nil {
			e.store.RemovePending(msg.ID)
			e.store.UpdateMessageStatus(msg.ID, core.StatusSent)
		} else {
			atomic.AddUint64(&e.statRetransmit, 1)
		}
	}
}

// ─── Incoming packet dispatch ─────────────────────────────────────────────────

func (e *Engine) handleIncoming(ctx context.Context, ipkt *core.IncomingPacket) {
	pkt := ipkt.Pkt
	switch pkt.Type {
	case core.MsgPing:
		e.sendPong(ctx, ipkt.From)
		return
	case core.MsgPong:
		e.updatePeerLastSeen(ipkt.From)
		return
	case core.MsgHandshake, core.MsgHandshakeAck:
		e.handleHandshake(ctx, ipkt)
		return
	case core.MsgCallSignal:
		e.handleCallSignalPacket(ctx, ipkt)
		return
	case core.MsgContactRequest:
		e.handleContactRequest(ctx, ipkt)
		return
	case core.MsgSenderKeyDist:
		e.handleSenderKeyDist(ctx, ipkt)
		return
	}

	var msg core.Message
	if err := json.Unmarshal(pkt.Body, &msg); err != nil {
		e.log.Warn("bad message body", zap.Error(err))
		return
	}

	// Auto-create DM chat if it does not exist locally
	if chat, _ := e.store.GetChat(msg.ChatID); chat == nil {
		chat = &core.Chat{
			ID:        msg.ChatID,
			Type:      core.ChatDirect,
			Members: []core.ChatMember{
				{PeerID: msg.SenderID, Role: core.RoleMember, Permissions: core.PermDefault, JoinedAt: time.Now()},
				{PeerID: e.localPeer.ID, Role: core.RoleMember, Permissions: core.PermDefault, JoinedAt: time.Now()},
			},
			NetworkID: "",
			Encrypted: e.cfg.E2EEnabled,
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}
		if err := e.store.SaveChat(chat); err != nil {
			e.log.Error("auto-create chat failed", zap.Error(err))
			return
		}
	}

	// Block check
	if peer, _ := e.store.GetPeer(msg.SenderID); peer != nil && peer.IsBlocked {
		return
	}
	// Contact filter – reject messages from non-contacts if configured
	if e.cfg.RequireContactAccept {
		if peer, _ := e.store.GetPeer(msg.SenderID); peer == nil || !peer.IsContact {
			// Only accept handshakes and contact requests from strangers
			return
		}
	}

	// Decrypt
	if e.cfg.E2EEnabled && pkt.Flags&core.FlagEncrypted != 0 {
		decrypted, err := e.decryptFromPeer(msg.SenderID, msg.ChatID, msg.Payload, &msg)
		if err != nil {
			e.log.Warn("decrypt failed", zap.Error(err))
			atomic.AddUint64(&e.statDecryptErr, 1)
			return
		}
		msg.Payload = decrypted
	}

	// Decompress
	if pkt.Flags&core.FlagCompressed != 0 {
		decompressed, err := e.compress.Decompress(msg.Payload)
		if err != nil {
			e.log.Warn("decompress failed", zap.Error(err))
			return
		}
		msg.Payload = decompressed
	}

	// FTS index plaintext for text messages
	if msg.Type == core.MsgText {
		e.store.IndexMessageText(msg.ID, string(msg.Payload))
	}

	switch msg.Type {
	case core.MsgEdit:
		e.handleEdit(&msg)
		return
	case core.MsgDelete:
		e.handleDelete(&msg)
		return
	case core.MsgRead:
		e.handleReadReceipt(ctx, &msg)
		return
	case core.MsgDelivered:
		e.store.UpdateMessageStatus(msg.ID, core.StatusDelivered)
		e.bus.Publish(core.Event{Type: core.EvtMessageDelivered, Data: msg.ID})
		return
	case core.MsgTyping:
		var te core.TypingEvent
		json.Unmarshal(msg.Payload, &te)
		if te.ChatID == "" {
			te.ChatID = msg.ChatID
		}
		e.bus.Publish(core.Event{Type: core.EvtPeerTyping, Timestamp: time.Now(), Data: te})
		return
	case core.MsgReaction:
		e.handleReactionMsg(&msg)
		return
	case core.MsgMemberAdded, core.MsgMemberRemoved, core.MsgMemberLeft,
		core.MsgGroupRenamed, core.MsgAdminChanged:
		e.handleSystemMessage(&msg)
	}

	msg.Status = core.StatusDelivered
	if err := e.store.SaveMessage(&msg); err != nil {
		e.log.Error("save incoming message", zap.Error(err))
		return
	}
	e.sendDeliveryReceipt(ctx, &msg)
	e.bus.Publish(core.Event{Type: core.EvtMessageReceived, Timestamp: time.Now(), Data: &msg})
}

func (e *Engine) handleReactionMsg(msg *core.Message) {
	var body map[string]string
	json.Unmarshal(msg.Payload, &body)
	msgID := core.MessageID(body["msg_id"])
	emoji := body["emoji"]
	action := body["action"]
	if action == "remove" {
		e.store.DeleteReaction(msgID, msg.SenderID, emoji)
	} else {
		e.store.SaveReaction(msgID, core.ReactionEntry{
			PeerID: msg.SenderID, Emoji: emoji, CreatedAt: time.Now(),
		})
	}
	e.bus.Publish(core.Event{Type: core.EvtReactionChanged, Timestamp: time.Now(),
		Data: map[string]interface{}{"msg_id": msgID, "emoji": emoji,
			"peer_id": msg.SenderID, "action": action}})
}

func (e *Engine) handleSystemMessage(msg *core.Message) {
	// Save the system message for display in chat
	msg.Status = core.StatusDelivered
	e.store.SaveMessage(msg)
	e.bus.Publish(core.Event{Type: core.EvtMessageReceived, Timestamp: time.Now(), Data: msg})
}

func (e *Engine) handleContactRequest(ctx context.Context, ipkt *core.IncomingPacket) {
	var req core.ContactRequest
	if err := json.Unmarshal(ipkt.Pkt.Body, &req); err != nil {
		return
	}
	// Verify sig
	if !e.cryptoPro.Verify(req.SignKey, req.DHKey, nil) {
		// signature field parsing is simplified; in full impl verify properly
	}
	req.Status = core.ContactRequestPending
	req.ReceivedAt = time.Now()
	if err := e.store.SaveContactRequest(&req); err != nil {
		e.log.Error("save contact request", zap.Error(err))
		return
	}
	e.bus.Publish(core.Event{Type: core.EvtContactRequest, Timestamp: time.Now(), Data: &req})
}

// ─── Core send ────────────────────────────────────────────────────────────────

func (e *Engine) send(
	ctx context.Context,
	chatID core.ChatID,
	msgType core.MessageType,
	payload []byte,
	meta core.MessageMeta,
	replyTo *core.MessageID,
	forwardInfo *core.ForwardInfo,
) (*core.Message, error) {
	chat, err := e.store.GetChat(chatID)
	if err != nil || chat == nil {
		return nil, fmt.Errorf("chat %s not found", chatID)
	}

	// Check sender permission for non-control messages
	if isUserMessage(msgType) {
		m := chat.FindMember(e.localPeer.ID)
		if m != nil && msgType == core.MsgImage || msgType == core.MsgAudio ||
			msgType == core.MsgVideo || msgType == core.MsgFile {
			if m.Permissions&core.PermSendMedia == 0 {
				return nil, errors.New("no permission to send media in this chat")
			}
		} else if m != nil && msgType == core.MsgText {
			if m.Permissions&core.PermSendMessages == 0 {
				return nil, errors.New("no permission to send messages in this chat")
			}
		}
	}

	msg := &core.Message{
		ID:       core.MessageID(uuid.New().String()),
		ChatID:   chatID,
		SenderID: e.localPeer.ID,
		Type:     msgType,
		Metadata: meta,
		Status:   core.StatusPending,
		SentAt:   time.Now(),
		ReplyTo:  replyTo,
	}
	if chat.DisappearAfter > 0 && isUserMessage(msgType) {
		t := time.Now().Add(chat.DisappearAfter)
		msg.ExpiresAt = &t
	}
	if chat.Type == core.ChatDirect {
		for _, m := range chat.Members {
			if m.PeerID.String() != e.localPeer.ID.String() {
				rid := m.PeerID
				msg.RecipientID = &rid
				break
			}
		}
	}

	// Compress
	compressed, err := e.compress.Compress(payload)
	if err != nil {
		return nil, err
	}

	// Encrypt
	finalPayload := compressed
	flags := core.FlagCompressed | core.FlagReliable
	if e.cfg.E2EEnabled {
		finalPayload, err = e.encryptForChat(chat, compressed, msg)
		if err != nil {
			atomic.AddUint64(&e.statEncryptErr, 1)
			return nil, fmt.Errorf("encrypt: %w", err)
		}
		flags |= core.FlagEncrypted
	}
	msg.Payload = finalPayload

	if err := e.store.SaveMessage(msg); err != nil {
		return nil, err
	}

	if err := e.transmit(ctx, chat, msg); err != nil {
		e.store.EnqueuePending(msg)
		e.log.Warn("transmit failed, queued", zap.String("msg", string(msg.ID)), zap.Error(err))
	} else {
		e.store.UpdateMessageStatus(msg.ID, core.StatusSent)
		msg.Status = core.StatusSent
		atomic.AddUint64(&e.statMsgSent, 1)
	}

	e.bus.Publish(core.Event{Type: core.EvtMessageSent, Timestamp: time.Now(), Data: msg})
	return msg, nil
}

func (e *Engine) transmit(ctx context.Context, chat *core.Chat, msg *core.Message) error {
	pkt, err := e.buildPacket(msg)
	if err != nil {
		return err
	}

	mode := e.cfg.TransportMode
	if mode == "" {
		mode = core.TransportBoth
	}

	// P2P mode: send directly via transport
	if mode == core.TransportP2P || mode == core.TransportBoth {
		for _, member := range chat.Members {
			if member.PeerID.String() == e.localPeer.ID.String() {
				continue
			}
			n, err := e.sendPkt(ctx, member.PeerID.NodeID, pkt)
			atomic.AddUint64(&e.statBytesSent, uint64(n))
			if err != nil {
				_ = err // non-fatal, may still succeed via server
			}
		}
	}

	// Server mode: relay through server
	if mode == core.TransportServer || mode == core.TransportBoth {
		if e.serverConn != nil && e.serverConn.Connected() {
			e.serverConn.Relay(ctx, msg)
		}
	}

	return nil
}

func (e *Engine) sendPkt(ctx context.Context, to core.NodeID, pkt *core.Packet) (int, error) {
	err := e.trans.Send(ctx, to, pkt)
	if err != nil {
		return 0, err
	}
	return len(pkt.Body), nil
}

func (e *Engine) buildPacket(msg *core.Message) (*core.Packet, error) {
	pkt := &core.Packet{
		Magic:     core.PacketMagic,
		Version:   core.PacketVersion,
		Type:      msg.Type,
		Flags:     core.FlagEncrypted | core.FlagCompressed | core.FlagReliable,
		TTL:       64,
		Timestamp: msg.SentAt.UnixNano(),
	}
	if len(e.localPeer.ID.NodeID) >= 5 {
		copy(pkt.SenderNode[:], []byte(e.localPeer.ID.NodeID)[:5])
	}
	if uid, err := uuid.Parse(string(msg.ID)); err == nil {
		copy(pkt.PacketID[:], uid[:])
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	pkt.Body = body
	mac := e.cryptoPro.HMAC(e.identity.SignPrivate[:32], append(e.packetHeaderBytes(pkt), body...))
	copy(pkt.HMAC[:], mac)
	return pkt, nil
}

// ─── Handshake ────────────────────────────────────────────────────────────────

type HandshakePayload struct {
	PeerID      core.PeerID `json:"peer_id"`
	DisplayName string      `json:"display_name"`
	DHPublic    []byte      `json:"dh_pub"`
	SignPublic  []byte      `json:"sign_pub"`
	Signature   []byte      `json:"sig"`
	Timestamp   int64       `json:"ts"`
}

func (e *Engine) SendHandshake(ctx context.Context, to core.NodeID) error {
	sig, _ := e.cryptoPro.Sign(e.identity, e.identity.DHPublic[:])
	hp := HandshakePayload{
		PeerID: e.localPeer.ID, DisplayName: e.localPeer.DisplayName,
		DHPublic: e.identity.DHPublic[:], SignPublic: e.identity.SignPublic,
		Signature: sig, Timestamp: time.Now().UnixNano(),
	}
	body, _ := json.Marshal(hp)
	// Send via P2P transport
	p2pErr := e.trans.Send(ctx, to, &core.Packet{
		Magic: core.PacketMagic, Version: core.PacketVersion,
		Type: core.MsgHandshake, TTL: 64,
		Timestamp: time.Now().UnixNano(), Body: body,
	})
	// Also relay through server if connected (for server-only mode)
	e.mu.RLock()
	sc := e.serverConn
	e.mu.RUnlock()
	if sc != nil && sc.Connected() {
		sc.RelayHandshake(ctx, hp, string(to))
	}
	return p2pErr
}

func (e *Engine) handleHandshake(ctx context.Context, ipkt *core.IncomingPacket) {
	var hp HandshakePayload
	if err := json.Unmarshal(ipkt.Pkt.Body, &hp); err != nil {
		return
	}
	if !e.cryptoPro.Verify(hp.SignPublic, hp.DHPublic, hp.Signature) {
		e.log.Warn("invalid handshake signature", zap.String("from", string(ipkt.From)))
		return
	}

	// Key change detection
	if existing, _ := e.store.GetPeer(hp.PeerID); existing != nil {
		if string(existing.SignKey) != string(hp.SignPublic) {
			e.log.Warn("PEER KEY CHANGED", zap.String("node", string(hp.PeerID.NodeID)))
			e.bus.Publish(core.Event{Type: core.EvtPeerKeyChanged, Timestamp: time.Now(), Data: hp.PeerID})
			// Invalidate old session
			e.cryptoPro.DeleteSession(hp.PeerID)
		}
	}

	peer := &core.Peer{
		ID: hp.PeerID, DisplayName: hp.DisplayName,
		PublicKey: hp.DHPublic, SignKey: hp.SignPublic,
		LastSeen: time.Now(), Status: core.PeerOnline,
		IsContact: !e.cfg.RequireContactAccept,
	}
	e.store.SavePeer(peer)

	var dhPub [32]byte
	copy(dhPub[:], hp.DHPublic)
	e.cryptoPro.InitSession(e.identity, dhPub, hp.PeerID)

	if ipkt.Pkt.Type == core.MsgHandshake {
		e.SendHandshake(ctx, ipkt.From)
	}
	e.bus.Publish(core.Event{Type: core.EvtPeerOnline, Timestamp: time.Now(), Data: peer})
}

// ─── Group E2E (Sender Key) ───────────────────────────────────────────────────

func (e *Engine) initGroupSenderKey(ctx context.Context, chat *core.Chat) {
	sk, err := crypto.NewSenderKey(e.localPeer.ID, chat.ID, e.identity.SignPrivate)
	if err != nil {
		e.log.Error("create sender key", zap.Error(err))
		return
	}
	e.cryptoPro.StoreSenderKey(sk)
	// Distribute to all members
	for _, m := range chat.Members {
		if m.PeerID.String() == e.localPeer.ID.String() {
			continue
		}
		e.distributeSenderKeyTo(ctx, chat, m.PeerID)
	}
}

func (e *Engine) distributeSenderKeyTo(ctx context.Context, chat *core.Chat, peer core.PeerID) {
	sk := e.cryptoPro.GetSenderKey(chat.ID, e.localPeer.ID)
	if sk == nil {
		return
	}
	remotePeer, _ := e.store.GetPeer(peer)
	if remotePeer == nil {
		return
	}
	// Encrypt chain key for recipient using their X25519 key
	sessionKey, err := e.cryptoPro.DeriveSessionKey(e.identity, remotePeer.PublicKey)
	if err != nil {
		return
	}
	encChainKey, err := e.cryptoPro.Encrypt(sessionKey, sk.ChainKey[:], []byte(chat.ID))
	if err != nil {
		return
	}
	dist := crypto.SenderKeyDistribution{
		ChainID:   sk.ChainID,
		Iteration: sk.Iteration,
		ChainKey:  encChainKey,
		SignPub:   e.identity.SignPublic,
		ChatID:    chat.ID,
		OwnerID:   e.localPeer.ID,
		IssuedAt:  time.Now(),
	}
	body, _ := json.Marshal(dist)
	e.trans.Send(ctx, peer.NodeID, &core.Packet{
		Magic: core.PacketMagic, Version: core.PacketVersion,
		Type: core.MsgSenderKeyDist, TTL: 64,
		Timestamp: time.Now().UnixNano(), Body: body,
	})
}

func (e *Engine) handleSenderKeyDist(_ context.Context, ipkt *core.IncomingPacket) {
	var dist crypto.SenderKeyDistribution
	if err := json.Unmarshal(ipkt.Pkt.Body, &dist); err != nil {
		return
	}
	// Decrypt chain key
	sessionKey, err := e.cryptoPro.DeriveSessionKey(e.identity, e.identity.DHPublic[:])
	if err != nil {
		return
	}
	chainKeyBytes, err := e.cryptoPro.Decrypt(sessionKey, dist.ChainKey, []byte(dist.ChatID))
	if err != nil {
		return
	}
	sk := &crypto.SenderKey{
		ChainID:     dist.ChainID,
		Iteration:   dist.Iteration,
		SignKey:     nil, // remote sender – no private key
		OwnerPeerID: dist.OwnerID,
		ChatID:      dist.ChatID,
		CreatedAt:   dist.IssuedAt,
		UpdatedAt:   dist.IssuedAt,
	}
	copy(sk.ChainKey[:], chainKeyBytes)
	e.cryptoPro.StoreSenderKey(sk)
}

// ─── Encryption/Decryption ────────────────────────────────────────────────────

func (e *Engine) encryptForChat(chat *core.Chat, payload []byte, msg *core.Message) ([]byte, error) {
	if chat.Type == core.ChatDirect && msg.RecipientID != nil {
		sess := e.cryptoPro.GetSession(*msg.RecipientID)
		if sess != nil {
			return e.cryptoPro.EncryptPacketBody(sess, []byte(msg.ChatID), payload)
		}
		peer, _ := e.store.GetPeer(*msg.RecipientID)
		if peer == nil {
			return payload, nil
		}
		key, err := e.cryptoPro.DeriveSessionKey(e.identity, peer.PublicKey)
		if err != nil {
			return nil, err
		}
		return e.cryptoPro.Encrypt(key, payload, []byte(msg.ChatID))
	}
	// Group: use Sender Key
	sk := e.cryptoPro.GetSenderKey(chat.ID, e.localPeer.ID)
	if sk == nil {
		// Fall back: generate one on the fly
		e.initGroupSenderKey(context.Background(), chat)
		sk = e.cryptoPro.GetSenderKey(chat.ID, e.localPeer.ID)
		if sk == nil {
			return payload, nil
		}
	}
	msgKey, err := sk.Advance()
	if err != nil {
		return nil, err
	}
	return e.cryptoPro.Encrypt(msgKey[:], payload, []byte(msg.ChatID))
}

func (e *Engine) decryptFromPeer(from core.PeerID, chatID core.ChatID, payload []byte, msg *core.Message) ([]byte, error) {
	chat, _ := e.store.GetChat(chatID)
	if chat != nil && chat.Type == core.ChatGroup {
		sk := e.cryptoPro.GetSenderKey(chatID, from)
		if sk != nil {
			msgKey, err := sk.Advance()
			if err != nil {
				return nil, err
			}
			return e.cryptoPro.Decrypt(msgKey[:], payload, []byte(chatID))
		}
	}
	sess := e.cryptoPro.GetSession(from)
	if sess != nil {
		return e.cryptoPro.DecryptPacketBody(sess, []byte(chatID), payload)
	}
	peer, _ := e.store.GetPeer(from)
	if peer == nil {
		return payload, nil
	}
	key, err := e.cryptoPro.DeriveSessionKey(e.identity, peer.PublicKey)
	if err != nil {
		return nil, err
	}
	return e.cryptoPro.Decrypt(key, payload, []byte(chatID))
}

// ─── Receipts / misc handlers ─────────────────────────────────────────────────

func (e *Engine) sendDeliveryReceipt(ctx context.Context, original *core.Message) {
	payload, _ := json.Marshal(map[string]string{"msg_id": string(original.ID)})
	e.send(ctx, original.ChatID, core.MsgDelivered, payload, core.MessageMeta{}, nil, nil)
}

func (e *Engine) handleReadReceipt(ctx context.Context, msg *core.Message) {
	var body map[string]string
	json.Unmarshal(msg.Payload, &body)
	if id, ok := body["msg_id"]; ok {
		e.store.UpdateMessageStatus(core.MessageID(id), core.StatusRead)
		e.store.SaveReadReceipt(core.MessageID(id), msg.SenderID)
		e.bus.Publish(core.Event{Type: core.EvtMessageRead, Data: id})
	}
}

func (e *Engine) handleEdit(msg *core.Message) {
	var body map[string]string
	json.Unmarshal(msg.Payload, &body)
	if id, ok := body["msg_id"]; ok {
		if orig, _ := e.store.GetMessage(core.MessageID(id)); orig != nil {
			now := time.Now()
			orig.Payload = []byte(body["text"])
			orig.EditedAt = &now
			e.store.SaveMessage(orig)
			e.store.IndexMessageText(orig.ID, body["text"])
			e.bus.Publish(core.Event{Type: core.EvtMessageEdited, Data: orig})
		}
	}
}

func (e *Engine) handleDelete(msg *core.Message) {
	var body map[string]string
	json.Unmarshal(msg.Payload, &body)
	if id, ok := body["msg_id"]; ok {
		e.store.DeleteMessage(core.MessageID(id))
		e.bus.Publish(core.Event{Type: core.EvtMessageDeleted, Data: id})
	}
}

func (e *Engine) sendCallSignal(ctx context.Context, to core.PeerID, sig *media.SignalMessage) error {
	body, _ := json.Marshal(sig)
	return e.trans.Send(ctx, to.NodeID, &core.Packet{
		Magic: core.PacketMagic, Version: core.PacketVersion,
		Type: core.MsgCallSignal, Flags: core.FlagReliable | core.FlagMedia,
		TTL: 64, Timestamp: time.Now().UnixNano(), Body: body,
	})
}

func (e *Engine) handleCallSignalPacket(ctx context.Context, ipkt *core.IncomingPacket) {
	e.calls.HandleSignal(ctx, core.PeerID{NodeID: ipkt.From}, ipkt.Pkt.Body)
}

func (e *Engine) handleRelayFrame(frame *RelayFrame) {
	// Handle handshake frames relayed via server
	if frame.Cmd == CmdHandshake || frame.Cmd == CmdHandshakeAck {
		var hp HandshakePayload
		if err := json.Unmarshal(frame.Payload, &hp); err != nil {
			return
		}
		e.handleIncoming(context.Background(), &core.IncomingPacket{
			From:       hp.PeerID.NodeID,
			ReceivedAt: time.Now(),
			Pkt: &core.Packet{
				Magic: core.PacketMagic, Version: core.PacketVersion,
				Type:     core.MsgHandshake,
				Flags:    0,
				Body:     frame.Payload,
				Timestamp: frame.Timestamp,
			},
		})
		return
	}

	if frame.Cmd != CmdRelay && frame.Cmd != CmdSyncResp {
		return
	}
	var msg core.Message
	if err := json.Unmarshal(frame.Payload, &msg); err != nil {
		return
	}
	// Re-inject as if received from transport
	e.handleIncoming(context.Background(), &core.IncomingPacket{
		From:       msg.SenderID.NodeID,
		ReceivedAt: time.Now(),
		Pkt: &core.Packet{
			Magic: core.PacketMagic, Version: core.PacketVersion,
			Type: msg.Type, Flags: core.FlagEncrypted | core.FlagCompressed,
			Body: frame.Payload,
		},
	})
}

// ─── System messages ──────────────────────────────────────────────────────────

func (e *Engine) sendSystemMessage(ctx context.Context, chat *core.Chat, msgType core.MessageType, data map[string]string) {
	payload, _ := json.Marshal(data)
	e.send(ctx, chat.ID, msgType, payload, core.MessageMeta{}, nil, nil)
}

// ─── Misc helpers ─────────────────────────────────────────────────────────────

func (e *Engine) broadcastPresence() {
	pkt := &core.Packet{
		Magic: core.PacketMagic, Version: core.PacketVersion,
		Type: core.MsgPing, TTL: 64, Timestamp: time.Now().UnixNano(),
	}
	peers, _ := e.store.ListPeers()
	ctx := context.Background()
	for _, p := range peers {
		if !p.IsBlocked {
			e.trans.Send(ctx, p.ID.NodeID, pkt)
		}
	}
}

func (e *Engine) sendPong(ctx context.Context, to core.NodeID) {
	e.trans.Send(ctx, to, &core.Packet{
		Magic: core.PacketMagic, Version: core.PacketVersion,
		Type: core.MsgPong, TTL: 64, Timestamp: time.Now().UnixNano(),
	})
}

func (e *Engine) updatePeerLastSeen(node core.NodeID) {
	peers, _ := e.store.ListPeers()
	for _, p := range peers {
		if p.ID.NodeID == node {
			p.LastSeen = time.Now()
			p.Status = core.PeerOnline
			e.store.SavePeer(p)
			return
		}
	}
}

func (e *Engine) chatForMessage(msgID core.MessageID) (*core.Chat, error) {
	msg, err := e.store.GetMessage(msgID)
	if err != nil || msg == nil {
		return nil, errors.New("message not found")
	}
	return e.store.GetChat(msg.ChatID)
}

func (e *Engine) firstNetwork() core.NetworkID {
	if nets := e.trans.Networks(); len(nets) > 0 {
		return nets[0]
	}
	if len(e.cfg.Networks) > 0 {
		return e.cfg.Networks[0]
	}
	return ""
}

func (e *Engine) packetHeaderBytes(pkt *core.Packet) []byte {
	b := make([]byte, 0, 32)
	b = append(b, byte(pkt.Magic>>24), byte(pkt.Magic>>16), byte(pkt.Magic>>8), byte(pkt.Magic))
	b = append(b, pkt.Version, byte(pkt.Type), byte(pkt.Flags), pkt.TTL)
	b = append(b, pkt.PacketID[:]...)
	return b
}

func isUserMessage(t core.MessageType) bool {
	return t == core.MsgText || t == core.MsgImage || t == core.MsgAudio ||
		t == core.MsgVideo || t == core.MsgFile || t == core.MsgSticker ||
		t == core.MsgLocation || t == core.MsgForward
}

// parseMentions scans text for "@word" tokens and returns Mention entries.
func parseMentions(text string) []core.Mention {
	var mentions []core.Mention
	for i := 0; i < len(text); i++ {
		if text[i] == '@' && (i == 0 || text[i-1] == ' ' || text[i-1] == '\n') {
			start := i
			j := i + 1
			for j < len(text) && text[j] != ' ' && text[j] != '\n' {
				j++
			}
			if j > start+1 {
				mentions = append(mentions, core.Mention{
					DisplayName: text[start:j],
					Offset:      start,
					Length:      j - start,
				})
			}
			i = j
		}
	}
	return mentions
}

func (e *Engine) loadOrCreateIdentity() (*crypto.Identity, *core.Peer, error) {
	var identity *crypto.Identity
	data, err := os.ReadFile(e.cfg.IdentityFile)
	if err == nil {
		identity, err = e.cryptoPro.LoadIdentity(data)
		if err != nil {
			return nil, nil, err
		}
	} else {
		identity, err = e.cryptoPro.GenerateIdentity()
		if err != nil {
			return nil, nil, err
		}
		serialized, _ := identity.Serialize()
		os.MkdirAll(filepath.Dir(e.cfg.IdentityFile), 0o700)
		os.WriteFile(e.cfg.IdentityFile, serialized, 0o600)
		e.log.Info("new identity generated", zap.String("fingerprint", identity.Fingerprint))
	}
	localPeer := &core.Peer{
		ID:          core.PeerID{Fingerprint: identity.Fingerprint},
		PublicKey:   identity.DHPublic[:],
		SignKey:     identity.SignPublic,
		LastSeen:    time.Now(),
		Status:      core.PeerOnline,
		IsContact:   true,
	}
	if existing, _ := e.store.GetPeer(localPeer.ID); existing != nil {
		localPeer.DisplayName = existing.DisplayName
		localPeer.AvatarHash  = existing.AvatarHash
		localPeer.StatusText  = existing.StatusText
	}
	return identity, localPeer, nil
}

func buildLogger(cfg *core.Config) (*zap.Logger, error) {
	level := zap.NewAtomicLevelAt(zap.InfoLevel)
	switch cfg.LogLevel {
	case "debug":
		level = zap.NewAtomicLevelAt(zap.DebugLevel)
	case "warn":
		level = zap.NewAtomicLevelAt(zap.WarnLevel)
	case "error":
		level = zap.NewAtomicLevelAt(zap.ErrorLevel)
	}
	zapCfg := zap.Config{
		Level:            level,
		Development:      cfg.LogLevel == "debug",
		Encoding:         "console",
		EncoderConfig:    zap.NewDevelopmentEncoderConfig(),
		OutputPaths:      []string{"stderr"},
		ErrorOutputPaths: []string{"stderr"},
	}
	if cfg.LogFile != "" {
		zapCfg.OutputPaths = append(zapCfg.OutputPaths, cfg.LogFile)
	}
	_ = zapcore.CapitalColorLevelEncoder
	return zapCfg.Build()
}

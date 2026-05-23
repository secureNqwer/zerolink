// server_relay.go – optional WebSocket relay to the messenger server.
// When connected, messages are also delivered through the server for:
//   - offline delivery (server stores messages until peer reconnects)
//   - group message fan-out (server does the broadcasting)
//   - message history sync for new devices
package messenger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/secureNqwer/zerolink/core"
)

// Re-export core types for convenience.
type RelayCommand = core.RelayCommand
type RelayFrame  = core.RelayFrame
type AuthPayload = core.AuthPayload

const (
	CmdAuth          = core.CmdAuth
	CmdAuthOK        = core.CmdAuthOK
	CmdRelay         = core.CmdRelay
	CmdSync          = core.CmdSync
	CmdSyncResp      = core.CmdSyncResp
	CmdPresence      = core.CmdPresence
	CmdPing          = core.CmdPing
	CmdPong          = core.CmdPong
	CmdError         = core.CmdError
	CmdRegister      = core.CmdRegister
	CmdLogin         = core.CmdLogin
	CmdListPeers     = core.CmdListPeers
	CmdUpdateProfile = core.CmdUpdateProfile
	CmdHandshake     = core.CmdHandshake
	CmdHandshakeAck  = core.CmdHandshakeAck
)

// serverRelay manages the WebSocket connection to the optional relay server.
type serverRelay struct {
	mu        sync.RWMutex
	addresses []string
	localID   core.PeerID
	log       *zap.Logger

	conn      *websocket.Conn
	connected bool

	sendCh  chan *RelayFrame
	stopCh  chan struct{}
	wg      sync.WaitGroup

	// OnMessage is called when the server delivers a message to us.
	OnMessage func(frame *RelayFrame)

	// Auth state
	username     string
	token        string
	registered   bool
	closeOnce    sync.Once
	responseCh   chan *RelayFrame // for synchronous request/response
}

func newServerRelay(addresses []string, localID core.PeerID, log *zap.Logger) *serverRelay {
	return &serverRelay{
		addresses:  addresses,
		localID:    localID,
		log:        log,
		sendCh:     make(chan *RelayFrame, 512),
		stopCh:     make(chan struct{}),
		responseCh: make(chan *RelayFrame, 64),
	}
}

// Connect establishes the WebSocket connection and authenticates.
// It tries each address in order, with jittered exponential backoff.
func (r *serverRelay) Connect(ctx context.Context) error {
	var lastErr error
	for _, addr := range r.addresses {
		if err := r.dial(ctx, addr); err != nil {
			r.log.Warn("relay dial failed", zap.String("addr", addr), zap.Error(err))
			lastErr = err
			continue
		}
		// Start read/write pumps
		r.wg.Add(2)
		go r.writePump()
		go r.readPump(ctx)

		// Start auto-reconnect watcher
		r.wg.Add(1)
		go r.watchdog(ctx)

		r.log.Info("connected to relay server", zap.String("addr", addr))
		return nil
	}
	return fmt.Errorf("relay: all addresses failed: %w", lastErr)
}

func (r *serverRelay) dial(ctx context.Context, addr string) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	url := "ws://" + addr + "/relay"
	conn, resp, err := dialer.DialContext(ctx, url, http.Header{
		"X-Node-ID": []string{string(r.localID.NodeID)},
	})
	if err != nil {
		return err
	}
	if resp != nil {
		resp.Body.Close()
	}

	r.mu.RLock()
	token := r.token
	username := r.username
	r.mu.RUnlock()

	// Send auth frame synchronously on connection
	authData := AuthPayload{
		NodeID:      string(r.localID.NodeID),
		Fingerprint: r.localID.Fingerprint,
		Timestamp:   time.Now().UnixNano(),
	}
	// Include username/token if we have them
	type authWithUser struct {
		AuthPayload
		Username string `json:"username,omitempty"`
		Token    string `json:"token,omitempty"`
	}
	authPayload := authWithUser{AuthPayload: authData}
	if token != "" {
		authPayload.Username = username
		authPayload.Token = token
	}
	authBytes, _ := json.Marshal(authPayload)
	frame := RelayFrame{
		Cmd:       CmdAuth,
		Payload:   authBytes,
		Timestamp: time.Now().UnixNano(),
	}
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteJSON(frame); err != nil {
		conn.Close()
		return fmt.Errorf("relay auth failed: %w", err)
	}

	// Read auth response
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var respFrame RelayFrame
	if err := conn.ReadJSON(&respFrame); err != nil {
		conn.Close()
		return fmt.Errorf("relay auth response: %w", err)
	}
	if respFrame.Cmd == CmdError {
		conn.Close()
		return fmt.Errorf("relay auth error: %s", string(respFrame.Payload))
	}

	r.mu.Lock()
	r.conn = conn
	r.connected = true
	if token != "" {
		r.registered = true
	}
	r.mu.Unlock()
	return nil
}

// watchdog reconnects when the connection drops.
func (r *serverRelay) watchdog(ctx context.Context) {
	defer r.wg.Done()
	for {
		select {
		case <-r.stopCh:
			return
		case <-time.After(5 * time.Second):
		}
		r.mu.RLock()
		ok := r.connected
		r.mu.RUnlock()
		if !ok {
			backoff := time.Duration(1+rand.Intn(5)) * time.Second
			r.log.Info("reconnecting relay", zap.Duration("backoff", backoff))
			time.Sleep(backoff)
			for _, addr := range r.addresses {
				if err := r.dial(ctx, addr); err == nil {
					r.log.Info("relay reconnected", zap.String("addr", addr))
					break
				}
			}
		}
	}
}

// Connected returns true when the relay WebSocket is active.
func (r *serverRelay) Connected() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.connected
}

// RelayHandshake forwards a handshake payload through the server.
func (r *serverRelay) RelayHandshake(ctx context.Context, hp HandshakePayload, to string) {
	payload, _ := json.Marshal(hp)
	frame := &RelayFrame{
		Cmd:       CmdHandshake,
		PeerID:    to,
		Payload:   payload,
		Timestamp: time.Now().UnixNano(),
	}
	select {
	case r.sendCh <- frame:
	default:
	}
}

// Relay forwards a message through the server.
func (r *serverRelay) Relay(ctx context.Context, msg *core.Message) {
	body, err := json.Marshal(msg)
	if err != nil {
		return
	}
	frame := &RelayFrame{
		Cmd:       CmdRelay,
		PeerID:    msg.SenderID.String(),
		Payload:   json.RawMessage(body),
		Timestamp: time.Now().UnixNano(),
	}
	select {
	case r.sendCh <- frame:
	default:
		r.log.Warn("relay send buffer full, dropping message")
	}
}

// SyncHistory asks the server to deliver messages we missed while offline.
// afterTS is a Unix nano timestamp; the server returns messages newer than it.
func (r *serverRelay) SyncHistory(chatID core.ChatID, afterTS int64) {
	payload, _ := json.Marshal(map[string]interface{}{
		"chat_id":  string(chatID),
		"after_ts": afterTS,
	})
	frame := &RelayFrame{
		Cmd:       CmdSync,
		Payload:   payload,
		Timestamp: time.Now().UnixNano(),
	}
	select {
	case r.sendCh <- frame:
	default:
	}
}

// ─── I/O pumps ────────────────────────────────────────────────────────────────

func (r *serverRelay) writePump() {
	defer r.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopCh:
			r.mu.RLock()
			conn := r.conn
			r.mu.RUnlock()
			if conn != nil {
				conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			}
			return
		case frame := <-r.sendCh:
			r.mu.RLock()
			conn := r.conn
			r.mu.RUnlock()
			if conn == nil {
				continue
			}
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteJSON(frame); err != nil {
				r.log.Warn("relay write error", zap.Error(err))
				r.markDisconnected()
			}
		case <-ticker.C:
			r.mu.RLock()
			conn := r.conn
			r.mu.RUnlock()
			if conn != nil {
				conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				conn.WriteMessage(websocket.PingMessage, nil)
			}
		}
	}
}

func (r *serverRelay) readPump(ctx context.Context) {
	defer r.wg.Done()
	for {
		r.mu.RLock()
		conn := r.conn
		r.mu.RUnlock()
		if conn == nil {
			select {
			case <-r.stopCh:
				return
			case <-time.After(time.Second):
				continue
			}
		}
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		conn.SetPongHandler(func(string) error {
			conn.SetReadDeadline(time.Now().Add(60 * time.Second))
			return nil
		})
		var frame RelayFrame
		if err := conn.ReadJSON(&frame); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				r.log.Warn("relay read error", zap.Error(err))
			}
			r.markDisconnected()
			select {
			case <-r.stopCh:
				return
			default:
				continue
			}
		}
		// Dispatch to response channel if it matches a pending request
		select {
		case r.responseCh <- &frame:
		default:
		}
		if r.OnMessage != nil {
			r.OnMessage(&frame)
		}
	}
}

func (r *serverRelay) markDisconnected() {
	r.mu.Lock()
	r.connected = false
	if r.conn != nil {
		r.conn.Close()
		r.conn = nil
	}
	r.mu.Unlock()
}

// ─── Auth / Account methods ─────────────────────────────────────────────────

// Register creates a new account on the server.
func (r *serverRelay) Register(ctx context.Context, username, password, nickname, bio, avatarPath string) (*RelayFrame, error) {
	if !r.Connected() {
		return nil, errors.New("not connected")
	}
	payload, _ := json.Marshal(map[string]string{
		"username":    username,
		"password":    password,
		"nickname":    nickname,
		"bio":         bio,
		"avatar_path": avatarPath,
	})
	frame := &RelayFrame{
		Cmd:       CmdRegister,
		Payload:   payload,
		Timestamp: time.Now().UnixNano(),
	}
	select {
	case r.sendCh <- frame:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Wait for response
	select {
	case resp := <-r.responseCh:
		if resp.Cmd == CmdAuthOK {
			// Parse token
			var data map[string]string
			json.Unmarshal(resp.Payload, &data)
			if t, ok := data["token"]; ok {
				r.mu.Lock()
				r.token = t
				r.username = data["username"]
				r.registered = true
				r.mu.Unlock()
			}
			return resp, nil
		}
		return resp, fmt.Errorf("register failed: %s", string(resp.Payload))
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Second):
		return nil, errors.New("register timeout")
	}
}

// Login authenticates with an existing account.
func (r *serverRelay) Login(ctx context.Context, username, password string) (*RelayFrame, error) {
	if !r.Connected() {
		return nil, errors.New("not connected")
	}
	payload, _ := json.Marshal(map[string]string{
		"username": username,
		"password": password,
	})
	frame := &RelayFrame{
		Cmd:       CmdLogin,
		Payload:   payload,
		Timestamp: time.Now().UnixNano(),
	}
	select {
	case r.sendCh <- frame:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	select {
	case resp := <-r.responseCh:
		if resp.Cmd == CmdAuthOK {
			var data map[string]string
			json.Unmarshal(resp.Payload, &data)
			if t, ok := data["token"]; ok {
				r.mu.Lock()
				r.token = t
				r.username = data["username"]
				r.registered = true
				r.mu.Unlock()
			}
			return resp, nil
		}
		return resp, fmt.Errorf("login failed: %s", string(resp.Payload))
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Second):
		return nil, errors.New("login timeout")
	}
}

// ListServerPeers requests the list of registered users from the server.
func (r *serverRelay) ListServerPeers(ctx context.Context) (*RelayFrame, error) {
	if !r.Connected() {
		return nil, errors.New("not connected")
	}
	frame := &RelayFrame{
		Cmd:       CmdListPeers,
		Timestamp: time.Now().UnixNano(),
	}
	select {
	case r.sendCh <- frame:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	select {
	case resp := <-r.responseCh:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Second):
		return nil, errors.New("list peers timeout")
	}
}

// UpdateProfile updates the user's profile on the server.
func (r *serverRelay) UpdateProfile(ctx context.Context, nickname, bio, avatarPath string) error {
	if !r.Connected() {
		return errors.New("not connected")
	}
	payload, _ := json.Marshal(map[string]string{
		"nickname":    nickname,
		"bio":         bio,
		"avatar_path": avatarPath,
	})
	frame := &RelayFrame{
		Cmd:       CmdUpdateProfile,
		Payload:   payload,
		Timestamp: time.Now().UnixNano(),
	}
	select {
	case r.sendCh <- frame:
	default:
		return errors.New("send buffer full")
	}
	return nil
}

// Username returns the authenticated username.
func (r *serverRelay) Username() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.username
}

// Token returns the auth token.
func (r *serverRelay) Token() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.token
}

// SetAuth sets the username and token for subsequent connections.
func (r *serverRelay) SetAuth(username, token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.username = username
	r.token = token
}

// Address returns the first server address.
func (r *serverRelay) Address() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.addresses) == 0 {
		return ""
	}
	return r.addresses[0]
}

// IsRegistered returns whether the client has an account linked.
func (r *serverRelay) IsRegistered() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.registered
}

// Close shuts down the relay connection.
func (r *serverRelay) Close() {
	r.closeOnce.Do(func() {
		close(r.stopCh)
	})
	r.markDisconnected()
	r.wg.Wait()
}

// FetchMedia downloads a media blob by hash from the relay server CDN.
// Returns the raw bytes or an error if the server is unreachable or the hash is unknown.
func (r *serverRelay) FetchMedia(ctx context.Context, hash string) ([]byte, error) {
	r.mu.RLock()
	connected := r.connected
	r.mu.RUnlock()
	if !connected || len(r.addresses) == 0 {
		return nil, errors.New("relay: not connected")
	}

	url := "http://" + r.addresses[0] + "/media/" + hash
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New("relay: media not found")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("relay: server returned %d", resp.StatusCode)
	}
	data := make([]byte, 0, 4096)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		data = append(data, buf[:n]...)
		if err != nil {
			break
		}
	}
	return data, nil
}

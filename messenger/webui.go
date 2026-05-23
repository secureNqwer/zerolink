package messenger

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/secureNqwer/zerolink/core"
)

//go:embed index.html
var indexHTML string

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
	ReadBufferSize: 4096,
	WriteBufferSize: 4096,
}

const authFile = "auth.json"

type authData struct {
	ServerAddr string `json:"server_addr"`
	Username   string `json:"username"`
	Token      string `json:"token"`
}

func loadAuth() *authData {
	data, err := os.ReadFile(authFile)
	if err != nil {
		return nil
	}
	var a authData
	if err := json.Unmarshal(data, &a); err != nil {
		return nil
	}
	return &a
}

func saveAuth(a *authData) {
	if a == nil {
		os.Remove(authFile)
		return
	}
	data, _ := json.Marshal(a)
	os.WriteFile(authFile, data, 0o600)
}

type WebUIClient struct {
	conn   *websocket.Conn
	sendCh chan []byte
	user   string
}

type WebUI struct {
	engine   *Engine
	log      *zap.Logger
	mu       sync.RWMutex
	clients  map[string]*WebUIClient
	onShutdown func()
}

func NewWebUI(engine *Engine) *WebUI {
	return &WebUI{
		engine:  engine,
		log:     engine.log.Named("webui"),
		clients: make(map[string]*WebUIClient),
	}
}

func (w *WebUI) SetShutdownHandler(fn func()) { w.onShutdown = fn }

func (w *WebUI) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", w.serveIndex)
	mux.HandleFunc("/api/server/connect", w.handleServerConnect)
	mux.HandleFunc("/api/auth/status", w.handleAuthStatus)
	mux.HandleFunc("/api/profile/update", w.handleProfileUpdate)
	mux.HandleFunc("/api/logout", w.handleLogout)
	mux.HandleFunc("/api/login", w.handleLogin)
	mux.HandleFunc("/api/register", w.handleRegister)
	mux.HandleFunc("/api/peers", w.handlePeers)
	mux.HandleFunc("/api/chats", w.handleChats)
	mux.HandleFunc("/api/messages", w.handleMessages)
	mux.HandleFunc("/api/send", w.handleSend)
	mux.HandleFunc("/api/settings", w.handleSettings)
	mux.HandleFunc("/api/dm", w.handleDM)
	mux.HandleFunc("/api/profile", w.handleProfile)
	mux.HandleFunc("/api/upload", w.handleUpload)
	mux.HandleFunc("/api/shutdown", w.handleShutdown)
	mux.HandleFunc("/ws", w.handleWS)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("./webui"))))
	return mux
}

func (w *WebUI) serveIndex(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Write([]byte(indexHTML))
}

func (w *WebUI) handleServerConnect(rw http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(rw, "POST required", 405)
		return
	}
	var req struct {
		Server string `json:"server"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(map[string]string{"error": "Invalid request payload"})
		return
	}
	req.Server = strings.TrimSpace(req.Server)
	if req.Server == "" {
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(map[string]string{"error": "Server address is required"})
		return
	}
	if !strings.Contains(req.Server, ":") {
		req.Server = req.Server + ":8080"
	}
	w.log.Info("Connecting to server relay", zap.String("address", req.Server))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := w.engine.ConnectToServer(ctx, req.Server)
	rw.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.log.Error("Failed to connect to server", zap.Error(err))
		json.NewEncoder(rw).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(rw).Encode(map[string]string{"ok": "true"})
}

func (w *WebUI) handleAuthStatus(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	sr := w.engine.ServerRelay()
	connected := sr != nil && sr.Connected()
	authenticated := sr != nil && sr.IsRegistered() && sr.Token() != ""
	
	nodeID := ""
	if w.engine.LocalPeer() != nil {
		nodeID = string(w.engine.LocalPeer().ID.NodeID)
	}
	
	username := ""
	token := ""
	if sr != nil {
		username = sr.Username()
		token = sr.Token()
	}
	
	nickname := ""
	bio := ""
	avatar := ""
	
	if w.engine.LocalPeer() != nil {
		nickname = w.engine.LocalPeer().DisplayName
		bio = w.engine.LocalPeer().StatusText
	}
	
	json.NewEncoder(rw).Encode(map[string]interface{}{
		"connected":     connected,
		"authenticated": authenticated,
		"username":      username,
		"token":         token,
		"node_id":       nodeID,
		"nickname":      nickname,
		"bio":           bio,
		"avatar":        avatar,
	})
}

func (w *WebUI) handleProfileUpdate(rw http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(rw, "POST required", 405)
		return
	}
	var req struct {
		Nickname string `json:"nickname"`
		Bio      string `json:"bio"`
		Avatar   string `json:"avatar"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(map[string]string{"error": "Invalid payload"})
		return
	}
	
	if err := w.engine.SetDisplayName(req.Nickname); err != nil {
		w.log.Warn("Failed to update local display name", zap.Error(err))
	}
	if err := w.engine.SetStatus(core.PeerOnline, req.Bio); err != nil {
		w.log.Warn("Failed to update local bio status", zap.Error(err))
	}
	
	sr := w.engine.ServerRelay()
	if sr != nil && sr.Connected() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := sr.UpdateProfile(ctx, req.Nickname, req.Bio, req.Avatar); err != nil {
			w.log.Error("Failed to update server profile", zap.Error(err))
			rw.Header().Set("Content-Type", "application/json")
			json.NewEncoder(rw).Encode(map[string]string{"error": "Failed to update server profile: " + err.Error()})
			return
		}
	}
	
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(map[string]string{"ok": "true"})
}

func (w *WebUI) handleLogout(rw http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(rw, "POST required", 405)
		return
	}
	saveAuth(nil)
	w.engine.DisconnectServer()
	
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(map[string]string{"ok": "true"})
}

func (w *WebUI) handleLogin(rw http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(rw, "POST required", 405)
		return
	}
	var req struct {
		Server   string `json:"server"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	
	if !strings.Contains(req.Server, ":") {
		req.Server = req.Server + ":8080"
	}
	
	result, err := httpPost("http://"+req.Server+"/auth/login", map[string]string{
		"username": req.Username, "password": req.Password,
	})
	if err != nil {
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(map[string]string{"error": err.Error()})
		return
	}
	
	rw.Header().Set("Content-Type", "application/json")
	if result["ok"] == "true" {
		token := result["token"]
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		
		w.engine.DisconnectServer()
		
		if err := w.engine.ConnectToServer(ctx, req.Server, req.Username, token); err != nil {
			json.NewEncoder(rw).Encode(map[string]string{"error": "Login success, but engine connection failed: " + err.Error()})
			return
		}
		
		if nickname, exists := result["nickname"]; exists && nickname != "" {
			w.engine.SetDisplayName(nickname)
		}
		if bio, exists := result["bio"]; exists && bio != "" {
			w.engine.SetStatus(core.PeerOnline, bio)
		}
		
		saveAuth(&authData{ServerAddr: req.Server, Username: req.Username, Token: token})
	}
	json.NewEncoder(rw).Encode(result)
}

func (w *WebUI) handleRegister(rw http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(rw, "POST required", 405)
		return
	}
	var req struct {
		Server   string `json:"server"`
		Username string `json:"username"`
		Password string `json:"password"`
		Nickname string `json:"nickname"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	
	if !strings.Contains(req.Server, ":") {
		req.Server = req.Server + ":8080"
	}
	
	result, err := httpPost("http://"+req.Server+"/auth/register", map[string]string{
		"username": req.Username, "password": req.Password,
		"nickname": req.Nickname, "bio": "", "avatar_path": "",
	})
	if err != nil {
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(map[string]string{"error": err.Error()})
		return
	}
	
	rw.Header().Set("Content-Type", "application/json")
	if result["ok"] == "true" {
		token := result["token"]
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		
		w.engine.DisconnectServer()
		
		if err := w.engine.ConnectToServer(ctx, req.Server, req.Username, token); err != nil {
			json.NewEncoder(rw).Encode(map[string]string{"error": "Registration success, but engine connection failed: " + err.Error()})
			return
		}
		
		if req.Nickname != "" {
			w.engine.SetDisplayName(req.Nickname)
		}
		
		saveAuth(&authData{ServerAddr: req.Server, Username: req.Username, Token: token})
	}
	json.NewEncoder(rw).Encode(result)
}

func (w *WebUI) handlePeers(rw http.ResponseWriter, r *http.Request) {
	if w.engine.ServerRelay() == nil || !w.engine.ServerRelay().Connected() {
		json.NewEncoder(rw).Encode(map[string]interface{}{"peers": []interface{}{}})
		return
	}
	resp, err := httpGet("http://" + w.engine.ServerRelay().Address() + "/peers")
	if err != nil {
		json.NewEncoder(rw).Encode(map[string]interface{}{"error": err.Error()})
		return
	}
	var data map[string]interface{}
	json.Unmarshal(resp, &data)
	json.NewEncoder(rw).Encode(data)
}

func (w *WebUI) handleChats(rw http.ResponseWriter, r *http.Request) {
	chats, _ := w.engine.ListChats()
	// Enrich DM chats with the other member's display name
	allPeers, _ := w.engine.ListPeers()
	peerMap := make(map[string]*core.Peer)
	for _, p := range allPeers {
		peerMap[string(p.ID.NodeID)] = p
	}
	for i := range chats {
		if chats[i].Type == core.ChatDirect && chats[i].Name == "" {
			for _, m := range chats[i].Members {
				if m.PeerID.NodeID != w.engine.LocalPeer().ID.NodeID {
					if p, ok := peerMap[string(m.PeerID.NodeID)]; ok && p.DisplayName != "" {
						chats[i].Name = p.DisplayName
					} else {
						// Fallback: show the first part of the fingerprint as a readable identifier
						chats[i].Name = string(m.PeerID.NodeID)
					}
					break
				}
			}
		}
	}
	json.NewEncoder(rw).Encode(chats)
}

func (w *WebUI) handleMessages(rw http.ResponseWriter, r *http.Request) {
	chatID := core.ChatID(r.URL.Query().Get("chat_id"))
	if chatID == "" {
		json.NewEncoder(rw).Encode([]interface{}{})
		return
	}
	msgs, _ := w.engine.GetMessages(chatID, 100, 0)
	// Reverse for chronological order
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	json.NewEncoder(rw).Encode(msgs)
}

func (w *WebUI) handleSend(rw http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(rw, "POST required", 405)
		return
	}
	var req struct {
		ChatID string `json:"chat_id"`
		Text   string `json:"text"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	msg, err := w.engine.SendText(context.Background(), core.ChatID(req.ChatID), req.Text, nil)
	if err != nil {
		json.NewEncoder(rw).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(rw).Encode(map[string]interface{}{"ok": true, "msg_id": string(msg.ID)})
}

func (w *WebUI) handleSettings(rw http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		mode := w.engine.GetTransportMode()
		sr := w.engine.ServerRelay()
		connected := sr != nil && sr.Connected()
		user := ""
		if sr != nil {
			user = sr.Username()
		}
		json.NewEncoder(rw).Encode(map[string]interface{}{
			"transport_mode": mode,
			"connected":      connected,
			"username":       user,
		})
		return
	}
	var req struct {
		Mode string `json:"transport_mode"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	w.engine.SetTransportMode(core.TransportMode(req.Mode))
	json.NewEncoder(rw).Encode(map[string]string{"ok": "true"})
}

func (w *WebUI) handleDM(rw http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(rw, "POST required", 405)
		return
	}
	var req struct {
		NodeID      string `json:"node_id"`
		Fingerprint string `json:"fingerprint"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	peer := core.PeerID{NodeID: core.NodeID(req.NodeID), Fingerprint: req.Fingerprint}

	// Find existing DM with this peer
	chats, _ := w.engine.ListChats()
	for _, c := range chats {
		if c.Type != core.ChatDirect {
			continue
		}
		for _, m := range c.Members {
			if m.PeerID.String() == peer.String() {
				// Existing chat found
				json.NewEncoder(rw).Encode(map[string]interface{}{"ok": true, "chat_id": c.ID, "existing": true})
				return
			}
		}
	}

	// Create new chat
	chat, err := w.engine.CreateChat("", []core.PeerID{peer})
	if err != nil {
		json.NewEncoder(rw).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.engine.SendHandshake(context.Background(), peer.NodeID)
	json.NewEncoder(rw).Encode(map[string]interface{}{"ok": true, "chat_id": chat.ID})
}

func (w *WebUI) handleUpload(rw http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(rw, "POST required", 405)
		return
	}
	r.ParseMultipartForm(50 << 20) // 50MB max
	file, _, err := r.FormFile("file")
	if err != nil {
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(map[string]string{"error": "No file provided"})
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(map[string]string{"error": "Failed to read file"})
		return
	}

	chatID := core.ChatID(r.FormValue("chat_id"))
	if chatID == "" {
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(map[string]string{"error": "chat_id required"})
		return
	}

	fileName := r.FormValue("name")
	if fileName == "" {
		fileName = "file"
	}

	msg, err := w.engine.SendFile(context.Background(), chatID, fileName, data)
	if err != nil {
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(map[string]string{"error": err.Error()})
		return
	}

	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(map[string]interface{}{"ok": true, "msg_id": string(msg.ID)})
}

func (w *WebUI) handleShutdown(rw http.ResponseWriter, r *http.Request) {
	json.NewEncoder(rw).Encode(map[string]string{"ok": "true"})
	if w.onShutdown != nil {
		go w.onShutdown()
	}
}

func (w *WebUI) handleProfile(rw http.ResponseWriter, r *http.Request) {
	p := w.engine.LocalPeer()
	json.NewEncoder(rw).Encode(p)
}

func (w *WebUI) handleWS(rw http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(rw, r, nil)
	if err != nil {
		return
	}
	c := &WebUIClient{
		conn:   conn,
		sendCh: make(chan []byte, 64),
	}
	id := uuid.New().String()
	w.mu.Lock()
	w.clients[id] = c
	w.mu.Unlock()

	go c.writePump()
	c.readPump(w, id)
}

func (c *WebUIClient) writePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case msg, ok := <-c.sendCh:
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			c.conn.WriteMessage(websocket.TextMessage, msg)
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			c.conn.WriteMessage(websocket.PingMessage, nil)
		}
	}
}

func (c *WebUIClient) readPump(w *WebUI, id string) {
	defer func() {
		w.mu.Lock()
		delete(w.clients, id)
		w.mu.Unlock()
		c.conn.Close()
	}()
	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
	}
}

// BroadcastLoop subscribes to engine events and forwards them to all web UI clients
func (w *WebUI) BroadcastLoop(bus core.EventBus) {
	ch := bus.Subscribe("webui", core.EvtMessageReceived, core.EvtPeerOnline, core.EvtPeerOffline)
	for evt := range ch {
		w.BroadcastEvent(evt)
	}
}

// BroadcastEvent sends an event to all connected web UI clients
func (w *WebUI) BroadcastEvent(evt core.Event) {
	data, _ := json.Marshal(evt)
	w.mu.RLock()
	defer w.mu.RUnlock()
	for _, c := range w.clients {
		select {
		case c.sendCh <- data:
		default:
		}
	}
}

// HTTP helpers
func httpPost(url string, data map[string]string) (map[string]string, error) {
	body, _ := json.Marshal(data)
	resp, err := http.Post(url, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	strResult := make(map[string]string)
	for k, v := range result {
		strResult[k] = fmt.Sprintf("%v", v)
	}
	return strResult, nil
}

func httpGet(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	io.Copy(&buf, resp.Body)
	return buf.Bytes(), nil
}


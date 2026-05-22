package messenger

import (
	"bytes"
	"context"
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
	"github.com/secureNqwer/zerolink/version"
)

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
	chat, err := w.engine.CreateChat("", []core.PeerID{peer})
	if err != nil {
		json.NewEncoder(rw).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.engine.SendHandshake(context.Background(), peer.NodeID)
	json.NewEncoder(rw).Encode(map[string]interface{}{"ok": true, "chat_id": chat.ID})
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

var indexHTML = `<!DOCTYPE html>
<html lang="ru">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>` + version.Name + `</title>
<style>
@import url('https://fonts.googleapis.com/css2?family=Inter:wght@300;400;500;600&display=swap');

* { margin:0; padding:0; box-sizing:border-box; }
:root {
  --bg-dark: #0e1621;
  --bg-sidebar: #182533;
  --bg-active: #2b5278;
  --bg-hover: #202b36;
  --bg-msg-own: #8774e1;
  --bg-msg-other: #182533;
  --text-main: #f5f6f7;
  --text-sec: #7f91a4;
  --accent: #8774e1;
  --accent-hover: #7662d9;
  --border: #101921;
  --danger: #e53935;
  --success: #00e676;
}

body {
  font-family: 'Inter', -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
  background: var(--bg-dark);
  color: var(--text-main);
  height: 100vh;
  overflow: hidden;
}

.hidden { display: none !important; }

/* Scrollbars */
::-webkit-scrollbar { width: 6px; height: 6px; }
::-webkit-scrollbar-track { background: transparent; }
::-webkit-scrollbar-thumb { background: #2b394a; border-radius: 3px; }
::-webkit-scrollbar-thumb:hover { background: #3e5066; }

/* Auth containers (Connect, Login/Register) */
.auth-container {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  min-height: 100vh;
  padding: 20px;
  background: var(--bg-dark);
}

.auth-card {
  width: 100%;
  max-width: 420px;
  background: var(--bg-sidebar);
  border-radius: 16px;
  padding: 36px;
  box-shadow: 0 10px 30px rgba(0,0,0,0.4);
  border: 1px solid rgba(255,255,255,0.05);
  animation: fadeIn 0.3s ease-out;
}

@keyframes fadeIn {
  from { opacity: 0; transform: translateY(10px); }
  to { opacity: 1; transform: translateY(0); }
}

.auth-card h1 {
  font-size: 28px;
  font-weight: 600;
  text-align: center;
  margin-bottom: 8px;
  color: var(--text-main);
}

.auth-card h1 span {
  color: var(--accent);
}

.auth-card p {
  color: var(--text-sec);
  text-align: center;
  font-size: 14px;
  margin-bottom: 24px;
}

/* Input Fields styling */
.input-wrapper {
  position: relative;
  width: 100%;
  margin-bottom: 14px;
}

.auth-card input, .settings-modal input, .settings-modal textarea {
  width: 100%;
  padding: 12px 16px;
  border-radius: 8px;
  border: 1px solid #202b36;
  background: var(--bg-dark);
  color: var(--text-main);
  font-size: 14px;
  outline: none;
  transition: border-color 0.2s, box-shadow 0.2s;
}

.auth-card input:focus, .settings-modal input:focus, .settings-modal textarea:focus {
  border-color: var(--accent);
  box-shadow: 0 0 0 2px rgba(135,116,225,0.25);
}

/* Password Toggle Buttons */
.password-toggle {
  position: absolute;
  right: 14px;
  top: 50%;
  transform: translateY(-50%);
  background: none;
  border: none;
  color: var(--text-sec);
  cursor: pointer;
  font-size: 16px;
  outline: none;
  display: flex;
  align-items: center;
  justify-content: center;
}

/* Action Buttons */
.btn-primary {
  width: 100%;
  padding: 12px;
  border-radius: 8px;
  border: none;
  background: var(--accent);
  color: #fff;
  font-size: 15px;
  font-weight: 500;
  cursor: pointer;
  transition: background-color 0.2s, transform 0.1s;
  outline: none;
  margin-top: 8px;
}

.btn-primary:hover {
  background: var(--accent-hover);
}

.btn-primary:active {
  transform: scale(0.98);
}

.btn-secondary {
  width: 100%;
  padding: 12px;
  border-radius: 8px;
  border: 1px solid var(--accent);
  background: transparent;
  color: var(--accent);
  font-size: 15px;
  font-weight: 500;
  cursor: pointer;
  transition: background-color 0.2s, color 0.2s;
  outline: none;
}

.btn-secondary:hover {
  background: rgba(135,116,225,0.08);
}

.toggle-link {
  text-align: center;
  color: var(--text-sec);
  cursor: pointer;
  margin-top: 16px;
  font-size: 13.5px;
  transition: color 0.2s;
}

.toggle-link:hover {
  color: var(--accent);
}

.error-msg {
  color: var(--danger);
  font-size: 13px;
  text-align: center;
  margin-top: 10px;
  min-height: 18px;
}

/* Connection logs panel */
.logs-container {
  width: 100%;
  background: #0b0f19;
  border-radius: 8px;
  padding: 12px;
  margin-top: 16px;
  font-family: monospace;
  font-size: 11.5px;
  color: #a0a5b5;
  max-height: 150px;
  overflow-y: auto;
  border: 1px solid #1c2331;
}

.log-line {
  margin-bottom: 4px;
  white-space: pre-wrap;
  line-height: 1.4;
}

.log-line.error {
  color: var(--danger);
}

/* Authentication Screen Tab system */
.auth-tabs {
  display: flex;
  margin-bottom: 24px;
  border-bottom: 2px solid #202b36;
}

.auth-tab {
  flex: 1;
  text-align: center;
  padding: 10px;
  cursor: pointer;
  color: var(--text-sec);
  font-weight: 500;
  font-size: 14.5px;
  transition: color 0.2s;
}

.auth-tab.active {
  color: var(--accent);
  border-bottom: 2px solid var(--accent);
  margin-bottom: -2px;
}

/* Application Layout (Telegram Theme) */
#app {
  display: flex;
  height: 100vh;
  width: 100vw;
  background: var(--bg-dark);
}

/* Sidebar pane */
#sidebar {
  width: 350px;
  background: var(--bg-sidebar);
  border-right: 1px solid var(--border);
  display: flex;
  flex-direction: column;
  z-index: 10;
}

#sidebar-header {
  padding: 16px 20px;
  border-bottom: 1px solid var(--border);
  display: flex;
  justify-content: space-between;
  align-items: center;
}

#sidebar-header h2 {
  font-size: 18px;
  font-weight: 600;
}

#sidebar-header .status-indicator {
  font-size: 12px;
  color: var(--text-sec);
  display: flex;
  align-items: center;
  gap: 6px;
  margin-top: 2px;
}

#sidebar-header .status-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  display: inline-block;
}

#sidebar-header .status-dot.connected { background: var(--success); }
#sidebar-header .status-dot.disconnected { background: var(--danger); }

.sidebar-actions {
  display: flex;
  gap: 8px;
}

.sidebar-actions button {
  background: none;
  border: none;
  color: var(--text-sec);
  cursor: pointer;
  font-size: 18px;
  padding: 6px;
  border-radius: 8px;
  transition: background-color 0.2s, color 0.2s;
}

.sidebar-actions button:hover {
  background: var(--bg-hover);
  color: var(--accent);
}

.sidebar-actions button.logout:hover {
  color: var(--danger);
}

/* Search inputs */
.search-wrapper {
  padding: 12px 20px;
  border-bottom: 1px solid var(--border);
}

.search-wrapper input {
  width: 100%;
  padding: 8px 12px;
  border-radius: 8px;
  background: var(--bg-dark);
  color: var(--text-main);
  border: 1px solid var(--border);
  font-size: 13.5px;
  outline: none;
}

.search-wrapper input:focus {
  border-color: var(--accent);
}

/* Active Chats list */
#chat-list {
  flex: 1;
  overflow-y: auto;
  padding: 8px;
}

.chat-item {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 10px 12px;
  margin: 2px 0;
  border-radius: 10px;
  cursor: pointer;
  transition: background-color 0.15s;
}

.chat-item:hover {
  background: var(--bg-hover);
}

.chat-item.active {
  background: var(--bg-active);
}

.chat-item-info {
  flex: 1;
  min-width: 0;
}

.chat-item-name {
  font-size: 14.5px;
  font-weight: 500;
  margin-bottom: 3px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.chat-item-meta {
  font-size: 12px;
  color: var(--text-sec);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

/* Main chat workspace */
#main {
  flex: 1;
  display: flex;
  flex-direction: column;
  background: var(--bg-dark);
}

#main-header {
  padding: 14px 24px;
  border-bottom: 1px solid var(--border);
  display: flex;
  justify-content: space-between;
  align-items: center;
  background: var(--bg-sidebar);
  height: 57px;
}

#main-header-info h3 {
  font-size: 15.5px;
  font-weight: 600;
}

#main-header-info p {
  font-size: 12px;
  color: var(--text-sec);
  margin-top: 2px;
}

#messages {
  flex: 1;
  overflow-y: auto;
  padding: 24px;
  display: flex;
  flex-direction: column;
  gap: 10px;
  background-image: linear-gradient(135deg, #0e1621 0%, #15202d 100%);
}

/* Message bubbles styling */
.msg-wrapper {
  display: flex;
  width: 100%;
}

.msg-wrapper.own {
  justify-content: flex-end;
}

.msg-wrapper.other {
  justify-content: flex-start;
}

.msg {
  max-width: 65%;
  padding: 10px 14px 8px 14px;
  border-radius: 14px;
  font-size: 14.2px;
  line-height: 1.45;
  word-break: break-word;
  box-shadow: 0 1px 3px rgba(0,0,0,0.15);
  position: relative;
  display: flex;
  flex-direction: column;
}

.msg.own {
  background: var(--bg-msg-own);
  color: #fff;
  border-bottom-right-radius: 3px;
  animation: slideOwn 0.2s ease-out;
}

.msg.other {
  background: var(--bg-msg-other);
  color: var(--text-main);
  border-bottom-left-radius: 3px;
  animation: slideOther 0.2s ease-out;
}

@keyframes slideOwn {
  from { opacity: 0; transform: translate(15px, 5px); }
  to { opacity: 1; transform: translate(0, 0); }
}

@keyframes slideOther {
  from { opacity: 0; transform: translate(-15px, 5px); }
  to { opacity: 1; transform: translate(0, 0); }
}

.msg-sender {
  font-size: 12px;
  font-weight: 600;
  margin-bottom: 4px;
  color: #bfa9ff;
}

.msg-text {
  margin-bottom: 2px;
}

.msg-time {
  font-size: 10.5px;
  margin-top: 4px;
  text-align: right;
  align-self: flex-end;
  color: rgba(255,255,255,0.65);
}

.msg.other .msg-time {
  color: var(--text-sec);
}

/* Chat Input Bar */
#input-area {
  padding: 16px 24px;
  border-top: 1px solid var(--border);
  background: var(--bg-sidebar);
  display: flex;
  gap: 12px;
  align-items: center;
}

#msg-input {
  flex: 1;
  padding: 12px 18px;
  border: none;
  border-radius: 22px;
  background: var(--bg-dark);
  color: var(--text-main);
  font-size: 14px;
  outline: none;
  transition: box-shadow 0.2s;
  border: 1px solid var(--border);
}

#msg-input:focus {
  border-color: rgba(135,116,225,0.4);
  box-shadow: 0 0 0 2px rgba(135,116,225,0.15);
}

#send-btn {
  width: 44px;
  height: 44px;
  border: none;
  border-radius: 50%;
  background: var(--accent);
  color: #fff;
  cursor: pointer;
  display: flex;
  align-items: center;
  justify-content: center;
  transition: background-color 0.2s, transform 0.1s;
}

#send-btn:hover {
  background: var(--accent-hover);
}

#send-btn:active {
  transform: scale(0.92);
}

#send-btn svg {
  width: 20px;
  height: 20px;
  fill: currentColor;
  margin-left: 2px;
}

/* Users Panel (Right panel) */
#peers-panel {
  width: 320px;
  background: var(--bg-sidebar);
  border-left: 1px solid var(--border);
  display: flex;
  flex-direction: column;
  z-index: 9;
  animation: slideLeft 0.25s ease-out;
}

@keyframes slideLeft {
  from { transform: translateX(320px); }
  to { transform: translateX(0); }
}

#peers-panel h3 {
  padding: 16px 20px;
  border-bottom: 1px solid var(--border);
  font-size: 15px;
  font-weight: 600;
}

#peer-list {
  flex: 1;
  overflow-y: auto;
  padding: 8px;
}

.peer-item {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 10px 12px;
  margin: 2px 0;
  border-radius: 10px;
  cursor: pointer;
  transition: background-color 0.15s;
}

.peer-item:hover {
  background: var(--bg-hover);
}

.peer-status-marker {
  position: relative;
}

.peer-status-dot {
  position: absolute;
  bottom: 0;
  right: 0;
  width: 10px;
  height: 10px;
  border-radius: 50%;
  border: 2px solid var(--bg-sidebar);
}

.peer-status-dot.online { background: var(--success); }
.peer-status-dot.offline { background: var(--text-sec); }

.peer-item-info {
  flex: 1;
  min-width: 0;
}

.peer-item-name {
  font-size: 14px;
  font-weight: 500;
  margin-bottom: 2px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.peer-item-meta {
  font-size: 11px;
  color: var(--text-sec);
}

.peer-settings-bar {
  padding: 16px 20px;
  border-top: 1px solid var(--border);
  font-size: 13px;
  color: var(--text-sec);
  display: flex;
  align-items: center;
  justify-content: space-between;
}

.peer-settings-bar select {
  background: var(--bg-dark);
  color: var(--text-main);
  border: 1px solid var(--border);
  padding: 6px 12px;
  border-radius: 6px;
  outline: none;
}

/* Avatar styling */
.avatar {
  width: 40px;
  height: 40px;
  border-radius: 50%;
  object-fit: cover;
  display: flex;
  align-items: center;
  justify-content: center;
  font-weight: 600;
  font-size: 16px;
  color: #fff;
  user-select: none;
}

.avatar.emoji-avatar {
  background: #252f40;
  font-size: 20px;
}

.chat-avatar-large {
  width: 64px;
  height: 64px;
  border-radius: 50%;
  margin: 0 auto 16px auto;
  display: flex;
  align-items: center;
  justify-content: center;
  font-weight: 600;
  font-size: 26px;
  color: #fff;
}

/* Modals / Settings dialog styling */
.modal-overlay {
  position: fixed;
  top: 0; left: 0; right: 0; bottom: 0;
  background: rgba(0,0,0,0.6);
  backdrop-filter: blur(4px);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 1000;
  animation: fadeIn 0.2s ease-out;
}

.settings-modal {
  width: 100%;
  max-width: 440px;
  background: var(--bg-sidebar);
  border-radius: 16px;
  padding: 28px;
  box-shadow: 0 15px 40px rgba(0,0,0,0.5);
  border: 1px solid rgba(255,255,255,0.06);
}

.settings-modal h3 {
  font-size: 18px;
  font-weight: 600;
  margin-bottom: 20px;
  text-align: center;
}

.settings-group {
  margin-bottom: 16px;
}

.settings-group label {
  display: block;
  font-size: 12.5px;
  color: var(--text-sec);
  margin-bottom: 6px;
  font-weight: 500;
}

.settings-group span.read-only-val {
  display: block;
  padding: 8px 12px;
  background: var(--bg-dark);
  border-radius: 6px;
  font-size: 13.5px;
  color: var(--text-sec);
  border: 1px solid var(--border);
  font-family: monospace;
}

.modal-actions {
  display: flex;
  gap: 12px;
  margin-top: 24px;
}

.btn-danger {
  width: 100%;
  padding: 12px;
  border-radius: 8px;
  border: none;
  background: var(--danger);
  color: #fff;
  font-size: 14.5px;
  font-weight: 500;
  cursor: pointer;
  transition: filter 0.2s;
  outline: none;
}

.btn-danger:hover {
  filter: brightness(0.9);
}
</style>
</head>
<body>

<!-- SCREEN 1: SERVER CONNECTION -->
<div id="screen-connect" class="auth-container">
  <div class="auth-card">
    <h1><span>Z</span>erolink</h1>
    <p>Подключение к серверу обмена</p>
    <div class="input-wrapper">
      <input id="server-addr" placeholder="Адрес сервера (например, 127.0.0.1:8080)" value="127.0.0.1:8080">
    </div>
    <div id="connect-error" class="error-msg"></div>
    <button id="connect-btn" class="btn-primary">Установить связь</button>
    <div id="connect-logs" class="logs-container hidden"></div>
  </div>
</div>

<!-- SCREEN 2: AUTHENTICATION -->
<div id="screen-auth" class="auth-container hidden">
  <div class="auth-card">
    <h1><span>Z</span>erolink</h1>
    <p id="auth-subtitle">Авторизация на сервере</p>
    
    <div class="auth-tabs">
      <div id="tab-login" class="auth-tab active" onclick="setAuthTab(false)">Вход</div>
      <div id="tab-register" class="auth-tab" onclick="setAuthTab(true)">Регистрация</div>
    </div>

    <div id="auth-form">
      <div class="input-wrapper">
        <input id="auth-username" placeholder="Имя пользователя">
      </div>
      <div id="auth-nickname-wrapper" class="input-wrapper hidden">
        <input id="auth-nickname" placeholder="Никнейм">
      </div>
      <div class="input-wrapper">
        <input id="auth-password" type="password" placeholder="Пароль">
        <button type="button" class="password-toggle" onclick="togglePass('auth-password', this)">👁️</button>
      </div>
      <div id="auth-confirm-wrapper" class="input-wrapper hidden">
        <input id="auth-confirm-password" type="password" placeholder="Подтверждение пароля">
        <button type="button" class="password-toggle" onclick="togglePass('auth-confirm-password', this)">👁️</button>
      </div>
      <div id="auth-error" class="error-msg"></div>
      <button id="auth-btn" class="btn-primary">Войти</button>
      <button id="auth-back-btn" class="btn-secondary" style="margin-top: 10px;">Сменить сервер</button>
    </div>
  </div>
</div>

<!-- SCREEN 3: MAIN MESSENGER DASHBOARD -->
<div id="screen-main" class="hidden">
  <div id="app">
    <!-- Sidebar Pane -->
    <div id="sidebar">
      <div id="sidebar-header">
        <div>
          <h2>` + version.Name + `</h2>
          <div class="status-indicator">
            <span id="status-dot" class="status-dot disconnected"></span>
            <span id="connection-status">отключение</span>
          </div>
        </div>
        <div class="sidebar-actions">
          <button id="peers-toggle" title="Пользователи">&#128101;</button>
          <button id="settings-btn" title="Настройки">&#9881;</button>
          <button id="quit-btn" class="logout" title="Выйти">&#10006;</button>
        </div>
      </div>
      
      <!-- Conversations Search -->
      <div class="search-wrapper">
        <input id="chat-search" placeholder="Поиск чатов..." oninput="filterChats()">
      </div>
      
      <!-- Chats list -->
      <div id="chat-list"></div>
    </div>
    
    <!-- Main Chat Workspace -->
    <div id="main">
      <div id="main-header">
        <div id="main-header-info">
          <h3 id="current-chat-name">Выберите чат</h3>
          <p id="current-chat-status" class="hidden"></p>
        </div>
      </div>
      
      <div id="messages"></div>
      
      <div id="input-area">
        <input id="msg-input" placeholder="Написать сообщение..." disabled>
        <button id="send-btn" disabled>
          <svg viewBox="0 0 24 24">
            <path d="M2,21L23,12L2,3V10L17,12L2,14V21Z" />
          </svg>
        </button>
      </div>
    </div>
    
    <!-- Registered Users / Peers Right Panel -->
    <div id="peers-panel" class="hidden">
      <h3>Зарегистрированные пользователи</h3>
      <div class="search-wrapper">
        <input id="peer-search" placeholder="Поиск пользователей..." oninput="filterPeers()">
      </div>
      <div id="peer-list"></div>
      <div class="peer-settings-bar">
        <span>Режим доставки:</span>
        <select id="transport-select">
          <option value="both">Смешанный</option>
          <option value="p2p">Прямой P2P</option>
          <option value="server">Через сервер</option>
        </select>
      </div>
    </div>
  </div>
</div>

<!-- SETTINGS MODAL DIALOG -->
<div id="settings-modal" class="modal-overlay hidden">
  <div class="settings-modal">
    <h3>Настройки профиля</h3>
    
    <!-- Interactive Avatar display -->
    <div id="settings-avatar-preview" class="chat-avatar-large"></div>
    
    <div class="settings-group">
      <label>Имя пользователя (аккаунт)</label>
      <span id="settings-username" class="read-only-val">@</span>
    </div>
    <div class="settings-group">
      <label>Никнейм (отображаемое имя)</label>
      <input id="settings-nickname" placeholder="Имя для отображения...">
    </div>
    <div class="settings-group">
      <label>О себе (статус/биография)</label>
      <input id="settings-bio" placeholder="Несколько слов о себе...">
    </div>
    <div class="settings-group">
      <label>Аватарка (Эмодзи или URL картинки)</label>
      <input id="settings-avatar-input" placeholder="Введите эмодзи или URL...">
    </div>
    <div class="settings-group">
      <label>Идентификатор узла (Node ID)</label>
      <span id="settings-node-id" class="read-only-val"></span>
    </div>
    
    <div class="modal-actions">
      <button id="settings-cancel" class="btn-secondary">Отмена</button>
      <button id="settings-save" class="btn-primary">Сохранить</button>
    </div>
    <button id="settings-logout" class="btn-danger" style="margin-top: 12px;">Выйти из аккаунта</button>
  </div>
</div>

<script>
let ws, token, username, currentChat, chats = {}, peers = [], localNodeID = '', connectedServer = '';
let isRegister = false;

const screenConnect = document.getElementById('screen-connect');
const screenAuth = document.getElementById('screen-auth');
const screenMain = document.getElementById('screen-main');

const connectBtn = document.getElementById('connect-btn');
const serverAddr = document.getElementById('server-addr');
const connectError = document.getElementById('connect-error');

const authSubtitle = document.getElementById('auth-subtitle');
const tabLogin = document.getElementById('tab-login');
const tabRegister = document.getElementById('tab-register');
const authNicknameWrapper = document.getElementById('auth-nickname-wrapper');
const authConfirmWrapper = document.getElementById('auth-confirm-wrapper');
const authUser = document.getElementById('auth-username');
const authPass = document.getElementById('auth-password');
const authConfirmPass = document.getElementById('auth-confirm-password');
const authNick = document.getElementById('auth-nickname');
const authBtn = document.getElementById('auth-btn');
const authBackBtn = document.getElementById('auth-back-btn');
const authError = document.getElementById('auth-error');

const settingsBtn = document.getElementById('settings-btn');
const settingsModal = document.getElementById('settings-modal');
const settingsCancel = document.getElementById('settings-cancel');
const settingsSave = document.getElementById('settings-save');
const settingsLogout = document.getElementById('settings-logout');

// Logger helper for Server Connection
function logMsg(msg, isError = false) {
  const el = document.getElementById('connect-logs');
  el.classList.remove('hidden');
  const logLine = document.createElement('div');
  logLine.className = 'log-line' + (isError ? ' error' : '');
  const time = new Date().toLocaleTimeString();
  logLine.textContent = '[' + time + '] ' + msg;
  el.appendChild(logLine);
  el.scrollTop = el.scrollHeight;
}

// Password toggle helper
function togglePass(id, btn) {
  const el = document.getElementById(id);
  if (el.type === 'password') {
    el.type = 'text';
    btn.textContent = '🙈';
  } else {
    el.type = 'password';
    btn.textContent = '👁️';
  }
}

// Avatar rendering algorithms
function renderAvatar(u, n, avatarPath) {
  if (avatarPath && avatarPath.trim() !== '') {
    if (avatarPath.length <= 4) {
      return '<div class="avatar emoji-avatar">' + avatarPath + '</div>';
    }
    return '<img class="avatar" src="' + avatarPath + '" onerror="this.outerHTML=renderDefaultAvatar(\'' + u + '\', \'' + n + '\')">';
  }
  return renderDefaultAvatar(u, n);
}

function renderDefaultAvatar(u, n) {
  const name = n || u || '?';
  const initial = name.charAt(0).toUpperCase();
  const colors = ['#e17076', '#faa774', '#a695e7', '#7bc862', '#6ec9cb', '#65a9e6', '#ee7aae'];
  let hash = 0;
  const str = u || name;
  for (let i = 0; i < str.length; i++) {
    hash = str.charCodeAt(i) + ((hash << 5) - hash);
  }
  const color = colors[Math.abs(hash) % colors.length];
  return '<div class="avatar initials-avatar" style="background-color: ' + color + ';">' + initial + '</div>';
}

function updateSettingsAvatarPreview() {
  const input = document.getElementById('settings-avatar-input').value.trim();
  const nick = document.getElementById('settings-nickname').value.trim();
  const previewEl = document.getElementById('settings-avatar-preview');
  
  if (input !== '') {
    if (input.length <= 4) {
      previewEl.outerHTML = '<div id="settings-avatar-preview" class="chat-avatar-large emoji-avatar">' + input + '</div>';
      return;
    }
    previewEl.outerHTML = '<img id="settings-avatar-preview" class="chat-avatar-large" src="' + input + '" onerror="this.outerHTML=renderDefaultAvatarLarge(\'' + username + '\', \'' + nick + '\')">';
  } else {
    previewEl.outerHTML = renderDefaultAvatarLarge(username, nick);
  }
}

function renderDefaultAvatarLarge(u, n) {
  const name = n || u || '?';
  const initial = name.charAt(0).toUpperCase();
  const colors = ['#e17076', '#faa774', '#a695e7', '#7bc862', '#6ec9cb', '#65a9e6', '#ee7aae'];
  let hash = 0;
  const str = u || name;
  for (let i = 0; i < str.length; i++) {
    hash = str.charCodeAt(i) + ((hash << 5) - hash);
  }
  const color = colors[Math.abs(hash) % colors.length];
  return '<div id="settings-avatar-preview" class="chat-avatar-large" style="background-color: ' + color + ';">' + initial + '</div>';
}

// Authentication Tabs management
function setAuthTab(reg) {
  isRegister = reg;
  tabLogin.classList.toggle('active', !isRegister);
  tabRegister.classList.toggle('active', isRegister);
  authSubtitle.textContent = isRegister ? 'Регистрация нового аккаунта' : 'Авторизация на сервере';
  authNicknameWrapper.classList.toggle('hidden', !isRegister);
  authConfirmWrapper.classList.toggle('hidden', !isRegister);
  authBtn.textContent = isRegister ? 'Зарегистрироваться' : 'Войти';
  authError.textContent = '';
}

// Screen Transitions
function showScreen(screenId) {
  screenConnect.classList.add('hidden');
  screenAuth.classList.add('hidden');
  screenMain.classList.add('hidden');
  
  if (screenId === 'connect') screenConnect.classList.remove('hidden');
  if (screenId === 'auth') screenAuth.classList.remove('hidden');
  if (screenId === 'main') screenMain.classList.remove('hidden');
}

// Step 1: Connect to Server
connectBtn.onclick = async () => {
  const s = serverAddr.value.trim();
  if (!s) { connectError.textContent = 'Введите адрес сервера'; return; }
  connectError.textContent = '';
  
  logMsg('Подключение к серверу ' + s + '...');
  connectBtn.disabled = true;
  
  try {
    const response = await fetch('/api/server/connect', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ server: s })
    });
    const d = await response.json();
    if (d.ok === 'true') {
      logMsg('Успешное подключение к серверу!');
      connectedServer = s;
      setTimeout(() => {
        connectBtn.disabled = false;
        showScreen('auth');
      }, 800);
    } else {
      logMsg('Ошибка связи: ' + (d.error || 'неизвестная ошибка'), true);
      connectError.textContent = d.error || 'Не удалось связаться с сервером';
      connectBtn.disabled = false;
    }
  } catch (err) {
    logMsg('Сбой подключения: ' + err.message, true);
    connectError.textContent = 'Ошибка сети при подключении к ' + s;
    connectBtn.disabled = false;
  }
};

// Step 2: Auth handlers
authBackBtn.onclick = () => {
  showScreen('connect');
};

authBtn.onclick = async () => {
  const u = authUser.value.trim(), p = authPass.value;
  if (!u || !p) { authError.textContent = 'Пожалуйста, заполните все поля'; return; }
  
  if (isRegister) {
    const nick = authNick.value.trim();
    const cp = authConfirmPass.value;
    if (!nick) { authError.textContent = 'Введите никнейм для отображения'; return; }
    if (p !== cp) { authError.textContent = 'Пароли не совпадают'; return; }
    if (p.length < 4) { authError.textContent = 'Пароль должен содержать от 4 символов'; return; }
    
    authError.textContent = '';
    try {
      let r = await fetch('/api/register', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ server: connectedServer, username: u, password: p, nickname: nick })
      });
      let d = await r.json();
      if (d.ok === 'true') {
        token = d.token;
        username = u;
        authSuccess();
      } else {
        authError.textContent = d.error || 'Сбой регистрации';
      }
    } catch(e) {
      authError.textContent = 'Ошибка связи при регистрации';
    }
  } else {
    authError.textContent = '';
    try {
      let r = await fetch('/api/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ server: connectedServer, username: u, password: p })
      });
      let d = await r.json();
      if (d.ok === 'true') {
        token = d.token;
        username = u;
        authSuccess();
      } else {
        authError.textContent = d.error || 'Неверный логин или пароль';
      }
    } catch(e) {
      authError.textContent = 'Ошибка связи при входе';
    }
  }
};

async function authSuccess() {
  showScreen('main');
  // Load status to fetch correct node_id and nickname
  const statusRes = await fetch('/api/auth/status');
  const statusData = await statusRes.json();
  localNodeID = statusData.node_id;
  
  connectWS();
  loadData();
}

function connectWS() {
  ws = new WebSocket((location.protocol === 'https:' ? 'wss' : 'ws') + '://' + location.host + '/ws');
  ws.onopen = () => {
    const dot = document.getElementById('status-dot');
    dot.className = 'status-dot connected';
    document.getElementById('connection-status').textContent = 'подключено';
  };
  ws.onclose = () => {
    const dot = document.getElementById('status-dot');
    dot.className = 'status-dot disconnected';
    document.getElementById('connection-status').textContent = 'отключение';
    setTimeout(connectWS, 3000);
  };
  ws.onmessage = e => {
    let evt = JSON.parse(e.data);
    if (evt.type === 'message.received') {
      loadMessages();
      loadChats(); // Update last message in chat preview
    }
    if (evt.type === 'peer.online' || evt.type === 'peer.offline') {
      loadPeers();
    }
  };
}

async function loadData() {
  loadPeers();
  loadChats();
  
  // Load transport settings select value
  const r = await fetch('/api/settings');
  const d = await r.json();
  if (d.transport_mode) {
    document.getElementById('transport-select').value = d.transport_mode;
  }
}

// Load Peers (Right panel users)
async function loadPeers() {
  let r = await fetch('/api/peers');
  let d = await r.json();
  if (d.peers) {
    peers = d.peers;
    filterPeers(); // Render initially
  }
}

function renderPeerList(list) {
  const el = document.getElementById('peer-list');
  el.innerHTML = '';
  list.forEach(p => {
    // Skip rendering self in active peers
    if (p.username === username) return;
    
    let div = document.createElement('div');
    div.className = 'peer-item';
    
    const avatarHTML = renderAvatar(p.username, p.nickname, p.avatar_path);
    
    div.innerHTML = avatarHTML + 
      '<div class="peer-item-info">' + 
        '<div class="peer-item-name">' + htmlEscape(p.nickname || p.username) + '</div>' +
        '<div class="peer-item-meta">@' + htmlEscape(p.username) + ' • <span style="font-family:monospace;font-size:10px;">' + p.node_id.substring(0,8) + '</span></div>' +
      '</div>' +
      '<div class="peer-status-marker">' +
        '<span class="peer-status-dot ' + (p.online ? 'online' : 'offline') + '"></span>' +
      '</div>';
      
    div.onclick = async () => {
      let r = await fetch('/api/dm', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ node_id: p.node_id, fingerprint: p.fingerprint })
      });
      let d = await r.json();
      if (d.ok) {
        currentChat = d.chat_id;
        await loadChats();
        loadMessages();
        document.getElementById('msg-input').disabled = false;
        document.getElementById('send-btn').disabled = false;
        document.getElementById('current-chat-name').textContent = p.nickname || p.username;
        // Optionally focus chat
        document.getElementById('msg-input').focus();
      }
    };
    el.appendChild(div);
  });
}

function filterPeers() {
  const query = document.getElementById('peer-search').value.toLowerCase();
  const filtered = peers.filter(p => 
    p.username.toLowerCase().includes(query) || 
    (p.nickname && p.nickname.toLowerCase().includes(query))
  );
  renderPeerList(filtered);
}

// Load Chats (Conversations sidebar)
async function loadChats() {
  let r = await fetch('/api/chats');
  let d = await r.json();
  if (!Array.isArray(d)) return;
  
  chats = {};
  let el = document.getElementById('chat-list');
  el.innerHTML = '';
  
  d.forEach(c => {
    chats[c.id] = c;
    let div = document.createElement('div');
    div.className = 'chat-item' + (c.id === currentChat ? ' active' : '');
    
    // Resolve clean chat name and preview last message
    let displayName = c.name || c.id.substring(0, 8);
    const avatarHTML = renderAvatar(displayName, displayName, '');
    
    div.innerHTML = avatarHTML + 
      '<div class="chat-item-info">' + 
        '<div class="chat-item-name">' + htmlEscape(displayName) + '</div>' +
        '<div class="chat-item-meta">Идентификатор: ' + c.id.substring(0, 8) + '</div>' +
      '</div>';
      
    div.onclick = () => {
      currentChat = c.id;
      loadMessages();
      document.querySelectorAll('.chat-item').forEach(x => x.classList.remove('active'));
      div.classList.add('active');
      document.getElementById('msg-input').disabled = false;
      document.getElementById('send-btn').disabled = false;
      document.getElementById('current-chat-name').textContent = displayName;
      document.getElementById('msg-input').focus();
    };
    el.appendChild(div);
  });
}

function filterChats() {
  const query = document.getElementById('chat-search').value.toLowerCase();
  document.querySelectorAll('.chat-item').forEach(item => {
    const text = item.querySelector('.chat-item-name').textContent.toLowerCase();
    item.classList.toggle('hidden', !text.includes(query));
  });
}

// Load Message History
async function loadMessages() {
  if (!currentChat) return;
  let r = await fetch('/api/messages?chat_id=' + currentChat);
  let d = await r.json();
  if (!Array.isArray(d)) return;
  
  let el = document.getElementById('messages');
  el.innerHTML = '';
  
  d.forEach(m => {
    let div = document.createElement('div');
    const isOwn = m.sender_id && m.sender_id.node_id === localNodeID;
    div.className = 'msg-wrapper ' + (isOwn ? 'own' : 'other');
    
    let senderHTML = '';
    if (!isOwn) {
      senderHTML = '<div class="msg-sender">' + htmlEscape(m.sender_id.node_id.substring(0, 8)) + '</div>';
    }
    
    div.innerHTML = '<div class="msg ' + (isOwn ? 'own' : 'other') + '">' +
      senderHTML + 
      '<div class="msg-text">' + htmlEscape(m.payload || '') + '</div>' +
      '<div class="msg-time">' + new Date(m.sent_at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) + '</div>' +
    '</div>';
    
    el.appendChild(div);
  });
  el.scrollTop = el.scrollHeight;
}

// Message Sending
document.getElementById('send-btn').onclick = sendMsg;
document.getElementById('msg-input').onkeydown = e => { if (e.key === 'Enter') sendMsg(); };

async function sendMsg() {
  let input = document.getElementById('msg-input');
  let text = input.value.trim();
  if (!text || !currentChat) return;
  input.value = '';
  await fetch('/api/send', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ chat_id: currentChat, text })
  });
  setTimeout(loadMessages, 100);
}

// Settings Modal interactions
settingsBtn.onclick = async () => {
  const r = await fetch('/api/auth/status');
  const d = await r.json();
  
  document.getElementById('settings-username').textContent = '@' + d.username;
  document.getElementById('settings-nickname').value = d.nickname || '';
  document.getElementById('settings-bio').value = d.bio || '';
  document.getElementById('settings-avatar-input').value = d.avatar || '';
  document.getElementById('settings-node-id').textContent = d.node_id;
  
  updateSettingsAvatarPreview();
  
  settingsModal.classList.remove('hidden');
};

document.getElementById('settings-avatar-input').oninput = updateSettingsAvatarPreview;
document.getElementById('settings-nickname').oninput = updateSettingsAvatarPreview;

settingsCancel.onclick = () => {
  settingsModal.classList.add('hidden');
};

settingsSave.onclick = async () => {
  const nick = document.getElementById('settings-nickname').value.trim();
  const bio = document.getElementById('settings-bio').value.trim();
  const avatar = document.getElementById('settings-avatar-input').value.trim();
  
  if (!nick) { alert('Никнейм не может быть пустым'); return; }
  
  const res = await fetch('/api/profile/update', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ nickname: nick, bio: bio, avatar: avatar })
  });
  const data = await res.json();
  if (data.ok === 'true') {
    settingsModal.classList.add('hidden');
    loadData();
  } else {
    alert(data.error || 'Ошибка при сохранении настроек');
  }
};

settingsLogout.onclick = async () => {
  if (confirm('Вы действительно хотите выйти из аккаунта?')) {
    await fetch('/api/logout', { method: 'POST' });
    settingsModal.classList.add('hidden');
    showScreen('connect');
  }
};

document.getElementById('transport-select').onchange = async e => {
  await fetch('/api/settings', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ transport_mode: e.target.value })
  });
};

document.getElementById('peers-toggle').onclick = () => {
  document.getElementById('peers-panel').classList.toggle('hidden');
};

document.getElementById('quit-btn').onclick = async () => {
  if (confirm('Закрыть Zerolink?')) {
    await fetch('/api/shutdown', { method: 'POST' });
    setTimeout(() => {
      document.body.innerHTML = '<div style="display:flex;height:100vh;align-items:center;justify-content:center;font-size:24px;color:#888;background:#0e1621;">Zerolink остановлен</div>';
    }, 500);
  }
};

// Automatic load and session persistence check
window.onload = async () => {
  try {
    const res = await fetch('/api/auth/status');
    const d = await res.json();
    
    if (d.connected && d.authenticated) {
      token = d.token;
      username = d.username;
      localNodeID = d.node_id;
      showScreen('main');
      connectWS();
      loadData();
    } else if (d.connected) {
      showScreen('auth');
    } else {
      showScreen('connect');
    }
  } catch (err) {
    showScreen('connect');
  }
};

function htmlEscape(s) {
  const d = document.createElement('div');
  d.textContent = s;
  return d.innerHTML;
}
</script>
</body>
</html>`

package messenger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/yourorg/messenger-core/core"
	"github.com/yourorg/messenger-core/version"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
	ReadBufferSize: 4096,
	WriteBufferSize: 4096,
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
	result, err := httpPost("http://"+req.Server+"/auth/login", map[string]string{
		"username": req.Username, "password": req.Password,
	})
	if err != nil {
		json.NewEncoder(rw).Encode(map[string]string{"error": err.Error()})
		return
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
	result, err := httpPost("http://"+req.Server+"/auth/register", map[string]string{
		"username": req.Username, "password": req.Password,
		"nickname": req.Nickname, "bio": "", "avatar_path": "",
	})
	if err != nil {
		json.NewEncoder(rw).Encode(map[string]string{"error": err.Error()})
		return
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
* { margin:0; padding:0; box-sizing:border-box; }
body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; background: #1a1a2e; color: #eee; height: 100vh; display: flex; flex-direction: column; }
.hidden { display: none !important; }
#auth { max-width: 400px; margin: auto; padding: 40px; background: #16213e; border-radius: 12px; width: 90%; }
#auth h1 { text-align: center; margin-bottom: 24px; font-size: 28px; }
#auth h1 span { color: #0f3460; }
#auth input { width: 100%; padding: 12px; margin: 8px 0; border: none; border-radius: 8px; background: #1a1a2e; color: #eee; font-size: 14px; }
#auth button { width: 100%; padding: 12px; margin: 8px 0; border: none; border-radius: 8px; background: #0f3460; color: #eee; font-size: 16px; cursor: pointer; }
#auth button:hover { background: #1a5276; }
#auth .toggle { text-align: center; color: #888; cursor: pointer; margin-top: 12px; font-size: 14px; }
#auth .error { color: #e74c3c; text-align: center; margin-top: 8px; font-size: 13px; }
#app { display: flex; height: 100vh; }
#sidebar { width: 300px; background: #16213e; display: flex; flex-direction: column; border-right: 1px solid #0f3460; }
#sidebar-header { padding: 16px; border-bottom: 1px solid #0f3460; display: flex; justify-content: space-between; align-items: center; }
#sidebar-header h2 { font-size: 18px; }
#sidebar-header .status { font-size: 12px; color: #888; }
#chat-list { flex: 1; overflow-y: auto; padding: 8px; }
#chat-list .chat-item { padding: 12px; margin: 4px 0; border-radius: 8px; cursor: pointer; }
#chat-list .chat-item:hover { background: #1a1a2e; }
#chat-list .chat-item.active { background: #0f3460; }
#main { flex: 1; display: flex; flex-direction: column; }
#main-header { padding: 16px 20px; border-bottom: 1px solid #0f3460; display: flex; justify-content: space-between; align-items: center; }
#main-header h3 { font-size: 16px; }
#messages { flex: 1; overflow-y: auto; padding: 20px; display: flex; flex-direction: column; gap: 8px; }
.msg { max-width: 70%; padding: 10px 16px; border-radius: 12px; font-size: 14px; line-height: 1.4; word-break: break-word; }
.msg.own { align-self: flex-end; background: #0f3460; border-bottom-right-radius: 4px; }
.msg.other { align-self: flex-start; background: #16213e; border-bottom-left-radius: 4px; }
.msg .time { font-size: 11px; color: #888; margin-top: 4px; text-align: right; }
#input-area { padding: 16px 20px; border-top: 1px solid #0f3460; display: flex; gap: 12px; }
#input-area input { flex: 1; padding: 12px; border: none; border-radius: 8px; background: #16213e; color: #eee; font-size: 14px; }
#input-area button { padding: 12px 24px; border: none; border-radius: 8px; background: #0f3460; color: #eee; cursor: pointer; font-size: 14px; }
#input-area button:hover { background: #1a5276; }
#peers-panel { width: 280px; background: #16213e; border-left: 1px solid #0f3460; display: flex; flex-direction: column; }
#peers-panel h3 { padding: 16px; border-bottom: 1px solid #0f3460; font-size: 14px; }
#peer-list { flex: 1; overflow-y: auto; padding: 8px; }
.peer-item { padding: 10px 12px; margin: 4px 0; border-radius: 8px; cursor: pointer; font-size: 13px; }
.peer-item:hover { background: #1a1a2e; }
.peer-item .online { color: #2ecc71; }
.peer-item .offline { color: #888; }
.settings-row { padding: 12px 16px; border-top: 1px solid #0f3460; font-size: 13px; }
.settings-row select { background: #1a1a2e; color: #eee; border: none; padding: 6px; border-radius: 4px; }
</style>
</head>
<body>
<div id="auth">
  <h1><span>` + "Z" + `</span>erolink</h1>
  <div id="auth-form">
    <input id="server-addr" placeholder="Server address (e.g. 192.168.1.100:8080)" value="127.0.0.1:8080">
    <input id="auth-username" placeholder="Username">
    <input id="auth-password" type="password" placeholder="Password">
    <input id="auth-nickname" class="hidden" placeholder="Nickname">
    <div id="auth-error" class="error"></div>
    <button id="auth-btn">Login</button>
    <div class="toggle" id="auth-toggle">No account? Register</div>
  </div>
</div>
<div id="app" class="hidden">
  <div id="sidebar">
    <div id="sidebar-header">
      <div><h2>` + version.Name + `</h2><span class="status" id="connection-status">disconnected</span></div>
      <div>
        <button style="background:none;border:none;color:#888;cursor:pointer;font-size:16px;" id="settings-btn" title="Settings">&#9881;</button>
        <button style="background:none;border:none;color:#e74c3c;cursor:pointer;font-size:16px;" id="quit-btn" title="Quit">&#10005;</button>
      </div>
    </div>
    <div id="chat-list"></div>
  </div>
  <div id="main">
    <div id="main-header"><h3 id="current-chat-name">Select a chat</h3><button id="peers-toggle" style="background:none;border:none;color:#888;cursor:pointer;">&#128101;</button></div>
    <div id="messages"></div>
    <div id="input-area">
      <input id="msg-input" placeholder="Type a message..." disabled>
      <button id="send-btn" disabled>Send</button>
    </div>
  </div>
  <div id="peers-panel" class="hidden">
    <h3>Users</h3>
    <div id="peer-list"></div>
    <div class="settings-row">
      Mode: <select id="transport-select"><option value="both">Both</option><option value="p2p">P2P</option><option value="server">Server</option></select>
    </div>
  </div>
</div>
<script>
let ws, token, username, currentChat, chats = {}, peers = [];
const authDiv = document.getElementById('auth');
const appDiv = document.getElementById('app');
const authError = document.getElementById('auth-error');
const authBtn = document.getElementById('auth-btn');
const authToggle = document.getElementById('auth-toggle');
const serverAddr = document.getElementById('server-addr');
const authUser = document.getElementById('auth-username');
const authPass = document.getElementById('auth-password');
const authNick = document.getElementById('auth-nickname');
let isRegister = false;
authToggle.onclick = () => { isRegister = !isRegister; authNick.classList.toggle('hidden', !isRegister); authBtn.textContent = isRegister ? 'Register' : 'Login'; authToggle.textContent = isRegister ? 'Already have an account? Login' : 'No account? Register'; };
authBtn.onclick = async () => {
  const s = serverAddr.value.trim(), u = authUser.value.trim(), p = authPass.value;
  if (!s || !u || !p) { authError.textContent = 'Fill all fields'; return; }
  authError.textContent = '';
  let ep = isRegister ? '/api/register' : '/api/login';
  let body = isRegister ? JSON.stringify({server:s,username:u,password:p,nickname:authNick.value}) : JSON.stringify({server:s,username:u,password:p});
  try {
    let r = await fetch(ep, {method:'POST',headers:{'Content-Type':'application/json'},body});
    let d = await r.json();
    if (d.ok === 'true') { token = d.token; username = u; authDiv.classList.add('hidden'); appDiv.classList.remove('hidden'); connectWS(); loadData(); }
    else authError.textContent = d.error || 'Auth failed';
  } catch(e) { authError.textContent = 'Connection error'; }
};
function connectWS() {
  ws = new WebSocket((location.protocol==='https'?'wss':'ws')+'://'+location.host+'/ws');
  ws.onopen = () => document.getElementById('connection-status').textContent = 'connected';
  ws.onclose = () => { document.getElementById('connection-status').textContent = 'disconnected'; setTimeout(connectWS, 3000); };
  ws.onmessage = e => {
    let evt = JSON.parse(e.data);
    if (evt.type === 'message.received') loadMessages(currentChat);
    if (evt.type === 'peer.online' || evt.type === 'peer.offline') loadPeers();
  };
}
async function loadData() { loadPeers(); loadChats(); }
async function loadPeers() {
  let r = await fetch('/api/peers'); let d = await r.json();
  if (d.peers) {
    peers = d.peers;
    let el = document.getElementById('peer-list'); el.innerHTML = '';
    d.peers.forEach(p => {
      let div = document.createElement('div'); div.className = 'peer-item';
      div.innerHTML = '<span class="'+(p.online?'online':'offline')+'">'+(p.online?'●':'○')+'</span> '+(p.nickname||p.username)+' <span style="color:#555;font-size:11px">'+p.node_id.substring(0,8)+'</span>';
      div.onclick = async () => {
        let r = await fetch('/api/dm', {method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({node_id:p.node_id,fingerprint:p.fingerprint})});
        let d = await r.json();
        if (d.ok) loadChats();
      };
      el.appendChild(div);
    });
  }
}
async function loadChats() {
  let r = await fetch('/api/chats'); let d = await r.json();
  if (!Array.isArray(d)) return;
  chats = {}; let el = document.getElementById('chat-list'); el.innerHTML = '';
  d.forEach(c => {
    chats[c.id] = c;
    let div = document.createElement('div'); div.className = 'chat-item'+(c.id===currentChat?' active':'');
    div.textContent = c.name || c.id.substring(0,8);
    div.onclick = () => { currentChat = c.id; loadMessages(); document.querySelectorAll('.chat-item').forEach(x=>x.classList.remove('active')); div.classList.add('active'); document.getElementById('msg-input').disabled = false; document.getElementById('send-btn').disabled = false; document.getElementById('current-chat-name').textContent = c.name || 'Chat'; };
    el.appendChild(div);
  });
}
async function loadMessages() {
  if (!currentChat) return;
  let r = await fetch('/api/messages?chat_id='+currentChat); let d = await r.json();
  if (!Array.isArray(d)) return;
  let el = document.getElementById('messages'); el.innerHTML = '';
  d.forEach(m => {
    let div = document.createElement('div'); div.className = 'msg '+(m.sender_id&&m.sender_id.node_id===document.querySelector('#auth-username').value?'own':'other');
    div.innerHTML = htmlEscape(m.payload||'') + '<div class="time">'+new Date(m.sent_at).toLocaleTimeString()+'</div>';
    el.appendChild(div);
  });
  el.scrollTop = el.scrollHeight;
}
document.getElementById('send-btn').onclick = sendMsg;
document.getElementById('msg-input').onkeydown = e => { if (e.key==='Enter') sendMsg(); };
async function sendMsg() {
  let input = document.getElementById('msg-input');
  let text = input.value.trim();
  if (!text || !currentChat) return;
  input.value = '';
  await fetch('/api/send', {method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({chat_id:currentChat,text})});
  setTimeout(loadMessages, 200);
}
document.getElementById('transport-select').onchange = async e => {
  await fetch('/api/settings', {method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({transport_mode:e.target.value})});
};
document.getElementById('peers-toggle').onclick = () => { document.getElementById('peers-panel').classList.toggle('hidden'); };
document.getElementById('settings-btn').onclick = () => { document.getElementById('peers-panel').classList.toggle('hidden'); };
document.getElementById('quit-btn').onclick = async () => {
  if (confirm('Shutdown Zerolink?')) {
    await fetch('/api/shutdown', {method:'POST'});
    setTimeout(() => { document.body.innerHTML = '<div style="display:flex;height:100vh;align-items:center;justify-content:center;font-size:24px;color:#888;">Zerolink stopped</div>'; }, 500);
  }
};
function htmlEscape(s) { const d = document.createElement('div'); d.textContent = s; return d.innerHTML; }
</script>
</body>
</html>`

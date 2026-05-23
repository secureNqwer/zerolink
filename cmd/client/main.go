package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/secureNqwer/zerolink/core"
	"github.com/secureNqwer/zerolink/gui"
	"github.com/secureNqwer/zerolink/messenger"
	"github.com/secureNqwer/zerolink/version"
)

type authData struct {
	ServerAddr string `json:"server_addr"`
	Username   string `json:"username"`
	Token      string `json:"token"`
}

const authFile = "auth.json"

func main() {
	cliMode     := flag.Bool("cli", false, "start CLI mode")
	desktopMode := flag.Bool("gui", false, "start native desktop GUI (Fyne)")
	showVer     := flag.Bool("version", false, "show version")
	installMode := flag.Bool("install", false, "install Zerolink system-wide")
	uninstallMode := flag.Bool("uninstall", false, "remove Zerolink from system")
	cfgPath     := flag.String("config", "messenger.json", "path to config JSON")
	network     := flag.String("network", "", "ZeroTier network ID to join")
	logLevel    := flag.String("log", "info", "log level (debug|info|warn|error)")
	flag.Parse()

	if *showVer {
		fmt.Printf("%s %s (commit: %s, built: %s)\n", version.Name, version.Version, version.Commit, version.BuildTime)
		return
	}

	if *installMode {
		doInstall()
		return
	}
	if *uninstallMode {
		doUninstall()
		return
	}

	cfg := core.DefaultConfig()
	cfg.LogLevel = *logLevel
	if data, err := os.ReadFile(*cfgPath); err == nil {
		json.Unmarshal(data, cfg)
	}
	if *network != "" {
		cfg.Networks = append(cfg.Networks, core.NetworkID(*network))
	}

	log, _ := zap.NewDevelopment()

	m, err := messenger.New(cfg)
	if err != nil {
		log.Fatal("init", zap.Error(err))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := m.Start(ctx); err != nil {
		log.Fatal("start", zap.Error(err))
	}
	defer m.Stop()

	// ─── Desktop GUI mode ───────────────────────────────────────────
	if *desktopMode {
		gui.Run(m, nil)
		return
	}

	fmt.Printf("\n  Messenger started\n")
	fmt.Printf("  Node   : %s\n", m.LocalPeer().ID.NodeID)
	fmt.Printf("  Key    : %s\n", m.LocalPeer().ID.Fingerprint)
	fmt.Printf("  E2E    : %v\n\n", cfg.E2EEnabled)

	// ─── Auto-update check ────────────────────────────────────────────────
	if runtime.GOARCH != "arm64" && runtime.GOARCH != "arm" {
		checkUpdate()
	}

	// ─── Quit signal ───────────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	// ─── Auto-login or guided setup ───────────────────────────────────────
	auth := loadAuth()
	if auth != nil && auth.ServerAddr != "" {
		fmt.Printf("Connecting to %s ...\n", auth.ServerAddr)
		var err error
		if auth.Token != "" {
			err = m.ConnectToServer(ctx, auth.ServerAddr, auth.Username, auth.Token)
		} else {
			err = m.ConnectToServer(ctx, auth.ServerAddr)
		}
		if err != nil {
			fmt.Printf("Warning: server %s unreachable (%v)\n", auth.ServerAddr, err)
			// Background retry: keep trying to connect
			go func() {
				retryTicker := time.NewTicker(5 * time.Second)
				defer retryTicker.Stop()
				for attempts := 0; attempts < 12; attempts++ {
					select {
					case <-ctx.Done():
						return
					case <-retryTicker.C:
						if auth.Token != "" {
							err = m.ConnectToServer(ctx, auth.ServerAddr, auth.Username, auth.Token)
						} else {
							err = m.ConnectToServer(ctx, auth.ServerAddr)
						}
						if err == nil {
							fmt.Printf("\nReconnected to %s as %s\n", auth.ServerAddr, auth.Username)
							return
						}
					}
				}
				fmt.Printf("\nCould not reconnect to %s after multiple attempts\n", auth.ServerAddr)
			}()
		} else {
			fmt.Printf("Connected as %s\n", auth.Username)
		}
	}

			fmt.Println("Server unreachable — will retry in background. Use the Web UI to connect.")

	// ─── System tray ──────────────────────────────────────────────
	if !*cliMode && runtime.GOARCH != "arm64" {
		go gui.StartTray(func() {
			m.DisconnectServer()
			cancel()
		})
	}

	// ─── CLI or Web UI mode ──────────────────────────────────────────
	if *cliMode {
		cliLoop(ctx, m, log, quit, cancel)
		return
	}

	// ─── Web UI mode (default) ─────────────────────────────────────────
	webui := messenger.NewWebUI(m)
	addr := ":8081"
	fmt.Printf("\n  Web UI: http://localhost%s\n", addr)
	fmt.Println("  Press Ctrl+C or use the Shutdown button in the UI")
	go webui.BroadcastLoop(m.Events())
	srv := &http.Server{Addr: addr, Handler: webui.Handler()}
	webui.SetShutdownHandler(func() {
		// Signal quit channel to trigger graceful shutdown through the normal signal path
		// This ensures all defers (including m.Stop()) run properly.
		quit <- syscall.SIGTERM
	})
	go func() {
		<-quit
		log.Info("shutting down...")
		srv.Shutdown(context.Background())
		cancel()
	}()
	// Open browser
	openURL("http://localhost" + addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Info("web UI stopped")
	}
}

func cliLoop(ctx context.Context, m *messenger.Engine, log *zap.Logger, quit chan os.Signal, cancel context.CancelFunc) {
	events := m.Events().Subscribe("cli",
		core.EvtMessageReceived,
		core.EvtPeerOnline,
		core.EvtPeerOffline,
		core.EvtCallIncoming,
	)
	go func() {
		for evt := range events {
			printEvent(evt)
		}
	}()

	scanner := bufio.NewScanner(os.Stdin)
	printHelp()

	go func() {
		<-quit
		cancel()
	}()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("bye.")
			return
		default:
		}

		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		handleCommand(ctx, m, line, log)
	}
}

func openURL(url string) {
	// Try Chrome/Chromium in app mode (window without browser chrome)
	for _, name := range []string{"chromium", "google-chrome", "google-chrome-stable", "brave-browser", "firefox"} {
		if p, _ := exec.LookPath(name); p != "" {
			if name == "firefox" {
				exec.Command(p, "--new-window", url).Start()
			} else {
				exec.Command(p, "--app="+url).Start()
			}
			return
		}
	}
	// Fallback: system default
	switch runtime.GOOS {
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		exec.Command("open", url).Start()
	default:
		exec.Command("xdg-open", url).Start()
	}
}

func runGuidedSetup(ctx context.Context, m *messenger.Engine) {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("─── Messenger Setup ───")
	fmt.Println("Connect to a server to find other users.")
	fmt.Println("Press Enter to skip and use offline mode.")
	fmt.Println()
	fmt.Println("If using ZeroTier, first join the same network:")
	fmt.Println("  /join <your_network_id>")
	fmt.Println()
	addr, _ := reader.ReadString('\n')
	addr = strings.TrimSpace(addr)
	if addr == "" {
		fmt.Println("Offline mode – you can connect later with /join_server")
		return
	}

	fmt.Printf("Connecting to %s ...\n", addr)
	if err := m.ConnectToServer(ctx, addr); err != nil {
		fmt.Printf("Failed to connect: %v\n", err)
		fmt.Println("You can try again with /join_server")
		return
	}
	fmt.Println("Connected!")

	for {
		fmt.Println("[R] Register new account")
		fmt.Println("[L] Login to existing account")
		fmt.Print("Choose (R/L): ")
		choice, _ := reader.ReadString('\n')
		choice = strings.TrimSpace(strings.ToLower(choice))

		if choice == "r" {
			if doRegister(ctx, m, reader) {
				return
			}
		} else if choice == "l" {
			if doLogin(ctx, m, reader) {
				return
			}
		} else {
			fmt.Println("Please enter R or L")
		}
	}
}

func doRegister(ctx context.Context, m *messenger.Engine, reader *bufio.Reader) bool {
	sr := m.ServerRelay()
	if sr == nil {
		fmt.Println("Not connected to server")
		return false
	}

	fmt.Println("\n─── Registration ───")

	username := ""
	for {
		fmt.Print("Username (english letters, digits, _, 3-32 chars): ")
		u, _ := reader.ReadString('\n')
		u = strings.TrimSpace(strings.ToLower(u))
		if isValidUsername(u) {
			username = u
			break
		}
		fmt.Println("Invalid username. Use a-z, 0-9, _ (3-32 chars)")
	}

	fmt.Print("Nickname (can include emoji, press Enter to skip): ")
	nickname, _ := reader.ReadString('\n')
	nickname = strings.TrimSpace(nickname)

	fmt.Print("Bio / description (optional): ")
	bio, _ := reader.ReadString('\n')
	bio = strings.TrimSpace(bio)

	fmt.Print("Avatar file path (optional): ")
	avatar, _ := reader.ReadString('\n')
	avatar = strings.TrimSpace(avatar)

	password := ""
	for {
		fmt.Print("Password: ")
		p1, _ := reader.ReadString('\n')
		p1 = strings.TrimSpace(p1)
		if len(p1) < 4 {
			fmt.Println("Password must be at least 4 characters")
			continue
		}
		fmt.Print("Repeat password: ")
		p2, _ := reader.ReadString('\n')
		p2 = strings.TrimSpace(p2)
		if p1 != p2 {
			fmt.Println("Passwords don't match")
			continue
		}
		password = p1
		break
	}

	fmt.Println("Registering...")
	addr := sr.Address()
	result, err := httpPostJSON("http://"+addr+"/auth/register", map[string]string{
		"username": username, "password": password,
		"nickname": nickname, "bio": bio, "avatar_path": avatar,
	})
	if err != nil {
		fmt.Printf("Registration failed: %v\n", err)
		return false
	}
	if result["ok"] == "true" {
		token := result["token"]
		saveAuth(&authData{ServerAddr: addr, Username: username, Token: token})
		fmt.Printf("✓ Registered as %s\n", username)
		m.DisconnectServer()
		if err := m.ConnectToServer(ctx, addr, username, token); err != nil {
			fmt.Printf("Reconnect failed: %v\n", err)
		} else {
			fmt.Println("✓ Authenticated")
		}
		return true
	}
	fmt.Printf("Registration error: %s\n", result["error"])
	return false
}

func doLogin(ctx context.Context, m *messenger.Engine, reader *bufio.Reader) bool {
	sr := m.ServerRelay()
	if sr == nil {
		fmt.Println("Not connected to server")
		return false
	}

	fmt.Println("\n─── Login ───")

	fmt.Print("Username: ")
	username, _ := reader.ReadString('\n')
	username = strings.TrimSpace(strings.ToLower(username))

	fmt.Print("Password: ")
	password, _ := reader.ReadString('\n')
	password = strings.TrimSpace(password)

	fmt.Println("Logging in...")
	addr := sr.Address()
	result, err := httpPostJSON("http://"+addr+"/auth/login", map[string]string{
		"username": username, "password": password,
	})
	if err != nil {
		fmt.Printf("Login failed: %v\n", err)
		return false
	}
	if result["ok"] == "true" {
		token := result["token"]
		saveAuth(&authData{ServerAddr: addr, Username: username, Token: token})
		fmt.Printf("✓ Logged in as %s\n", username)
		m.DisconnectServer()
		if err := m.ConnectToServer(ctx, addr, username, token); err != nil {
			fmt.Printf("Reconnect failed: %v\n", err)
		} else {
			fmt.Println("✓ Authenticated")
		}
		return true
	}
	fmt.Printf("Login error: %s\n", result["error"])
	return false
}

func handleCommand(ctx context.Context, m *messenger.Engine, line string, log *zap.Logger) {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return
	}
	cmd := parts[0]
	args := parts[1:]

	switch cmd {
	case "/quit":
		os.Exit(0)

	case "/help":
		printHelp()

	case "/join_server":
		if len(args) == 0 {
			fmt.Println("usage: /join_server <addr>")
			fmt.Println("  e.g. /join_server 192.168.1.100:8080")
			fmt.Println("  e.g. /join_server 10.145.95.213:8080")
			fmt.Println("\nIf the server is on another network, you need to be on the same")
			fmt.Println("ZeroTier network. Join it first with: /join <networkID>")
			return
		}
		addr := args[0]
		if !strings.Contains(addr, ":") {
			addr = addr + ":8080"
		}
		fmt.Printf("Connecting to %s ...\n", addr)

		auth := loadAuth()
		var err error
		if auth != nil && auth.ServerAddr == addr && auth.Token != "" {
			err = m.ConnectToServer(ctx, addr, auth.Username, auth.Token)
		} else {
			err = m.ConnectToServer(ctx, addr)
		}
		if err != nil {
			fmt.Println("error:", err)
			// Check if IP looks like a ZeroTier address (10.x.x.x)
			if isZerotierIP(addr) {
				fmt.Println("\nThis looks like a ZeroTier IP.")
				fmt.Println("Make sure you're on the same ZeroTier network:")
				fmt.Println("  /join <your_network_id>")
				fmt.Println("Then try /join_server again.")
			}
			return
		}
		fmt.Println("connected to server")
		if auth != nil && auth.ServerAddr == addr {
			fmt.Printf("Authenticated as %s\n", auth.Username)
		}

	case "/register":
		sr := m.ServerRelay()
		if sr == nil {
			fmt.Println("not connected to server – use /join_server first")
			return
		}
		if len(args) < 2 {
			fmt.Println("usage: /register <username> <password> [nickname]")
			return
		}
		username := strings.ToLower(args[0])
		if !isValidUsername(username) {
			fmt.Println("invalid username: use a-z, 0-9, _ (3-32 chars)")
			return
		}
		password := args[1]
		nickname := ""
		if len(args) > 2 {
			nickname = strings.Join(args[2:], " ")
		}
		fmt.Println("Registering...")
		addr := sr.Address()
		serverAddr := "http://" + addr
		result, err := httpPostJSON(serverAddr+"/auth/register", map[string]string{
			"username": username, "password": password,
			"nickname": nickname, "bio": "", "avatar_path": "",
		})
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		if result["ok"] == "true" {
			token := result["token"]
			saveAuth(&authData{ServerAddr: addr, Username: username, Token: token})
			fmt.Printf("✓ Registered and logged in as %s\n", username)
			m.DisconnectServer()
			if err := m.ConnectToServer(ctx, addr, username, token); err != nil {
				fmt.Printf("reconnect: %v\n", err)
			}
		} else {
			fmt.Printf("error: %s\n", result["error"])
		}

	case "/login":
		sr := m.ServerRelay()
		if sr == nil {
			fmt.Println("not connected to server – use /join_server first")
			return
		}
		if len(args) < 2 {
			fmt.Println("usage: /login <username> <password>")
			return
		}
		username := strings.ToLower(args[0])
		password := args[1]
		fmt.Println("Logging in...")
		addr := sr.Address()
		result, err := httpPostJSON("http://"+addr+"/auth/login", map[string]string{
			"username": username, "password": password,
		})
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		if result["ok"] == "true" {
			token := result["token"]
			saveAuth(&authData{ServerAddr: addr, Username: username, Token: token})
			fmt.Printf("✓ Logged in as %s\n", username)
			m.DisconnectServer()
			if err := m.ConnectToServer(ctx, addr, username, token); err != nil {
				fmt.Printf("reconnect: %v\n", err)
			}
		} else {
			fmt.Printf("error: %s\n", result["error"])
		}

	case "/logout":
		saveAuth(nil)
		m.DisconnectServer()
		fmt.Println("logged out")

	case "/settings":
		if len(args) == 0 {
			mode := m.GetTransportMode()
			fmt.Printf("Transport mode: %s\n", mode)
			fmt.Println("  /settings p2p     – direct P2P only")
			fmt.Println("  /settings server  – relay server only")
			fmt.Println("  /settings both    – both (default)")
			return
		}
		mode := core.TransportMode(args[0])
		if err := m.SetTransportMode(mode); err != nil {
			fmt.Println("error:", err)
			return
		}
		fmt.Println("transport mode set to", mode)

	case "/name":
		if len(args) == 0 {
			fmt.Println("usage: /name <displayName>")
			return
		}
		m.SetDisplayName(strings.Join(args, " "))
		fmt.Println("display name updated")

	case "/peers":
		sr := m.ServerRelay()
		if sr != nil && sr.Connected() {
			resp, err := http.Get("http://" + sr.Address() + "/peers")
			if err == nil {
				var body map[string]interface{}
				json.NewDecoder(resp.Body).Decode(&body)
				resp.Body.Close()
				if body["ok"] == true {
					peersRaw, _ := json.Marshal(body["peers"])
					var serverPeers []struct {
						Username    string `json:"username"`
						Nickname    string `json:"nickname"`
						NodeID      string `json:"node_id"`
						Fingerprint string `json:"fingerprint"`
						Online      bool   `json:"online"`
					}
					json.Unmarshal(peersRaw, &serverPeers)
					if len(serverPeers) > 0 {
						fmt.Println("── Registered users ──")
						for _, p := range serverPeers {
							status := "offline"
							if p.Online {
								status = "online"
							}
							name := p.Username
							if p.Nickname != "" {
								name = p.Nickname + " (" + p.Username + ")"
							}
							fmt.Printf("  %s  %s  [%s:%s]\n",
								name, status, p.NodeID, p.Fingerprint)
						}
					} else {
						fmt.Println("no registered users yet")
					}
				}
			}
		}
		// Show local peers
		peers, _ := m.ListPeers()
		if len(peers) > 0 {
			fmt.Println("── Local contacts ──")
			for _, p := range peers {
				fmt.Printf("  %s  %s  [%s]\n",
					p.ID.NodeID, p.DisplayName, statusName(p.Status))
			}
		}
		if (sr == nil || !sr.Connected()) && len(peers) == 0 {
			fmt.Println("no known peers")
		}

	case "/networks":
		nets := m.ListNetworks()
		for _, n := range nets {
			fmt.Println(" ", n)
		}

	case "/join":
		if len(args) == 0 {
			fmt.Println("usage: /join <networkID>")
			return
		}
		if err := m.JoinNetwork(core.NetworkID(args[0])); err != nil {
			fmt.Println("error:", err)
		} else {
			fmt.Println("joined", args[0])
		}

	case "/chats":
		chats, _ := m.ListChats()
		if len(chats) == 0 {
			fmt.Println("no chats yet")
			return
		}
		for _, c := range chats {
			fmt.Printf("  %-36s  %-20s  members:%d\n",
				c.ID, c.Name, len(c.Members))
		}

	case "/dm":
		if len(args) == 0 {
			fmt.Println("usage: /dm <nodeID:fingerprint>")
			fmt.Println("Hint: use /peers to find user addresses")
			return
		}
		peer := parsePeerID(args[0])
		chat, err := m.CreateChat("", []core.PeerID{peer})
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		m.SendHandshake(ctx, peer.NodeID)
		fmt.Println("DM chat created:", chat.ID)

	case "/new":
		if len(args) < 2 {
			fmt.Println("usage: /new <name> <nodeID:fp> [nodeID:fp ...]")
			return
		}
		name := args[0]
		var members []core.PeerID
		for _, a := range args[1:] {
			members = append(members, parsePeerID(a))
		}
		chat, err := m.CreateChat(name, members)
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		fmt.Println("group chat created:", chat.ID)

	case "/msg":
		if len(args) < 2 {
			fmt.Println("usage: /msg <chatID> <text...>")
			return
		}
		chatID := core.ChatID(args[0])
		text := strings.Join(args[1:], " ")
		msg, err := m.SendText(ctx, chatID, text, nil)
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		fmt.Printf("[sent %s]\n", msg.ID)

	case "/file":
		if len(args) < 2 {
			fmt.Println("usage: /file <chatID> <path>")
			return
		}
		chatID := core.ChatID(args[0])
		path := args[1]
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Println("read error:", err)
			return
		}
		msg, err := m.SendFile(ctx, chatID, path, data)
		if err != nil {
			fmt.Println("send error:", err)
			return
		}
		fmt.Printf("[sent file %s, size %d, id %s]\n", path, len(data), msg.ID)

	case "/history":
		if len(args) == 0 {
			fmt.Println("usage: /history <chatID> [limit]")
			return
		}
		chatID := core.ChatID(args[0])
		limit := 20
		if len(args) > 1 {
			fmt.Sscanf(args[1], "%d", &limit)
		}
		msgs, err := m.GetMessages(chatID, limit, 0)
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		for i := len(msgs) - 1; i >= 0; i-- {
			msg := msgs[i]
			ts := msg.SentAt.Format("15:04:05")
			fmt.Printf("[%s] %s: %s\n",
				ts,
				msg.SenderID.NodeID,
				truncate(string(msg.Payload), 120),
			)
		}

	case "/call":
		if len(args) == 0 {
			fmt.Println("usage: /call <chatID>")
			return
		}
		sess, err := m.Calls().InitiateCall(ctx, core.ChatID(args[0]), core.CallVoice)
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		fmt.Println("call initiated, session:", sess.ID)

	case "/vcall":
		if len(args) == 0 {
			fmt.Println("usage: /vcall <chatID>")
			return
		}
		sess, err := m.Calls().InitiateCall(ctx, core.ChatID(args[0]), core.CallVideo)
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		fmt.Println("video call initiated, session:", sess.ID)

	case "/hangup":
		if len(args) == 0 {
			fmt.Println("usage: /hangup <sessionID>")
			return
		}
		if err := m.Calls().HangUp(ctx, args[0]); err != nil {
			fmt.Println("error:", err)
			return
		}
		fmt.Println("call ended")

	case "/update":
		fmt.Println("Checking for updates...")
		cmd := exec.Command("git", "pull", "--ff-only")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Println("Update failed:", err)
			return
		}
		fmt.Println("Rebuilding...")
		build := exec.Command("go", "build", "-tags", "fts5", "-o", "zerolink", "./cmd/client")
		build.Stdout = os.Stdout
		build.Stderr = os.Stderr
		if err := build.Run(); err != nil {
			fmt.Println("Rebuild failed:", err)
			return
		}
		fmt.Println("✓ Updated and rebuilt. Restart Zerolink to use the new version.")

	default:
		fmt.Println("unknown command:", cmd)
	}
}

func printHelp() {
	fmt.Println("\nCommands:")
	fmt.Println("  /help                        – show this help")
	fmt.Println("  /join_server <addr>          – connect to relay server")
	fmt.Println("  /register <user> <pass> [nick] – register account")
	fmt.Println("  /login <user> <pass>         – login to existing account")
	fmt.Println("  /logout                      – logout and disconnect")
	fmt.Println("  /settings [p2p|server|both]  – show/set transport mode")
	fmt.Println("  /chats                       – list chats")
	fmt.Println("  /new <name> <nodeID:fp ...>  – create group chat")
	fmt.Println("  /dm <nodeID:fp>              – start direct message")
	fmt.Println("  /msg <chatID> <text>         – send text message")
	fmt.Println("  /file <chatID> <path>        – send file")
	fmt.Println("  /history <chatID> [limit]    – print message history")
	fmt.Println("  /call <chatID>               – start voice call")
	fmt.Println("  /vcall <chatID>              – start video call")
	fmt.Println("  /hangup <sessionID>          – end call")
	fmt.Println("  /name <displayName>          – set your display name")
	fmt.Println("  /peers                       – list known peers")
	fmt.Println("  /networks                    – list joined networks")
	fmt.Println("  /join <networkID>            – join a ZeroTier network")
	fmt.Println("  /quit                        – exit")
	fmt.Println()
}

// ─── Auth persistence ────────────────────────────────────────────────────

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

func httpPostJSON(url string, data map[string]string) (map[string]string, error) {
	body, _ := json.Marshal(data)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("bad response: %s", string(respBody))
	}
	strResult := make(map[string]string)
	for k, v := range result {
		if s, ok := v.(string); ok {
			strResult[k] = s
		} else {
			strResult[k] = fmt.Sprintf("%v", v)
		}
	}
	return strResult, nil
}

func isZerotierIP(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		// ZeroTier uses 10.x.x.x and 192.168.x.x (but those can also be LAN)
		// 10.x.x.x with 10.145.x.x is common ZeroTier range
		if ip4[0] == 10 {
			return true
		}
	}
	return false
}

func doInstall() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	dst := "/usr/local/bin/zerolink"
	fmt.Printf("Installing to %s ...\n", dst)
	// Use cp via shell since binary can't overwrite itself while running
	if err := runCmd("cp", exe, dst); err != nil {
		fmt.Println("Error:", err)
		return
	}
	fmt.Println("✓ Installed to /usr/local/bin/zerolink")
	fmt.Println("  Now you can run: zerolink, zerolink -gui, zerolink -version")
}

func doUninstall() {
	runCmd("rm", "-f", "/usr/local/bin/zerolink", "/usr/local/bin/zerolink-server")
	fmt.Println("✓ Zerolink removed from system")
}

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func checkUpdate() {
	resp, err := http.Get("https://api.github.com/repos/secureNqwer/zerolink/releases/latest")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var rel struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return
	}
	if rel.TagName != "" && strings.TrimPrefix(rel.TagName, "v") > strings.TrimPrefix(version.Version, "v") {
		fmt.Printf("\nUpdate available: %s → %s\n", version.Version, rel.TagName)
		fmt.Printf("  Download: https://github.com/secureNqwer/zerolink/releases/tag/%s\n", rel.TagName)
	}
}

func isValidUsername(s string) bool {
	if len(s) < 3 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_') {
			return false
		}
	}
	return true
}

// ─── Event printing ──────────────────────────────────────────────────────

func printEvent(evt core.Event) {
	ts := evt.Timestamp.Format("15:04:05")
	switch evt.Type {
	case core.EvtMessageReceived:
		if msg, ok := evt.Data.(*core.Message); ok {
			fmt.Printf("\n[%s] ← %s: %s\n> ",
				ts, msg.SenderID.NodeID,
				truncate(string(msg.Payload), 80))
		}
	case core.EvtPeerOnline:
		if p, ok := evt.Data.(*core.Peer); ok {
			fmt.Printf("\n[%s] ● %s online\n> ", ts, p.DisplayName)
		}
	case core.EvtPeerOffline:
		if p, ok := evt.Data.(*core.Peer); ok {
			fmt.Printf("\n[%s] ○ %s offline\n> ", ts, p.DisplayName)
		}
	case core.EvtCallIncoming:
		if s, ok := evt.Data.(*core.CallSession); ok {
			fmt.Printf("\n[%s] incoming call from %s  session=%s\n> ",
				ts, s.Initiator.NodeID, s.ID)
		}
	}
}

func parsePeerID(s string) core.PeerID {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) == 2 {
		return core.PeerID{NodeID: core.NodeID(parts[0]), Fingerprint: parts[1]}
	}
	return core.PeerID{NodeID: core.NodeID(s)}
}

func statusName(s core.PeerStatus) string {
	switch s {
	case core.PeerOnline:
		return "online"
	case core.PeerAway:
		return "away"
	case core.PeerBusy:
		return "busy"
	default:
		return "offline"
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

var _ = time.Now

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yourorg/messenger-core/server"
)

func main() {
	cfgPath := flag.String("config", "server.json", "path to server config JSON")
	flag.Parse()

	cfg := server.DefaultServerConfig()
	if data, err := os.ReadFile(*cfgPath); err == nil {
		if err := json.Unmarshal(data, cfg); err != nil {
			panic("invalid config: " + err.Error())
		}
		fmt.Printf("Config loaded: zt_network=%q\n", cfg.ZTNetwork)
	} else {
		fmt.Printf("No config file (%s), using defaults\n", *cfgPath)
	}

	// ─── Create server ─────────────────────────────────────────────────
	srv, err := server.NewServer(cfg)
	if err != nil {
		log.Fatal("create server:", err)
	}

	// ─── Print addresses ───────────────────────────────────────────────
	port := portFromAddr(cfg.ListenAddr)
	fmt.Println("\n─── Messenger Server ───")
	fmt.Printf("  Port: %s\n", port)

	ztNetwork := cfg.ZTNetwork
	if ztNetwork != "" {
		ztIP := systemZTIP(ztNetwork)
		if ztIP != "" {
			fmt.Printf("  Give your friend: %s\n", net.JoinHostPort(ztIP, port))
			fmt.Println("  (via system ZeroTier – friend must be on same ZT network)")
		} else {
			fmt.Printf("  ZT network %s configured but no IP found.\n", ztNetwork)
			fmt.Println("  Make sure ZeroTier is running and you're on this network.")
			fmt.Printf("  Run: zerotier-cli join %s\n", ztNetwork)
		}
	} else {
		sysZTs := detectSystemZTNetworks()
		if len(sysZTs) > 0 {
			fmt.Println("  ZeroTier networks detected:")
			for _, n := range sysZTs {
				fmt.Printf("    %s\n", n)
			}
			fmt.Println("  Add 'zt_network' to server.json to use one.")
		}
		ifaceIP := primaryIP()
		fmt.Printf("  LAN address: %s\n", net.JoinHostPort(ifaceIP, port))
	}
	fmt.Println()

	// ─── Graceful shutdown ─────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-quit
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		fmt.Println("\nshutting down...")
		if err := srv.Stop(ctx); err != nil {
			log.Printf("server shutdown error: %v", err)
		}
	}()

	if err := srv.Start(); err != nil {
		log.Println("server stopped:", err)
	}
}

func systemZTIP(networkID string) string {
	// Try to match ZT network by checking routes/IPs
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if len(iface.Name) < 2 || (iface.Name[:2] != "zt" && iface.Name[:2] != "ZW") {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil {
				continue
			}
			return ip4.String()
		}
	}
	return ""
}

func detectSystemZTNetworks() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var nets []string
	seen := make(map[string]bool)
	for _, iface := range ifaces {
		if len(iface.Name) < 2 || (iface.Name[:2] != "zt" && iface.Name[:2] != "ZW") {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil {
				continue // skip IPv6
			}
			n := fmt.Sprintf("  %s (%s)", ip4.String(), iface.Name)
			if !seen[n] {
				seen[n] = true
				nets = append(nets, n)
			}
		}
	}
	return nets
}

func primaryIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() {
				continue
			}
			return ip.String()
		}
	}
	return "127.0.0.1"
}

func portFromAddr(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "8080"
	}
	return port
}

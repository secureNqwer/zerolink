//go:build cgo

// Package transport implements the ZeroTier-based network transport.
// It embeds libzerotiercore via CGO so the end-user does NOT need ZeroTier One installed.
//
// CGO build requirements:
//   - libzerotiercore.a (or .so) in CGO_LIBRARY_PATH
//   - ZeroTierOne/include/ZeroTierSockets.h in CGO_INCLUDE_PATH
//
// Build with:
//   CGO_LDFLAGS="-L/path/to/zt/lib -lzerotiercore -lstdc++" go build ./...
package transport

/*
#cgo CFLAGS:  -I${SRCDIR}/../vendor/zerotier/include -DZTS_STATIC
#cgo LDFLAGS: -L${SRCDIR}/../vendor/zerotier/lib -lzerotiercore -lstdc++ -lm
#cgo windows LDFLAGS: -lws2_32 -liphlpapi -lshlwapi -static -static-libgcc -static-libstdc++

#include "ZeroTierSockets.h"
#include <stdlib.h>
#include <string.h>

// Callbacks are defined in zt_callbacks.c (included via CGO)
void ztNodeEventCallback(void* uptr, void* tptr, zts_event_t eventCode, void* metaData);
void ztPeerEventCallback(void* uptr, void* tptr, zts_event_t eventCode, void* metaData);
void ztNetworkEventCallback(void* uptr, void* tptr, zts_event_t eventCode, void* metaData);
*/
import "C"

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"go.uber.org/zap"

	"github.com/secureNqwer/zerolink/core"
)

const (
	ztUDPPort  = 7777 // messenger app UDP port inside the ZT virtual network
	maxPktSize = 65507
)

// ─── ZeroTier Transport ───────────────────────────────────────────────────────

// ZTTransport implements core.Transport via embedded libzerotiercore.
type ZTTransport struct {
	mu       sync.RWMutex
	cfg      *core.Config
	log      *zap.Logger
	nodeID   core.NodeID
	networks map[core.NetworkID]struct{}
	peers    map[core.NodeID]*peerEntry
	recvCh   chan *core.IncomingPacket
	conn     *net.UDPConn // virtual UDP socket inside ZT network
	started  bool
	stopCh   chan struct{}
	wg       sync.WaitGroup
	// eventBus is injected by the engine so ZT C callbacks can publish events.
	eventBus core.EventBus
}

type peerEntry struct {
	NodeID    core.NodeID
	VirtualIP net.IP // ZT-assigned IP inside the network
	LastSeen  time.Time
	Latency   time.Duration
}

// NewZTTransport creates but does not start the ZeroTier transport.
func NewZTTransport(cfg *core.Config, log *zap.Logger) *ZTTransport {
	return &ZTTransport{
		cfg:      cfg,
		log:      log,
		networks: make(map[core.NetworkID]struct{}),
		peers:    make(map[core.NodeID]*peerEntry),
		recvCh:   make(chan *core.IncomingPacket, 1024),
		stopCh:   make(chan struct{}),
	}
}

// Start initialises libzerotiercore, reads or creates the identity from disk,
// and opens a virtual UDP listener on ztUDPPort.
func (t *ZTTransport) Start(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started {
		return errors.New("transport: already started")
	}

	// ── Init ZeroTier node ────────────────────────────────────────────────
	dataDir := C.CString(t.cfg.ZeroTierDataDir)
	defer C.free(unsafe.Pointer(dataDir))

	// zts_init_from_storage initialises the node and loads/creates an identity.
	ret := C.zts_init_from_storage(dataDir)
	if ret != C.ZTS_ERR_OK {
		return fmt.Errorf("transport: zts_init_from_storage failed: %d", ret)
	}

	// Register event callbacks (implemented in zt_callbacks.c)
	C.zts_node_start()

	// Wait for the node to come online (up to 30 s)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if C.zts_node_is_online() == 1 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if C.zts_node_is_online() != 1 {
		return errors.New("transport: ZeroTier node failed to come online")
	}

	// Read our node ID
	nid := C.zts_node_get_id()
	t.nodeID = core.NodeID(fmt.Sprintf("%010x", uint64(nid)))
	t.log.Info("ZeroTier node online", zap.String("node_id", string(t.nodeID)))

	// Join configured networks
	for _, nwid := range t.cfg.Networks {
		if err := t.joinNetworkLocked(nwid); err != nil {
			t.log.Warn("failed to join network", zap.String("network", string(nwid)), zap.Error(err))
		}
	}

	// Open UDP socket (ZeroTier virtual networking)
	addr := &net.UDPAddr{Port: int(t.cfg.ListenPort)}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("transport: failed to listen UDP: %w", err)
	}
	t.conn = conn
	t.started = true

	t.wg.Add(1)
	go t.recvLoop()

	return nil
}

// Stop shuts down the ZeroTier node.
func (t *ZTTransport) Stop() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.started {
		return nil
	}
	close(t.stopCh)
	t.conn.Close()
	t.wg.Wait()
	C.zts_node_stop()
	C.zts_node_free()
	t.started = false
	return nil
}

// NodeID returns the local ZeroTier address.
func (t *ZTTransport) NodeID() core.NodeID {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.nodeID
}

// Networks returns all joined network IDs.
func (t *ZTTransport) Networks() []core.NetworkID {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]core.NetworkID, 0, len(t.networks))
	for n := range t.networks {
		out = append(out, n)
	}
	return out
}

// JoinNetwork joins a ZeroTier network and waits for an IP assignment.
func (t *ZTTransport) JoinNetwork(ctx context.Context, id core.NetworkID) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.joinNetworkLocked(id)
}

func (t *ZTTransport) joinNetworkLocked(id core.NetworkID) error {
	nwid, err := parseNetworkID(id)
	if err != nil {
		return err
	}
	ret := C.zts_net_join(C.uint64_t(nwid))
	if ret != C.ZTS_ERR_OK {
		return fmt.Errorf("transport: zts_net_join failed: %d", ret)
	}
	t.networks[id] = struct{}{}
	t.log.Info("joined ZeroTier network", zap.String("network", string(id)))
	return nil
}

// LeaveNetwork leaves a ZeroTier network.
func (t *ZTTransport) LeaveNetwork(_ context.Context, id core.NetworkID) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	nwid, err := parseNetworkID(id)
	if err != nil {
		return err
	}
	ret := C.zts_net_leave(C.uint64_t(nwid))
	if ret != C.ZTS_ERR_OK {
		return fmt.Errorf("transport: zts_net_leave failed: %d", ret)
	}
	delete(t.networks, id)
	return nil
}

// Send transmits a packet to a peer.
// It looks up the peer's virtual IP and sends a UDP datagram.
func (t *ZTTransport) Send(_ context.Context, to core.NodeID, pkt *core.Packet) error {
	t.mu.RLock()
	peer, ok := t.peers[to]
	t.mu.RUnlock()
	if !ok {
		// Try resolving via ZeroTier API
		ip, err := t.resolveNodeIP(to)
		if err != nil {
			return fmt.Errorf("transport: unknown peer %s: %w", to, err)
		}
		t.mu.Lock()
		peer = &peerEntry{NodeID: to, VirtualIP: ip, LastSeen: time.Now()}
		t.peers[to] = peer
		t.mu.Unlock()
	}

	raw, err := marshalPacket(pkt)
	if err != nil {
		return err
	}

	addr := &net.UDPAddr{IP: peer.VirtualIP, Port: int(t.cfg.ListenPort)}
	_, err = t.conn.WriteToUDP(raw, addr)
	return err
}

// Broadcast sends a packet to all known peers in a network.
func (t *ZTTransport) Broadcast(_ context.Context, network core.NetworkID, pkt *core.Packet) error {
	raw, err := marshalPacket(pkt)
	if err != nil {
		return err
	}

	// ZeroTier multicast: 224.0.0.0/4 range or ff02:: for IPv6.
	// For simplicity we use the managed group broadcast on 255.255.255.255 within the VLAN.
	addr := &net.UDPAddr{
		IP:   net.IPv4bcast,
		Port: int(t.cfg.ListenPort),
	}
	_ = network // future: per-network multicast address
	_, err = t.conn.WriteToUDP(raw, addr)
	return err
}

// Recv returns the incoming packet channel.
func (t *ZTTransport) Recv() <-chan *core.IncomingPacket {
	return t.recvCh
}

// PeerReachable checks whether the ZeroTier node can reach a peer directly.
func (t *ZTTransport) PeerReachable(id core.NodeID) bool {
	nid, err := parseNodeID(id)
	if err != nil {
		return false
	}
	return C.zts_core_query_path_count(C.uint64_t(nid)) > 0
}

// ─── Receive loop ─────────────────────────────────────────────────────────────

func (t *ZTTransport) recvLoop() {
	defer t.wg.Done()
	buf := make([]byte, maxPktSize)
	for {
		select {
		case <-t.stopCh:
			return
		default:
		}
		t.conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, addr, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			select {
			case <-t.stopCh:
				return
			default:
				t.log.Error("recv error", zap.Error(err))
				continue
			}
		}

		pkt, err := unmarshalPacket(buf[:n])
		if err != nil {
			t.log.Warn("malformed packet", zap.String("from", addr.String()), zap.Error(err))
			continue
		}

		// Update peer last-seen
		fromNode := core.NodeID(fmt.Sprintf("%010x", nodeIDFromIP(addr.IP)))
		t.mu.Lock()
		if pe, ok := t.peers[fromNode]; ok {
			pe.LastSeen = time.Now()
		} else {
			t.peers[fromNode] = &peerEntry{
				NodeID:   fromNode,
				VirtualIP: addr.IP,
				LastSeen: time.Now(),
			}
		}
		t.mu.Unlock()

		select {
		case t.recvCh <- &core.IncomingPacket{
			From:       fromNode,
			Pkt:        pkt,
			ReceivedAt: time.Now(),
		}:
		default:
			t.log.Warn("recv channel full, dropping packet")
		}
	}
}

// ─── Packet serialisation ─────────────────────────────────────────────────────

// marshalPacket serialises a core.Packet into a byte slice.
//
// Wire format (big-endian):
//   [4]  magic
//   [1]  version
//   [1]  type
//   [1]  flags
//   [1]  ttl
//   [16] packet_id
//   [5]  sender_node
//   [8]  timestamp (unix nano)
//   [4]  body_len
//   [N]  body
//   [32] hmac
func marshalPacket(p *core.Packet) ([]byte, error) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, p.Magic)
	buf.WriteByte(p.Version)
	buf.WriteByte(uint8(p.Type))
	buf.WriteByte(uint8(p.Flags))
	buf.WriteByte(p.TTL)
	buf.Write(p.PacketID[:])
	buf.Write(p.SenderNode[:])
	binary.Write(&buf, binary.BigEndian, p.Timestamp)
	binary.Write(&buf, binary.BigEndian, uint32(len(p.Body)))
	buf.Write(p.Body)
	buf.Write(p.HMAC[:])
	return buf.Bytes(), nil
}

// unmarshalPacket deserialises a raw byte slice into a core.Packet.
func unmarshalPacket(data []byte) (*core.Packet, error) {
	const headerSize = 4 + 1 + 1 + 1 + 1 + 16 + 5 + 8 + 4 // 41 bytes
	const hmacSize = 32
	if len(data) < headerSize+hmacSize {
		return nil, fmt.Errorf("transport: packet too short (%d bytes)", len(data))
	}

	r := bytes.NewReader(data)
	p := &core.Packet{}

	binary.Read(r, binary.BigEndian, &p.Magic)
	if p.Magic != core.PacketMagic {
		return nil, fmt.Errorf("transport: bad magic 0x%08X", p.Magic)
	}
	p.Version, _ = r.ReadByte()
	if p.Version != core.PacketVersion {
		return nil, fmt.Errorf("transport: unsupported version %d", p.Version)
	}
	t, _ := r.ReadByte()
	p.Type = core.MessageType(t)
	f, _ := r.ReadByte()
	p.Flags = core.PacketFlags(f)
	p.TTL, _ = r.ReadByte()
	r.Read(p.PacketID[:])
	r.Read(p.SenderNode[:])
	binary.Read(r, binary.BigEndian, &p.Timestamp)

	var bodyLen uint32
	binary.Read(r, binary.BigEndian, &bodyLen)
	if uint32(r.Len())-hmacSize < bodyLen {
		return nil, fmt.Errorf("transport: body_len %d exceeds remaining data", bodyLen)
	}
	p.Body = make([]byte, bodyLen)
	r.Read(p.Body)
	r.Read(p.HMAC[:])
	return p, nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func parseNetworkID(id core.NetworkID) (uint64, error) {
	b, err := hex.DecodeString(string(id))
	if err != nil || len(b) != 8 {
		return 0, fmt.Errorf("transport: invalid network ID %q", id)
	}
	return binary.BigEndian.Uint64(b), nil
}

func parseNodeID(id core.NodeID) (uint64, error) {
	if len(id) != 10 {
		return 0, fmt.Errorf("transport: invalid node ID %q", id)
	}
	var v uint64
	_, err := fmt.Sscanf(string(id), "%x", &v)
	return v, err
}

// resolveNodeIP asks ZeroTier for the virtual IP of a peer node within the
// first joined network. For multi-network setups this should be extended.
func (t *ZTTransport) resolveNodeIP(id core.NodeID) (net.IP, error) {
	nid, err := parseNodeID(id)
	if err != nil {
		return nil, err
	}

	t.mu.RLock()
	var netID core.NetworkID
	for nwid := range t.networks {
		netID = nwid
		break
	}
	t.mu.RUnlock()

	if netID == "" {
		return nil, errors.New("transport: no networks joined")
	}

	nwid, err := parseNetworkID(netID)
	if err != nil {
		return nil, err
	}

	// Compute RFC4193 IPv6 address for the node
	var buf [64]C.char
	ret := C.zts_addr_compute_rfc4193_str(C.uint64_t(nwid), C.uint64_t(nid), &buf[0], 64)
	if ret == C.ZTS_ERR_OK {
		ipStr := C.GoString(&buf[0])
		if ip := net.ParseIP(ipStr); ip != nil {
			return ip, nil
		}
	}

	// Fallback/heuristic for IPv4: ZeroTier uses last two bytes of node ID
	// inside the managed IPv4 subnet. We query local IP to find the subnet prefix.
	var localAddr C.struct_zts_sockaddr_storage
	if C.zts_addr_get(C.uint64_t(nwid), C.ZTS_AF_INET, &localAddr) == C.ZTS_ERR_OK {
		localIP := sockaddrToIP(&localAddr)
		if localIP != nil {
			ip4 := localIP.To4()
			if ip4 != nil {
				peerIP := make(net.IP, 4)
				peerIP[0] = ip4[0]
				peerIP[1] = ip4[1]
				peerIP[2] = byte((nid >> 8) & 0xFF)
				peerIP[3] = byte(nid & 0xFF)
				return peerIP, nil
			}
		}
	}

	return nil, fmt.Errorf("transport: could not resolve IP for node %s", id)
}

// nodeIDFromIP extracts a ZeroTier node ID from a ZT virtual IP address.
// ZT encodes the 40-bit node ID in the last 5 bytes of a /104 IPv6 block,
// or in the last 2 octets of the 10.x.x.x/8 IPv4 assignment.
// This is a simplified heuristic – production should use the proper mapping.
// GetManagedIP returns the ZeroTier-managed IPv4 address for a given network.
// Blocks until the network is ready or a timeout.
func (t *ZTTransport) GetManagedIP(networkID core.NetworkID, timeout time.Duration) (net.IP, error) {
	nid, err := strconv.ParseUint(string(networkID), 16, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid network ID: %s", networkID)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var addr C.struct_zts_sockaddr_storage
		ret := C.zts_addr_get(C.uint64_t(nid), C.ZTS_AF_INET, &addr)
		if ret == 0 {
			if ip := sockaddrToIP(&addr); ip != nil {
				return ip, nil
			}
		}
		time.Sleep(time.Second)
	}
	return nil, fmt.Errorf("timed out waiting for managed IP on network %s", networkID)
}

func nodeIDFromIP(ip net.IP) uint64 {
	if ip4 := ip.To4(); ip4 != nil {
		return uint64(ip4[2])<<8 | uint64(ip4[3])
	}
	// IPv6: last 5 bytes encode the node ID
	if len(ip) == 16 {
		return uint64(ip[11])<<32 | uint64(ip[12])<<24 |
			uint64(ip[13])<<16 | uint64(ip[14])<<8 | uint64(ip[15])
	}
	return 0
}

func sockaddrToIP(sa *C.struct_zts_sockaddr_storage) net.IP {
	// Cast to sockaddr_in or sockaddr_in6 based on family
	family := (*C.struct_zts_sockaddr)(unsafe.Pointer(sa)).sa_family
	switch family {
	case C.ZTS_AF_INET:
		sin := (*C.struct_zts_sockaddr_in)(unsafe.Pointer(sa))
		b := (*[4]byte)(unsafe.Pointer(&sin.sin_addr))
		return net.IP(b[:])
	case C.ZTS_AF_INET6:
		sin6 := (*C.struct_zts_sockaddr_in6)(unsafe.Pointer(sa))
		b := (*[16]byte)(unsafe.Pointer(&sin6.sin6_addr))
		return net.IP(b[:])
	}
	return nil
}

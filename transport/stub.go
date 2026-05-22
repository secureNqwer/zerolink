//go:build !cgo

package transport

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/yourorg/messenger-core/core"
)

// ZTTransport is a stub for platforms without CGO/libzt.
// Communication is only possible via the relay server.
type ZTTransport struct {
	mu     sync.RWMutex
	recvCh chan *core.IncomingPacket
}

func NewZTTransport(cfg *core.Config, log interface{}) *ZTTransport {
	return &ZTTransport{
		recvCh: make(chan *core.IncomingPacket, 64),
	}
}

func (t *ZTTransport) Start(ctx context.Context) error { return nil }
func (t *ZTTransport) Stop() error                     { return nil }
func (t *ZTTransport) NodeID() core.NodeID             { return "0000000000" }
func (t *ZTTransport) Networks() []core.NetworkID      { return nil }
func (t *ZTTransport) PeerReachable(id core.NodeID) bool { return false }

func (t *ZTTransport) Send(ctx context.Context, to core.NodeID, pkt *core.Packet) error {
	return errors.New("transport: not available (no libzt)")
}

func (t *ZTTransport) Broadcast(ctx context.Context, network core.NetworkID, pkt *core.Packet) error {
	return errors.New("transport: not available (no libzt)")
}

func (t *ZTTransport) Recv() <-chan *core.IncomingPacket {
	return t.recvCh
}

func (t *ZTTransport) JoinNetwork(ctx context.Context, id core.NetworkID) error {
	return errors.New("transport: not available (no libzt)")
}

func (t *ZTTransport) LeaveNetwork(ctx context.Context, id core.NetworkID) error {
	return errors.New("transport: not available (no libzt)")
}

func (t *ZTTransport) SetEventBus(bus core.EventBus) {}

func (t *ZTTransport) GetManagedIP(networkID core.NetworkID, timeout time.Duration) (net.IP, error) {
	return nil, errors.New("transport: not available (no libzt)")
}

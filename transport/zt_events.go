//go:build cgo

package transport

/*
#cgo CFLAGS: -DZTS_STATIC
#include "ZeroTierSockets.h"
*/
import "C"
import (
	"fmt"
	"time"
	"unsafe"

	"go.uber.org/zap"

	"github.com/yourorg/messenger-core/core"
)

// ─── Event code constants (from ZeroTierSockets.h) ───────────────────────────

const (
	ZTS_EVENT_NODE_UP          = 200
	ZTS_EVENT_NODE_ONLINE       = 201
	ZTS_EVENT_NODE_OFFLINE      = 202
	ZTS_EVENT_NODE_DOWN         = 203
	ZTS_EVENT_NODE_FATAL_ERROR  = 204

	ZTS_EVENT_NETWORK_NOT_FOUND = 210
	ZTS_EVENT_NETWORK_CLIENT_TOO_OLD = 211
	ZTS_EVENT_NETWORK_REQ_CONFIG    = 212
	ZTS_EVENT_NETWORK_OK            = 213
	ZTS_EVENT_NETWORK_ACCESS_DENIED = 214
	ZTS_EVENT_NETWORK_READY_IP4     = 215
	ZTS_EVENT_NETWORK_READY_IP6     = 216
	ZTS_EVENT_NETWORK_DOWN          = 217
	ZTS_EVENT_NETWORK_UPDATE        = 218

	ZTS_EVENT_PEER_DIRECT    = 220
	ZTS_EVENT_PEER_RELAY     = 221
	ZTS_EVENT_PEER_UNREACHABLE = 222
	ZTS_EVENT_PEER_PATH_DISCOVERED = 223
	ZTS_EVENT_PEER_PATH_DEAD = 224
)

// globalBus is set by ZTTransport.Start() so callbacks can publish events.
// Using a package-level pointer is safe because only one ZT node runs per process.
var globalTransport *ZTTransport

//export goZTNodeEvent
func goZTNodeEvent(eventCode C.int, _ unsafe.Pointer) {
	if globalTransport == nil {
		return
	}
	log := globalTransport.log
	switch int(eventCode) {
	case ZTS_EVENT_NODE_UP:
		log.Debug("ZeroTier node up")
	case ZTS_EVENT_NODE_ONLINE:
		log.Info("ZeroTier node online")
	case ZTS_EVENT_NODE_OFFLINE:
		log.Warn("ZeroTier node offline")
		globalTransport.bus().Publish(core.Event{
			Type:      core.EvtNetworkLeft,
			Timestamp: time.Now(),
		})
	case ZTS_EVENT_NODE_DOWN:
		log.Warn("ZeroTier node down")
	case ZTS_EVENT_NODE_FATAL_ERROR:
		log.Error("ZeroTier node fatal error")
	}
}

//export goZTNetworkEvent
func goZTNetworkEvent(eventCode C.int, metaData unsafe.Pointer) {
	if globalTransport == nil {
		return
	}
	log := globalTransport.log
	switch int(eventCode) {
	case ZTS_EVENT_NETWORK_OK:
		log.Debug("ZeroTier network OK")
	case ZTS_EVENT_NETWORK_READY_IP4, ZTS_EVENT_NETWORK_READY_IP6:
		log.Info("ZeroTier network ready")
		globalTransport.bus().Publish(core.Event{
			Type:      core.EvtNetworkJoined,
			Timestamp: time.Now(),
		})
	case ZTS_EVENT_NETWORK_ACCESS_DENIED:
		log.Warn("ZeroTier network access denied")
	case ZTS_EVENT_NETWORK_DOWN:
		log.Warn("ZeroTier network down")
		globalTransport.bus().Publish(core.Event{
			Type:      core.EvtNetworkLeft,
			Timestamp: time.Now(),
		})
	case ZTS_EVENT_NETWORK_NOT_FOUND:
		log.Warn("ZeroTier network not found")
	}
}

//export goZTPeerEvent
func goZTPeerEvent(eventCode C.int, metaData unsafe.Pointer) {
	if globalTransport == nil || metaData == nil {
		return
	}
	// peer details are in a zts_peer_info_t struct
	// Cast and read the node ID
	info := (*C.zts_peer_info_t)(metaData)
	nodeID := core.NodeID(nodeIDHex(uint64(info.peer_id)))

	switch int(eventCode) {
	case ZTS_EVENT_PEER_DIRECT:
		globalTransport.log.Debug("peer direct path", zap.String("node", string(nodeID)))
		globalTransport.bus().Publish(core.Event{
			Type:      core.EvtPeerOnline,
			Timestamp: time.Now(),
			Data:      nodeID,
		})
	case ZTS_EVENT_PEER_RELAY:
		globalTransport.log.Debug("peer via relay", zap.String("node", string(nodeID)))
	case ZTS_EVENT_PEER_UNREACHABLE:
		globalTransport.log.Debug("peer unreachable", zap.String("node", string(nodeID)))
		globalTransport.bus().Publish(core.Event{
			Type:      core.EvtPeerOffline,
			Timestamp: time.Now(),
			Data:      nodeID,
		})
	}
}

// bus returns the event bus via the engine stored on the transport.
// The bus is injected by the engine after both are constructed.
func (t *ZTTransport) bus() core.EventBus {
	t.mu.RLock()
	b := t.eventBus
	t.mu.RUnlock()
	return b
}

// SetEventBus injects the shared event bus into the transport.
func (t *ZTTransport) SetEventBus(b core.EventBus) {
	t.mu.Lock()
	t.eventBus = b
	globalTransport = t
	t.mu.Unlock()
}

func nodeIDHex(nid uint64) string {
	return fmt.Sprintf("%010x", nid)
}

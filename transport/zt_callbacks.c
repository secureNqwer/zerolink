/*
 * zt_callbacks.c
 * CGO implementation of ZeroTier node/network/peer event callbacks.
 * This file is compiled as part of the transport package.
 *
 * The callbacks receive events from libzerotiercore and bridge them into Go
 * via a global Go-callable function (registered by the Go side).
 *
 * Build requirements:
 *   - libzerotiercore headers in vendor/zerotier/include/
 *   - libzerotiercore.a or .so in vendor/zerotier/lib/
 */

#include "ZeroTierSockets.h"
#include <stdio.h>
#include <string.h>

/* ── Forward-declared Go bridge (implemented in zerotier.go via //export) ── */
extern void goZTNodeEvent(int eventCode, void* metaData);
extern void goZTPeerEvent(int eventCode, void* metaData);
extern void goZTNetworkEvent(int eventCode, void* metaData);

/* ── Node event callback ─────────────────────────────────────────────────── */
void ztNodeEventCallback(void* uptr, void* tptr,
                         zts_event_t eventCode, void* metaData)
{
    (void)uptr; (void)tptr;
    goZTNodeEvent((int)eventCode, metaData);
}

/* ── Peer event callback ─────────────────────────────────────────────────── */
void ztPeerEventCallback(void* uptr, void* tptr,
                         zts_event_t eventCode, void* metaData)
{
    (void)uptr; (void)tptr;
    goZTPeerEvent((int)eventCode, metaData);
}

/* ── Network event callback ──────────────────────────────────────────────── */
void ztNetworkEventCallback(void* uptr, void* tptr,
                            zts_event_t eventCode, void* metaData)
{
    (void)uptr; (void)tptr;
    goZTNetworkEvent((int)eventCode, metaData);
}

/* ── Combined event router (used by newer libzerotiercore versions) ───────── */
void ztCombinedCallback(void* uptr, void* tptr,
                        zts_event_t eventCode, void* metaData)
{
    if (eventCode >= ZTS_EVENT_NODE_UP && eventCode <= ZTS_EVENT_NODE_FATAL_ERROR) {
        ztNodeEventCallback(uptr, tptr, eventCode, metaData);
    } else if (eventCode >= ZTS_EVENT_NETWORK_NOT_FOUND && eventCode <= ZTS_EVENT_NETWORK_UPDATE) {
        ztNetworkEventCallback(uptr, tptr, eventCode, metaData);
    } else if (eventCode >= ZTS_EVENT_PEER_DIRECT && eventCode <= ZTS_EVENT_PEER_PATH_DEAD) {
        ztPeerEventCallback(uptr, tptr, eventCode, metaData);
    }
}

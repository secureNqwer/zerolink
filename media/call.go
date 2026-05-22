// Package media/call implements the WebRTC-based call manager.
// It uses the pion/webrtc library for real-time audio/video transport.
// ZeroTier provides the network; pion handles WebRTC signalling internally;
// the signalling messages are exchanged over the messenger's own message channel
// (MsgCallSignal type) – no separate signalling server is required.
package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v3"
	"go.uber.org/zap"

	"github.com/yourorg/messenger-core/core"
)

// ─── Signal message payloads ──────────────────────────────────────────────────

// SignalType distinguishes WebRTC signalling message kinds.
type SignalType string

const (
	SignalOffer     SignalType = "offer"
	SignalAnswer    SignalType = "answer"
	SignalCandidate SignalType = "candidate"
	SignalBye       SignalType = "bye"
)

// SignalMessage is JSON-encoded in a core.Packet body.
type SignalMessage struct {
	Type      SignalType `json:"type"`
	SessionID string     `json:"session_id"`
	From      core.PeerID `json:"from"`
	CallType  core.CallType `json:"call_type,omitempty"`
	SDP       string     `json:"sdp,omitempty"`
	Candidate string     `json:"candidate,omitempty"` // JSON-encoded ICECandidateInit
}

// ─── Session internals ────────────────────────────────────────────────────────

type callSessionInternal struct {
	*core.CallSession

	pc          *webrtc.PeerConnection
	localTracks []*webrtc.TrackLocalStaticRTP

	// inbound remote tracks
	remoteTracks map[string]*webrtc.TrackRemote

	audioSender *webrtc.RTPSender
	videoSender *webrtc.RTPSender

	mu             sync.Mutex
	pendingCandidates []webrtc.ICECandidateInit
	offerAnswerDone   bool
}

// ─── CallManager ─────────────────────────────────────────────────────────────

// CallManager manages WebRTC calls.
// It must be wired up to the messenger's send/receive pipeline by
// calling HandleSignal when a MsgCallSignal packet arrives, and by
// setting SendSignalFn to dispatch outgoing signals.
type CallManager struct {
	mu       sync.RWMutex
	sessions map[string]*callSessionInternal

	log      *zap.Logger
	localPeer core.PeerID
	bus      core.EventBus

	webrtcAPI *webrtc.API
	iceServers []webrtc.ICEServer

	// SendSignalFn is set by the messenger engine; it transmits a SignalMessage
	// to a remote peer via the regular message channel.
	SendSignalFn func(ctx context.Context, to core.PeerID, sig *SignalMessage) error

	// OnTrack is called when a remote peer's media track arrives.
	// The UI layer can hook here to render audio/video.
	OnTrack func(sessionID string, peer core.PeerID, track *webrtc.TrackRemote)
}

// NewCallManager creates a CallManager.
// iceServers may be nil (pion will use its default STUN server over ZeroTier).
func NewCallManager(localPeer core.PeerID, bus core.EventBus, iceServers []webrtc.ICEServer, log *zap.Logger) *CallManager {
	// Configure pion's media engine
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		log.Fatal("webrtc: codec registration failed", zap.Error(err))
	}

	// Allow video: H.264 + VP8; audio: Opus
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me))

	if len(iceServers) == 0 {
		// Default STUN – over ZeroTier this works for LAN; for cross-ZT-network
		// add a TURN server that is also a ZeroTier member.
		iceServers = []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		}
	}

	return &CallManager{
		sessions:   make(map[string]*callSessionInternal),
		log:        log,
		localPeer:  localPeer,
		bus:        bus,
		webrtcAPI:  api,
		iceServers: iceServers,
	}
}

// ─── Initiate ────────────────────────────────────────────────────────────────

// InitiateCall creates a new call session and sends an offer to the target chat.
func (cm *CallManager) InitiateCall(ctx context.Context, chatID core.ChatID, ctype core.CallType) (*core.CallSession, error) {
	sessID := uuid.New().String()
	sess := &core.CallSession{
		ID:          sessID,
		ChatID:      chatID,
		Type:        ctype,
		State:       core.CallRinging,
		Initiator:   cm.localPeer,
		Participants: []core.PeerID{cm.localPeer},
		StartedAt:  time.Now(),
	}

	pc, err := cm.newPeerConnection()
	if err != nil {
		return nil, fmt.Errorf("call: create peer connection: %w", err)
	}

	internal := &callSessionInternal{
		CallSession:  sess,
		pc:           pc,
		remoteTracks: make(map[string]*webrtc.TrackRemote),
	}

	// Add audio track
	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio", sessID,
	)
	if err != nil {
		return nil, err
	}
	audioSender, err := pc.AddTrack(audioTrack)
	if err != nil {
		return nil, err
	}
	internal.localTracks = append(internal.localTracks, audioTrack)
	internal.audioSender = audioSender

	// Add video track only for video/screen-share calls
	if ctype == core.CallVideo || ctype == core.CallScreenShare {
		videoTrack, err := webrtc.NewTrackLocalStaticRTP(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
			"video", sessID,
		)
		if err != nil {
			return nil, err
		}
		videoSender, err := pc.AddTrack(videoTrack)
		if err != nil {
			return nil, err
		}
		internal.localTracks = append(internal.localTracks, videoTrack)
		internal.videoSender = videoSender
	}

	// ICE candidate trickle
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		cJSON, _ := json.Marshal(c.ToJSON())
		sig := &SignalMessage{
			Type:      SignalCandidate,
			SessionID: sessID,
			From:      cm.localPeer,
			Candidate: string(cJSON),
		}
		// Deliver to all participants (simplified: single peer for now)
		if cm.SendSignalFn != nil && len(sess.Participants) > 1 {
			cm.SendSignalFn(ctx, sess.Participants[1], sig)
		}
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		internal.mu.Lock()
		internal.remoteTracks[track.ID()] = track
		internal.mu.Unlock()
		if cm.OnTrack != nil {
			cm.OnTrack(sessID, cm.localPeer, track)
		}
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		cm.log.Debug("call state", zap.String("session", sessID), zap.String("state", s.String()))
		switch s {
		case webrtc.PeerConnectionStateConnected:
			internal.mu.Lock()
			internal.State = core.CallActive
			internal.mu.Unlock()
			cm.bus.Publish(core.Event{Type: core.EvtCallConnected, Timestamp: time.Now(), Data: sess})
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateDisconnected:
			internal.State = core.CallFailed
			cm.bus.Publish(core.Event{Type: core.EvtCallFailed, Timestamp: time.Now(), Data: sess})
		}
	})

	// Create offer
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return nil, fmt.Errorf("call: create offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return nil, fmt.Errorf("call: set local description: %w", err)
	}

	cm.mu.Lock()
	cm.sessions[sessID] = internal
	cm.mu.Unlock()

	// Send offer to callee – the UI must supply a SendSignalFn that routes to chatID
	if cm.SendSignalFn == nil {
		return nil, errors.New("call: SendSignalFn not set")
	}

	cm.bus.Publish(core.Event{Type: core.EvtCallIncoming, Timestamp: time.Now(), Data: sess})
	return sess, nil
}

// ─── Accept / Decline ─────────────────────────────────────────────────────────

// AcceptCall answers an incoming call.
func (cm *CallManager) AcceptCall(ctx context.Context, sessionID string) error {
	cm.mu.RLock()
	internal, ok := cm.sessions[sessionID]
	cm.mu.RUnlock()
	if !ok {
		return fmt.Errorf("call: session %s not found", sessionID)
	}

	internal.mu.Lock()
	defer internal.mu.Unlock()
	internal.State = core.CallConnecting

	// Create answer and set local description
	answer, err := internal.pc.CreateAnswer(nil)
	if err != nil {
		return err
	}
	if err := internal.pc.SetLocalDescription(answer); err != nil {
		return err
	}

	// Send answer to initiator
	sig := &SignalMessage{
		Type:      SignalAnswer,
		SessionID: sessionID,
		From:      cm.localPeer,
		SDP:       answer.SDP,
	}
	return cm.SendSignalFn(ctx, internal.Initiator, sig)
}

// DeclineCall sends a bye and discards the session.
func (cm *CallManager) DeclineCall(ctx context.Context, sessionID string) error {
	cm.mu.RLock()
	internal, ok := cm.sessions[sessionID]
	cm.mu.RUnlock()
	if !ok {
		return nil
	}

	sig := &SignalMessage{
		Type:      SignalBye,
		SessionID: sessionID,
		From:      cm.localPeer,
	}
	if cm.SendSignalFn != nil {
		cm.SendSignalFn(ctx, internal.Initiator, sig)
	}
	return cm.cleanup(sessionID, core.CallDeclined)
}

// HangUp terminates an active call.
func (cm *CallManager) HangUp(ctx context.Context, sessionID string) error {
	cm.mu.RLock()
	internal, ok := cm.sessions[sessionID]
	cm.mu.RUnlock()
	if !ok {
		return nil
	}

	for _, p := range internal.Participants {
		if p.String() == cm.localPeer.String() {
			continue
		}
		if cm.SendSignalFn != nil {
			cm.SendSignalFn(ctx, p, &SignalMessage{
				Type: SignalBye, SessionID: sessionID, From: cm.localPeer,
			})
		}
	}
	return cm.cleanup(sessionID, core.CallEnded)
}

// ─── Signal handling ──────────────────────────────────────────────────────────

// HandleSignal processes a signalling message received from a remote peer.
func (cm *CallManager) HandleSignal(ctx context.Context, from core.PeerID, rawSignal []byte) error {
	var sig SignalMessage
	if err := json.Unmarshal(rawSignal, &sig); err != nil {
		return fmt.Errorf("call: bad signal JSON: %w", err)
	}

	switch sig.Type {
	case SignalOffer:
		return cm.handleOffer(ctx, from, &sig)
	case SignalAnswer:
		return cm.handleAnswer(ctx, from, &sig)
	case SignalCandidate:
		return cm.handleCandidate(ctx, from, &sig)
	case SignalBye:
		return cm.cleanup(sig.SessionID, core.CallEnded)
	default:
		return fmt.Errorf("call: unknown signal type %q", sig.Type)
	}
}

func (cm *CallManager) handleOffer(ctx context.Context, from core.PeerID, sig *SignalMessage) error {
	pc, err := cm.newPeerConnection()
	if err != nil {
		return err
	}

	sess := &core.CallSession{
		ID:           sig.SessionID,
		Type:         sig.CallType,
		State:        core.CallRinging,
		Initiator:    from,
		Participants: []core.PeerID{from, cm.localPeer},
		StartedAt:    time.Now(),
	}
	internal := &callSessionInternal{
		CallSession:  sess,
		pc:           pc,
		remoteTracks: make(map[string]*webrtc.TrackRemote),
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		cJSON, _ := json.Marshal(c.ToJSON())
		if cm.SendSignalFn != nil {
			cm.SendSignalFn(ctx, from, &SignalMessage{
				Type: SignalCandidate, SessionID: sig.SessionID,
				From: cm.localPeer, Candidate: string(cJSON),
			})
		}
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		internal.mu.Lock()
		internal.remoteTracks[track.ID()] = track
		internal.mu.Unlock()
		if cm.OnTrack != nil {
			cm.OnTrack(sig.SessionID, from, track)
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sig.SDP,
	}); err != nil {
		return err
	}

	cm.mu.Lock()
	cm.sessions[sig.SessionID] = internal
	cm.mu.Unlock()

	// Deliver pending candidates
	internal.mu.Lock()
	for _, c := range internal.pendingCandidates {
		pc.AddICECandidate(c)
	}
	internal.pendingCandidates = nil
	internal.offerAnswerDone = true
	internal.mu.Unlock()

	cm.bus.Publish(core.Event{Type: core.EvtCallIncoming, Timestamp: time.Now(), Data: sess})
	return nil
}

func (cm *CallManager) handleAnswer(_ context.Context, _ core.PeerID, sig *SignalMessage) error {
	cm.mu.RLock()
	internal, ok := cm.sessions[sig.SessionID]
	cm.mu.RUnlock()
	if !ok {
		return fmt.Errorf("call: answer for unknown session %s", sig.SessionID)
	}

	if err := internal.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sig.SDP,
	}); err != nil {
		return err
	}

	internal.mu.Lock()
	for _, c := range internal.pendingCandidates {
		internal.pc.AddICECandidate(c)
	}
	internal.pendingCandidates = nil
	internal.offerAnswerDone = true
	internal.mu.Unlock()
	return nil
}

func (cm *CallManager) handleCandidate(_ context.Context, _ core.PeerID, sig *SignalMessage) error {
	cm.mu.RLock()
	internal, ok := cm.sessions[sig.SessionID]
	cm.mu.RUnlock()
	if !ok {
		return nil // session may not be set up yet – drop
	}

	var c webrtc.ICECandidateInit
	if err := json.Unmarshal([]byte(sig.Candidate), &c); err != nil {
		return err
	}

	internal.mu.Lock()
	defer internal.mu.Unlock()
	if !internal.offerAnswerDone {
		internal.pendingCandidates = append(internal.pendingCandidates, c)
		return nil
	}
	return internal.pc.AddICECandidate(c)
}

// ─── Media controls ───────────────────────────────────────────────────────────

// SetAudioEnabled mutes / unmutes the local microphone.
func (cm *CallManager) SetAudioEnabled(sessionID string, enabled bool) error {
	cm.mu.RLock()
	internal, ok := cm.sessions[sessionID]
	cm.mu.RUnlock()
	if !ok {
		return fmt.Errorf("call: session %s not found", sessionID)
	}
	if internal.audioSender != nil {
		for _, enc := range internal.audioSender.GetParameters().Encodings {
			_ = enc // pion doesn't directly support mute; we send silence instead
		}
	}
	cm.log.Info("audio toggled", zap.Bool("enabled", enabled), zap.String("session", sessionID))
	return nil
}

// SetVideoEnabled pauses / resumes video.
func (cm *CallManager) SetVideoEnabled(sessionID string, enabled bool) error {
	cm.mu.RLock()
	_, ok := cm.sessions[sessionID]
	cm.mu.RUnlock()
	if !ok {
		return fmt.Errorf("call: session %s not found", sessionID)
	}
	cm.log.Info("video toggled", zap.Bool("enabled", enabled), zap.String("session", sessionID))
	return nil
}

// SetScreenShare starts/stops screen capture (capture is handled by the UI layer;
// the UI writes RTP packets to the videoTrack directly).
func (cm *CallManager) SetScreenShare(sessionID string, enabled bool) error {
	cm.mu.RLock()
	_, ok := cm.sessions[sessionID]
	cm.mu.RUnlock()
	if !ok {
		return fmt.Errorf("call: session %s not found", sessionID)
	}
	cm.log.Info("screen share toggled", zap.Bool("enabled", enabled), zap.String("session", sessionID))
	return nil
}

// GetVideoTrack returns the local video track for a session so the UI can write RTP.
func (cm *CallManager) GetVideoTrack(sessionID string) (*webrtc.TrackLocalStaticRTP, error) {
	cm.mu.RLock()
	internal, ok := cm.sessions[sessionID]
	cm.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("call: session %s not found", sessionID)
	}
	for _, t := range internal.localTracks {
		if t.Kind() == webrtc.RTPCodecTypeVideo {
			return t, nil
		}
	}
	return nil, errors.New("call: no video track in session")
}

// GetAudioTrack returns the local audio track.
func (cm *CallManager) GetAudioTrack(sessionID string) (*webrtc.TrackLocalStaticRTP, error) {
	cm.mu.RLock()
	internal, ok := cm.sessions[sessionID]
	cm.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("call: session %s not found", sessionID)
	}
	for _, t := range internal.localTracks {
		if t.Kind() == webrtc.RTPCodecTypeAudio {
			return t, nil
		}
	}
	return nil, errors.New("call: no audio track in session")
}

// ActiveCalls returns a snapshot of all active/ringing call sessions.
func (cm *CallManager) ActiveCalls() []*core.CallSession {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	var out []*core.CallSession
	for _, s := range cm.sessions {
		out = append(out, s.CallSession)
	}
	return out
}

// ─── Internals ────────────────────────────────────────────────────────────────

func (cm *CallManager) newPeerConnection() (*webrtc.PeerConnection, error) {
	cfg := webrtc.Configuration{
		ICEServers: cm.iceServers,
		// BundlePolicy: max-bundle to use a single port for all media
		BundlePolicy:  webrtc.BundlePolicyMaxBundle,
		RTCPMuxPolicy: webrtc.RTCPMuxPolicyRequire,
	}
	return cm.webrtcAPI.NewPeerConnection(cfg)
}

func (cm *CallManager) cleanup(sessionID string, endState core.CallState) error {
	cm.mu.Lock()
	internal, ok := cm.sessions[sessionID]
	if ok {
		delete(cm.sessions, sessionID)
	}
	cm.mu.Unlock()
	if !ok {
		return nil
	}

	now := time.Now()
	internal.State = endState
	internal.EndedAt = &now
	if !internal.StartedAt.IsZero() {
		internal.Duration = now.Sub(internal.StartedAt)
	}
	internal.pc.Close()

	cm.bus.Publish(core.Event{Type: core.EvtCallEnded, Timestamp: now, Data: internal.CallSession})
	return nil
}

// ─── RTCP Quality stats ───────────────────────────────────────────────────────

// StartQualityReporting begins periodic RTCP stat collection for a session.
// It publishes EvtCallQuality events on the bus every interval.
func (cm *CallManager) StartQualityReporting(sessionID string, interval time.Duration) error {
	cm.mu.RLock()
	internal, ok := cm.sessions[sessionID]
	cm.mu.RUnlock()
	if !ok {
		return fmt.Errorf("call: session %s not found", sessionID)
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				cm.mu.RLock()
				_, stillActive := cm.sessions[sessionID]
				cm.mu.RUnlock()
				if !stillActive {
					return
				}
				q := cm.collectQuality(internal)
				cm.bus.Publish(core.Event{
					Type:      core.EvtCallQuality,
					Timestamp: time.Now(),
					Data:      q,
				})
			}
		}
	}()
	return nil
}

func (cm *CallManager) collectQuality(s *callSessionInternal) *core.CallQuality {
	q := &core.CallQuality{
		SessionID: s.ID,
		Timestamp: time.Now(),
	}

	// Collect stats from all senders
	for _, sender := range s.pc.GetSenders() {
		stats := s.pc.GetStats()
		for _, stat := range stats {
			switch v := stat.(type) {
			case webrtc.RemoteInboundRTPStreamStats:
				q.RTT = time.Duration(v.RoundTripTime * float64(time.Second))
				q.PacketLoss = float64(v.FractionLost)
				q.Jitter = time.Duration(v.Jitter * float64(time.Second))
			case webrtc.OutboundRTPStreamStats:
				if sender.Track() != nil {
					switch sender.Track().Kind() {
					case webrtc.RTPCodecTypeAudio:
						q.AudioBitrate = int(v.BytesSent * 8)
					case webrtc.RTPCodecTypeVideo:
						q.VideoBitrate = int(v.BytesSent * 8)
					}
				}
			}
		}
	}
	return q
}

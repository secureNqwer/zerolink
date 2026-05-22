// Package crypto implements the cryptographic layer for the messenger.
// Algorithms:
//   - Key agreement:   X25519 ECDH
//   - Signatures:      Ed25519
//   - Symmetric enc:   AES-256-GCM (authenticated)
//   - Fast enc:        ChaCha20-Poly1305
//   - Key derivation:  HKDF-SHA256
//   - MAC:             HMAC-SHA256
//   - Forward secrecy: Double-Ratchet session key rotation
//   - Group E2E:       Sender-Key protocol
//   - Verification:    Safety Numbers (Signal-compatible)
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"

	"github.com/yourorg/messenger-core/core"
)

// ─── Identity ────────────────────────────────────────────────────────────────

// Identity holds both the X25519 DH keypair and the Ed25519 signing keypair.
type Identity struct {
	SignPublic  ed25519.PublicKey  `json:"sign_pub"`
	SignPrivate ed25519.PrivateKey `json:"sign_priv"`
	DHPublic    [32]byte           `json:"dh_pub"`
	DHPrivate   [32]byte           `json:"dh_priv"`
	Fingerprint string             `json:"fingerprint"`
	CreatedAt   time.Time          `json:"created_at"`
}

// PeerID converts this identity into a core.PeerID.
func (id *Identity) PeerID(nodeID core.NodeID) core.PeerID {
	return core.PeerID{NodeID: nodeID, Fingerprint: id.Fingerprint}
}

// Serialize returns a JSON-encoded private representation. Store encrypted on disk.
func (id *Identity) Serialize() ([]byte, error) { return json.Marshal(id) }

// ─── Safety Numbers ───────────────────────────────────────────────────────────

// SafetyNumber is a human-verifiable fingerprint of a session.
// Displayed as 12 groups of 5 digits (Signal-compatible) or as QR payload.
type SafetyNumber struct {
	Combined  string // 60-digit string shown in UI
	QRPayload []byte // raw bytes for QR encoding
}

// ComputeSafetyNumber derives a Safety Number from both parties' identity keys.
// The result is symmetric: A↔B gives the same number as B↔A.
func ComputeSafetyNumber(
	localSignPub ed25519.PublicKey, localNodeID core.NodeID,
	remoteSignPub ed25519.PublicKey, remoteNodeID core.NodeID,
) (*SafetyNumber, error) {
	// Sort by node ID to ensure symmetry
	a, aKey := []byte(string(localNodeID)), []byte(localSignPub)
	b, bKey := []byte(string(remoteNodeID)), []byte(remoteSignPub)
	if string(localNodeID) > string(remoteNodeID) {
		a, b = b, a
		aKey, bKey = bKey, aKey
	}

	hashA := iteratedHash(aKey, a, 5200)
	hashB := iteratedHash(bKey, b, 5200)
	combined := append(hashA, hashB...)
	digits := fingerprintToDigits(combined)

	qr := make([]byte, 0, 2+len(aKey)+len(bKey))
	qr = append(qr, 0x02, 0x0D) // version tag
	qr = append(qr, aKey...)
	qr = append(qr, bKey...)

	return &SafetyNumber{Combined: digits, QRPayload: qr}, nil
}

func iteratedHash(key, id []byte, iterations int) []byte {
	h := sha512.New()
	h.Write(key)
	h.Write(id)
	result := h.Sum(nil)
	for i := 1; i < iterations; i++ {
		h.Reset()
		h.Write(result)
		h.Write(key)
		result = h.Sum(nil)
	}
	return result[:30]
}

func fingerprintToDigits(data []byte) string {
	out := make([]byte, 0, 60)
	for i := 0; i < 12; i++ {
		chunk := data[i*5 : i*5+5]
		padded := append(make([]byte, 3), chunk...)
		v := binary.BigEndian.Uint64(padded) % 100000
		out = append(out, fmt.Sprintf("%05d", v)...)
	}
	return string(out)
}

// ─── Session / Double-Ratchet ────────────────────────────────────────────────

// SessionKey represents a per-peer double-ratchet session.
type SessionKey struct {
	mu sync.Mutex

	RootKey   [32]byte
	SendChain [32]byte
	RecvChain [32]byte

	SendCounter uint64
	RecvCounter uint64

	LocalDHPub  [32]byte
	LocalDHPriv [32]byte
	RemoteDHPub [32]byte

	PeerID    core.PeerID
	CreatedAt time.Time
	UpdatedAt time.Time

	// key rotation state
	MessagesSinceRotation uint64
	LastRotatedAt         time.Time
	RotateAfterMessages   uint64
	RotateAfterDuration   time.Duration
}

// NeedsRotation returns true when a new DH ratchet step should be triggered.
func (s *SessionKey) NeedsRotation() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.RotateAfterMessages > 0 && s.MessagesSinceRotation >= s.RotateAfterMessages {
		return true
	}
	if s.RotateAfterDuration > 0 && time.Since(s.LastRotatedAt) >= s.RotateAfterDuration {
		return true
	}
	return false
}

// Advance derives a message key from the sending chain and increments the counter.
func (s *SessionKey) Advance() ([32]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mk, ck, err := kdfChain(s.SendChain)
	if err != nil {
		return [32]byte{}, err
	}
	s.SendChain = ck
	s.SendCounter++
	s.MessagesSinceRotation++
	s.UpdatedAt = time.Now()
	return mk, nil
}

// AdvanceRecv derives a message key from the receive chain.
func (s *SessionKey) AdvanceRecv() ([32]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mk, ck, err := kdfChain(s.RecvChain)
	if err != nil {
		return [32]byte{}, err
	}
	s.RecvChain = ck
	s.RecvCounter++
	return mk, nil
}

// DHRatchet performs a Diffie-Hellman ratchet step with a remote ephemeral key.
func (s *SessionKey) DHRatchet(remotePub [32]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	shared, err := curve25519.X25519(s.LocalDHPriv[:], remotePub[:])
	if err != nil {
		return err
	}
	rk, rc, err := kdfRootKey(s.RootKey[:], shared)
	if err != nil {
		return err
	}
	s.RootKey = rk
	s.RecvChain = rc
	s.RemoteDHPub = remotePub

	pub, priv, err := generateDHKeypair()
	if err != nil {
		return err
	}
	s.LocalDHPub = pub
	s.LocalDHPriv = priv

	shared2, err := curve25519.X25519(s.LocalDHPriv[:], remotePub[:])
	if err != nil {
		return err
	}
	rk2, sc, err := kdfRootKey(s.RootKey[:], shared2)
	if err != nil {
		return err
	}
	s.RootKey = rk2
	s.SendChain = sc
	s.UpdatedAt = time.Now()
	s.MessagesSinceRotation = 0
	s.LastRotatedAt = time.Now()
	return nil
}

// ─── Sender Key (group E2E) ───────────────────────────────────────────────────

// SenderKey is the per-member key used in the Sender Key group protocol.
// Each member has their own sending chain; forward secrecy is per-member.
type SenderKey struct {
	mu sync.Mutex

	ChainID   [16]byte
	Iteration uint32
	ChainKey  [32]byte
	SignKey    ed25519.PrivateKey

	OwnerPeerID core.PeerID
	ChatID      core.ChatID
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// NewSenderKey generates a fresh sender key for a group chat member.
func NewSenderKey(owner core.PeerID, chatID core.ChatID, signKey ed25519.PrivateKey) (*SenderKey, error) {
	var chainID [16]byte
	if _, err := io.ReadFull(rand.Reader, chainID[:]); err != nil {
		return nil, err
	}
	var chainKey [32]byte
	if _, err := io.ReadFull(rand.Reader, chainKey[:]); err != nil {
		return nil, err
	}
	return &SenderKey{
		ChainID:     chainID,
		Iteration:   0,
		ChainKey:    chainKey,
		SignKey:     signKey,
		OwnerPeerID: owner,
		ChatID:      chatID,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}, nil
}

// Advance returns the message key for the current iteration and advances the chain.
func (sk *SenderKey) Advance() ([32]byte, error) {
	sk.mu.Lock()
	defer sk.mu.Unlock()

	mk, err := deriveKey(sk.ChainKey[:], nil, []byte("sender-msg"), 32)
	if err != nil {
		return [32]byte{}, err
	}
	nck, err := deriveKey(sk.ChainKey[:], nil, []byte("sender-chain"), 32)
	if err != nil {
		return [32]byte{}, err
	}
	copy(sk.ChainKey[:], nck)
	sk.Iteration++
	sk.UpdatedAt = time.Now()

	var mkArr [32]byte
	copy(mkArr[:], mk)
	return mkArr, nil
}

// SenderKeyDistribution is the payload broadcast when a sender key is created or rotated.
// The ChainKey field is encrypted separately for each recipient using their X25519 key.
type SenderKeyDistribution struct {
	ChainID   [16]byte          `json:"chain_id"`
	Iteration uint32            `json:"iteration"`
	ChainKey  []byte            `json:"chain_key"` // encrypted for recipient
	SignPub   ed25519.PublicKey `json:"sign_pub"`
	ChatID    core.ChatID       `json:"chat_id"`
	OwnerID   core.PeerID       `json:"owner_id"`
	Signature []byte            `json:"sig"` // sign(chainID+chatID+iteration, SignKey)
	IssuedAt  time.Time         `json:"issued_at"`
}

// ─── Provider ────────────────────────────────────────────────────────────────

// Provider implements core.Crypto and owns all session/key state.
type Provider struct {
	sessions   sync.Map // PeerID.String() → *SessionKey
	senderKeys sync.Map // chatID+":"+peerID → *SenderKey
}

// NewProvider creates a ready-to-use crypto provider.
func NewProvider() *Provider { return &Provider{} }

// GenerateIdentity creates fresh Ed25519 + X25519 keypairs.
func (p *Provider) GenerateIdentity() (*Identity, error) {
	signPub, signPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	dhPub, dhPriv, err := generateDHKeypair()
	if err != nil {
		return nil, err
	}
	return &Identity{
		SignPublic:  signPub,
		SignPrivate: signPriv,
		DHPublic:    dhPub,
		DHPrivate:   dhPriv,
		Fingerprint: fingerprintKey(signPub),
		CreatedAt:   time.Now(),
	}, nil
}

// LoadIdentity restores an identity from JSON bytes.
func (p *Provider) LoadIdentity(data []byte) (*Identity, error) {
	var id Identity
	return &id, json.Unmarshal(data, &id)
}

// InitSession establishes a new double-ratchet session with a remote peer.
func (p *Provider) InitSession(local *Identity, remoteDHPub [32]byte, remotePeer core.PeerID) (*SessionKey, error) {
	shared, err := curve25519.X25519(local.DHPrivate[:], remoteDHPub[:])
	if err != nil {
		return nil, err
	}
	salt := sha256.Sum256(append(local.DHPublic[:], remoteDHPub[:]...))
	rootKey, err := deriveKey(shared, salt[:], []byte("ZT-Messenger-v1-Root"), 32)
	if err != nil {
		return nil, err
	}
	sc, err := deriveKey(rootKey, []byte("send"), []byte("chain"), 32)
	if err != nil {
		return nil, err
	}
	rc, err := deriveKey(rootKey, []byte("recv"), []byte("chain"), 32)
	if err != nil {
		return nil, err
	}
	ePub, ePriv, err := generateDHKeypair()
	if err != nil {
		return nil, err
	}
	var rk, scArr, rcArr [32]byte
	copy(rk[:], rootKey)
	copy(scArr[:], sc)
	copy(rcArr[:], rc)

	sess := &SessionKey{
		RootKey:             rk,
		SendChain:           scArr,
		RecvChain:           rcArr,
		LocalDHPub:          ePub,
		LocalDHPriv:         ePriv,
		RemoteDHPub:         remoteDHPub,
		PeerID:              remotePeer,
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
		LastRotatedAt:       time.Now(),
		RotateAfterMessages: 100,
		RotateAfterDuration: 7 * 24 * time.Hour,
	}
	p.sessions.Store(remotePeer.String(), sess)
	return sess, nil
}

// GetSession retrieves the session for a peer, nil if not found.
func (p *Provider) GetSession(peer core.PeerID) *SessionKey {
	v, ok := p.sessions.Load(peer.String())
	if !ok {
		return nil
	}
	return v.(*SessionKey)
}

// DeleteSession removes a session (call when a peer's key changes).
func (p *Provider) DeleteSession(peer core.PeerID) {
	p.sessions.Delete(peer.String())
}

// StoreSenderKey saves a sender key for a group chat member.
func (p *Provider) StoreSenderKey(sk *SenderKey) {
	p.senderKeys.Store(senderKeyID(sk.ChatID, sk.OwnerPeerID), sk)
}

// GetSenderKey retrieves a sender key.
func (p *Provider) GetSenderKey(chatID core.ChatID, peer core.PeerID) *SenderKey {
	v, ok := p.senderKeys.Load(senderKeyID(chatID, peer))
	if !ok {
		return nil
	}
	return v.(*SenderKey)
}

// DeleteSenderKey removes a sender key (e.g. when a member leaves the group).
func (p *Provider) DeleteSenderKey(chatID core.ChatID, peer core.PeerID) {
	p.senderKeys.Delete(senderKeyID(chatID, peer))
}

// DeriveSessionKey performs a one-shot X25519 ECDH (bootstrap, no ratchet).
func (p *Provider) DeriveSessionKey(local *Identity, remotePub []byte) ([]byte, error) {
	if len(remotePub) != 32 {
		return nil, errors.New("crypto: remote DH public key must be 32 bytes")
	}
	shared, err := curve25519.X25519(local.DHPrivate[:], remotePub)
	if err != nil {
		return nil, err
	}
	return deriveKey(shared, nil, []byte("ZT-Messenger-v1-Session"), 32)
}

// ─── Symmetric encryption ─────────────────────────────────────────────────────

// EncryptAES encrypts with AES-256-GCM. Returns [nonce][ciphertext+tag].
func (p *Provider) EncryptAES(key, plaintext, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

// DecryptAES decrypts AES-256-GCM.
func (p *Provider) DecryptAES(key, ciphertext, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(ciphertext) < ns {
		return nil, errors.New("crypto: ciphertext too short")
	}
	return gcm.Open(nil, ciphertext[:ns], ciphertext[ns:], aad)
}

// EncryptChaCha encrypts with ChaCha20-Poly1305 (faster without AES-NI).
func (p *Provider) EncryptChaCha(key, plaintext, aad []byte) ([]byte, error) {
	c, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, c.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return c.Seal(nonce, nonce, plaintext, aad), nil
}

// DecryptChaCha decrypts ChaCha20-Poly1305.
func (p *Provider) DecryptChaCha(key, ciphertext, aad []byte) ([]byte, error) {
	c, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	ns := c.NonceSize()
	if len(ciphertext) < ns {
		return nil, errors.New("crypto: ciphertext too short")
	}
	return c.Open(nil, ciphertext[:ns], ciphertext[ns:], aad)
}

// Encrypt satisfies core.Crypto (AES-256-GCM).
func (p *Provider) Encrypt(key, plaintext, aad []byte) ([]byte, error) {
	return p.EncryptAES(key, plaintext, aad)
}

// Decrypt satisfies core.Crypto (AES-256-GCM).
func (p *Provider) Decrypt(key, ciphertext, aad []byte) ([]byte, error) {
	return p.DecryptAES(key, ciphertext, aad)
}

// Sign signs msg with Ed25519.
func (p *Provider) Sign(identity *Identity, msg []byte) ([]byte, error) {
	return ed25519.Sign(identity.SignPrivate, msg), nil
}

// Verify checks an Ed25519 signature.
func (p *Provider) Verify(publicKey, msg, sig []byte) bool {
	if len(publicKey) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(publicKey, msg, sig)
}

// HMAC computes HMAC-SHA256.
func (p *Provider) HMAC(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// DeriveKey derives a sub-key using HKDF-SHA256.
func (p *Provider) DeriveKey(secret, salt, info []byte, length int) ([]byte, error) {
	return deriveKey(secret, salt, info, length)
}

// HashData returns hex-encoded SHA-256 of data.
func HashData(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// ─── Packet-level helpers ─────────────────────────────────────────────────────

// EncryptPacketBody encrypts a packet body using the double-ratchet session.
func (p *Provider) EncryptPacketBody(sessKey *SessionKey, header, body []byte) ([]byte, error) {
	msgKey, err := sessKey.Advance()
	if err != nil {
		return nil, err
	}
	var ctrBytes [8]byte
	binary.BigEndian.PutUint64(ctrBytes[:], sessKey.SendCounter)
	encKey, err := deriveKey(msgKey[:], ctrBytes[:], []byte("enc"), 32)
	if err != nil {
		return nil, err
	}
	return p.Encrypt(encKey, body, header)
}

// DecryptPacketBody decrypts a packet body using the double-ratchet session.
func (p *Provider) DecryptPacketBody(sessKey *SessionKey, header, body []byte) ([]byte, error) {
	msgKey, err := sessKey.AdvanceRecv()
	if err != nil {
		return nil, err
	}
	var ctrBytes [8]byte
	binary.BigEndian.PutUint64(ctrBytes[:], sessKey.RecvCounter)
	encKey, err := deriveKey(msgKey[:], ctrBytes[:], []byte("enc"), 32)
	if err != nil {
		return nil, err
	}
	return p.Decrypt(encKey, body, header)
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

func senderKeyID(chatID core.ChatID, peer core.PeerID) string {
	return string(chatID) + ":" + peer.String()
}

func generateDHKeypair() ([32]byte, [32]byte, error) {
	var priv [32]byte
	if _, err := io.ReadFull(rand.Reader, priv[:]); err != nil {
		return [32]byte{}, [32]byte{}, err
	}
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64
	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return [32]byte{}, [32]byte{}, err
	}
	var pubArr [32]byte
	copy(pubArr[:], pub)
	return pubArr, priv, nil
}

func deriveKey(secret, salt, info []byte, length int) ([]byte, error) {
	if salt == nil {
		salt = make([]byte, sha512.Size)
	}
	r := hkdf.New(sha256.New, secret, salt, info)
	key := make([]byte, length)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, err
	}
	return key, nil
}

func kdfChain(ck [32]byte) ([32]byte, [32]byte, error) {
	mk, err := deriveKey(ck[:], []byte{0x01}, []byte("msg"), 32)
	if err != nil {
		return [32]byte{}, [32]byte{}, err
	}
	nc, err := deriveKey(ck[:], []byte{0x02}, []byte("chain"), 32)
	if err != nil {
		return [32]byte{}, [32]byte{}, err
	}
	var mkArr, ncArr [32]byte
	copy(mkArr[:], mk)
	copy(ncArr[:], nc)
	return mkArr, ncArr, nil
}

func kdfRootKey(rk, dhOut []byte) ([32]byte, [32]byte, error) {
	newRK, err := deriveKey(dhOut, rk, []byte("root"), 32)
	if err != nil {
		return [32]byte{}, [32]byte{}, err
	}
	ck, err := deriveKey(dhOut, rk, []byte("chain"), 32)
	if err != nil {
		return [32]byte{}, [32]byte{}, err
	}
	var rkArr, ckArr [32]byte
	copy(rkArr[:], newRK)
	copy(ckArr[:], ck)
	return rkArr, ckArr, nil
}

func fingerprintKey(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:16])
}

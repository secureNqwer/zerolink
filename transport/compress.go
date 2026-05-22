// Package transport/compress provides a multi-algorithm compressor.
// Algorithm selection:
//   - Zstandard (zstd)  – best for messages, files (high ratio + fast)
//   - LZ4                – real-time media streams (ultra fast, lower ratio)
//   - None               – already-compressed data (JPEG, Opus, H.264)
package transport

import (
	"bytes"
	"errors"
	"io"

	"github.com/klauspost/compress/zstd"
	"github.com/klauspost/compress/snappy"
	"github.com/pierrec/lz4/v4"
)

// CompressionAlgo selects the codec.
type CompressionAlgo uint8

const (
	AlgoNone   CompressionAlgo = 0
	AlgoZstd   CompressionAlgo = 1 // default for messages / files
	AlgoLZ4    CompressionAlgo = 2 // real-time streams
	AlgoSnappy CompressionAlgo = 3 // fast, moderate ratio
)

// alreadyCompressedMagics lists magic bytes of formats that should NOT be re-compressed.
var alreadyCompressedMagics = [][]byte{
	{0xFF, 0xD8, 0xFF},       // JPEG
	{0x89, 0x50, 0x4E, 0x47}, // PNG
	{0x47, 0x49, 0x46},       // GIF
	{0x52, 0x49, 0x46, 0x46}, // RIFF (WebP)
	{0x00, 0x00, 0x00},       // MP4/MOV (partial)
	{0x1A, 0x45, 0xDF, 0xA3}, // WebM / MKV
	{0x4F, 0x67, 0x67, 0x53}, // OGG (Opus)
	{0xFF, 0xFB},             // MP3
	{0x66, 0x4C, 0x61, 0x43}, // FLAC
	{0x50, 0x4B},             // ZIP/docx/xlsx
	{0x1F, 0x8B},             // gzip
	{0x28, 0xB5, 0x2F, 0xFD}, // zstd
}

// isAlreadyCompressed returns true when data begins with a known compressed magic.
func isAlreadyCompressed(data []byte) bool {
	for _, magic := range alreadyCompressedMagics {
		if len(data) >= len(magic) && bytes.HasPrefix(data, magic) {
			return true
		}
	}
	return false
}

// MultiCompressor implements core.Compressor with automatic algo selection.
type MultiCompressor struct {
	zstdEnc *zstd.Encoder
	zstdDec *zstd.Decoder
}

// NewMultiCompressor creates a MultiCompressor with sane defaults.
func NewMultiCompressor() (*MultiCompressor, error) {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedBestCompression),
		zstd.WithEncoderConcurrency(4),
	)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	return &MultiCompressor{zstdEnc: enc, zstdDec: dec}, nil
}

// Compress compresses data, choosing the right algorithm automatically.
// Returns a 1-byte algo prefix followed by the compressed data.
// If the data is already compressed or compression would expand it,
// returns AlgoNone prefix + original data.
func (c *MultiCompressor) Compress(data []byte) ([]byte, error) {
	if isAlreadyCompressed(data) {
		return prependAlgo(AlgoNone, data), nil
	}
	// Try zstd
	compressed := c.zstdEnc.EncodeAll(data, nil)
	if len(compressed) >= len(data) {
		return prependAlgo(AlgoNone, data), nil
	}
	return prependAlgo(AlgoZstd, compressed), nil
}

// CompressFast uses LZ4 (for real-time paths, e.g., chunked media upload).
func (c *MultiCompressor) CompressFast(data []byte) ([]byte, error) {
	if isAlreadyCompressed(data) {
		return prependAlgo(AlgoNone, data), nil
	}
	var buf bytes.Buffer
	w := lz4.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if buf.Len() >= len(data) {
		return prependAlgo(AlgoNone, data), nil
	}
	return prependAlgo(AlgoLZ4, buf.Bytes()), nil
}

// Decompress decompresses data produced by Compress / CompressFast.
func (c *MultiCompressor) Decompress(data []byte) ([]byte, error) {
	if len(data) < 1 {
		return nil, errors.New("compress: empty data")
	}
	algo, payload := CompressionAlgo(data[0]), data[1:]
	switch algo {
	case AlgoNone:
		return payload, nil
	case AlgoZstd:
		return c.zstdDec.DecodeAll(payload, nil)
	case AlgoLZ4:
		var buf bytes.Buffer
		r := lz4.NewReader(bytes.NewReader(payload))
		if _, err := io.Copy(&buf, r); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case AlgoSnappy:
		return snappy.Decode(nil, payload)
	default:
		return nil, errors.New("compress: unknown algorithm")
	}
}

// CompressStream returns a streaming zstd write-closer.
// The caller must Close() the writer to flush.
func (c *MultiCompressor) CompressStream(dst io.Writer) (io.WriteCloser, error) {
	// Prefix the stream with the algo byte
	dst.Write([]byte{byte(AlgoZstd)})
	return zstd.NewWriter(dst)
}

// DecompressStream returns a decompressing reader.
// It reads and consumes the algo prefix byte first.
func (c *MultiCompressor) DecompressStream(src io.Reader) (io.ReadCloser, error) {
	var algoByte [1]byte
	if _, err := io.ReadFull(src, algoByte[:]); err != nil {
		return nil, err
	}
	switch CompressionAlgo(algoByte[0]) {
	case AlgoNone:
		return io.NopCloser(src), nil
	case AlgoZstd:
		r, err := zstd.NewReader(src)
		if err != nil {
			return nil, err
		}
		return r.IOReadCloser(), nil
	case AlgoLZ4:
		return io.NopCloser(lz4.NewReader(src)), nil
	default:
		return nil, errors.New("compress: unknown algorithm in stream")
	}
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func prependAlgo(algo CompressionAlgo, data []byte) []byte {
	out := make([]byte, 1+len(data))
	out[0] = byte(algo)
	copy(out[1:], data)
	return out
}

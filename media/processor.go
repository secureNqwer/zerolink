// Package media handles encoding, decoding, resizing, thumbnailing, and
// metadata extraction for all supported media types.
//
// Image: Go stdlib (JPEG, PNG, GIF, WebP via golang.org/x/image)
// Audio: passthrough + metadata extraction; Opus encoding via libopus CGO
// Video: thumbnail extraction via ffmpegthumbnailer (optional, subprocess)
// All heavy transcoding can be delegated to ffmpeg if present; the core
// remains functional without it (it simply skips transcoding).
package media

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	_ "image/gif"
	"io"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nfnt/resize"
	"go.uber.org/zap"

	"github.com/secureNqwer/zerolink/core"
)

// ImageOptions controls image processing.
type ImageOptions struct {
	MaxWidth    int
	MaxHeight   int
	JpegQuality int  // 1–100
	ThumbWidth  int
	ThumbHeight int
	StripExif   bool
}

// AudioOptions controls audio processing.
type AudioOptions struct {
	BitrateKbps int
	SampleRate  int // 48000 for Opus
	Channels    int // 1 or 2
}

// VideoOptions controls video processing.
type VideoOptions struct {
	MaxHeight    int
	BitrateKbps  int
	ThumbWidth   int
	ThumbHeight  int
	ThumbnailAt  time.Duration // timestamp for thumbnail frame
}

// ProcessedMedia is returned by every processor method.
type ProcessedMedia struct {
	Hash      string        // SHA-256 of processed data
	Data      []byte        // processed media bytes
	Thumbnail []byte        // JPEG thumbnail (nil if not applicable)
	MimeType  string
	FileName  string
	Size      int64
	Width     int
	Height    int
	Duration  time.Duration
}

// ─── Processor ────────────────────────────────────────────────────────────────

// Processor implements core.MediaProcessor.
type Processor struct {
	log      *zap.Logger
	cfg      *core.Config
	tmpDir   string
	ffmpegOK bool // whether ffmpeg is available on PATH
}

// NewProcessor creates a media processor.
func NewProcessor(cfg *core.Config, log *zap.Logger) *Processor {
	p := &Processor{
		log:    log,
		cfg:    cfg,
		tmpDir: os.TempDir(),
	}
	if _, err := exec.LookPath("ffmpeg"); err == nil {
		p.ffmpegOK = true
		log.Info("ffmpeg found – full transcoding enabled")
	} else {
		log.Warn("ffmpeg not found – video transcoding disabled; thumbnails only")
	}
	return p
}

// ─── Image ────────────────────────────────────────────────────────────────────

// ProcessImage decodes, resizes if needed, re-encodes as JPEG, and generates a thumbnail.
func (p *Processor) ProcessImage(data []byte, opts ImageOptions) (*ProcessedMedia, error) {
	if opts.MaxWidth == 0 {
		opts.MaxWidth = p.cfg.MaxImageWidthPx
	}
	if opts.MaxHeight == 0 {
		opts.MaxHeight = p.cfg.MaxImageWidthPx // keep aspect
	}
	if opts.JpegQuality == 0 {
		opts.JpegQuality = 85
	}
	if opts.ThumbWidth == 0 {
		opts.ThumbWidth = 320
	}
	if opts.ThumbHeight == 0 {
		opts.ThumbHeight = 240
	}

	src, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("media: image decode: %w", err)
	}
	_ = format

	// Resize if larger than limits
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w > opts.MaxWidth || h > opts.MaxHeight {
		src = resize.Thumbnail(uint(opts.MaxWidth), uint(opts.MaxHeight), src, resize.Lanczos3)
		bounds = src.Bounds()
		w, h = bounds.Dx(), bounds.Dy()
	}

	// Re-encode as JPEG
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: opts.JpegQuality}); err != nil {
		return nil, fmt.Errorf("media: jpeg encode: %w", err)
	}

	// Thumbnail
	thumb := resize.Thumbnail(uint(opts.ThumbWidth), uint(opts.ThumbHeight), src, resize.Bilinear)
	var thumbBuf bytes.Buffer
	jpeg.Encode(&thumbBuf, thumb, &jpeg.Options{Quality: 60})

	processed := buf.Bytes()
	hash := hashBytes(processed)

	return &ProcessedMedia{
		Hash:      hash,
		Data:      processed,
		Thumbnail: thumbBuf.Bytes(),
		MimeType:  "image/jpeg",
		Size:      int64(len(processed)),
		Width:     w,
		Height:    h,
	}, nil
}

// ProcessPNG processes a PNG image without re-encoding to JPEG.
func (p *Processor) ProcessPNG(data []byte, opts ImageOptions) (*ProcessedMedia, error) {
	if opts.MaxWidth == 0 {
		opts.MaxWidth = p.cfg.MaxImageWidthPx
	}

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("media: png decode: %w", err)
	}

	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w > opts.MaxWidth || h > opts.MaxHeight && opts.MaxHeight > 0 {
		src = resize.Thumbnail(uint(opts.MaxWidth), uint(opts.MaxHeight), src, resize.Lanczos3)
		bounds = src.Bounds()
		w, h = bounds.Dx(), bounds.Dy()
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		return nil, err
	}

	// JPEG thumbnail
	thumb := resize.Thumbnail(320, 240, src, resize.Bilinear)
	var thumbBuf bytes.Buffer
	jpeg.Encode(&thumbBuf, thumb, &jpeg.Options{Quality: 60})

	processed := buf.Bytes()
	return &ProcessedMedia{
		Hash:      hashBytes(processed),
		Data:      processed,
		Thumbnail: thumbBuf.Bytes(),
		MimeType:  "image/png",
		Size:      int64(len(processed)),
		Width:     w,
		Height:    h,
	}, nil
}

// ─── Auto-dispatch image processor ───────────────────────────────────────────

// ProcessImageAuto detects format and calls the right handler.
func (p *Processor) ProcessImageAuto(data []byte, opts ImageOptions) (*ProcessedMedia, error) {
	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("media: detect format: %w", err)
	}
	if format == "png" {
		return p.ProcessPNG(data, opts)
	}
	return p.ProcessImage(data, opts)
}

// ─── Audio ────────────────────────────────────────────────────────────────────

// ProcessAudio validates and packages audio.
// If ffmpeg is available it transcodes to Opus/OGG; otherwise it passes through.
func (p *Processor) ProcessAudio(data []byte, opts AudioOptions) (*ProcessedMedia, error) {
	if opts.BitrateKbps == 0 {
		opts.BitrateKbps = p.cfg.AudioBitrateKbps
	}
	if opts.SampleRate == 0 {
		opts.SampleRate = 48000
	}
	if opts.Channels == 0 {
		opts.Channels = 1
	}

	mimeType := detectMime(data)
	var out []byte
	var duration time.Duration

	if p.ffmpegOK && !strings.HasPrefix(mimeType, "audio/ogg") {
		var err error
		out, duration, err = p.transcodeAudioFFmpeg(data, opts)
		if err != nil {
			p.log.Warn("audio transcode failed, passing through", zap.Error(err))
			out = data
		}
		mimeType = "audio/ogg; codecs=opus"
	} else {
		out = data
		duration = estimateDuration(data, opts.BitrateKbps)
	}

	return &ProcessedMedia{
		Hash:     hashBytes(out),
		Data:     out,
		MimeType: mimeType,
		Size:     int64(len(out)),
		Duration: duration,
	}, nil
}

// transcodeAudioFFmpeg uses ffmpeg to encode to Opus.
func (p *Processor) transcodeAudioFFmpeg(data []byte, opts AudioOptions) ([]byte, time.Duration, error) {
	in, err := os.CreateTemp(p.tmpDir, "msg-audio-in-*")
	if err != nil {
		return nil, 0, err
	}
	defer os.Remove(in.Name())
	in.Write(data)
	in.Close()

	out := in.Name() + ".ogg"
	defer os.Remove(out)

	cmd := exec.Command("ffmpeg", "-y", "-i", in.Name(),
		"-c:a", "libopus",
		"-b:a", fmt.Sprintf("%dk", opts.BitrateKbps),
		"-ar", fmt.Sprintf("%d", opts.SampleRate),
		"-ac", fmt.Sprintf("%d", opts.Channels),
		"-vbr", "on",
		out,
	)
	if err := cmd.Run(); err != nil {
		return nil, 0, fmt.Errorf("ffmpeg audio: %w", err)
	}

	result, err := os.ReadFile(out)
	if err != nil {
		return nil, 0, err
	}

	dur := probeAudioDuration(out)
	return result, dur, nil
}

// ─── Video ────────────────────────────────────────────────────────────────────

// ProcessVideo processes a video file:
//   - Generates a thumbnail at opts.ThumbnailAt (default: 1 second)
//   - Optionally transcodes to H.264/AAC MP4 via ffmpeg
func (p *Processor) ProcessVideo(data []byte, opts VideoOptions) (*ProcessedMedia, error) {
	if opts.MaxHeight == 0 {
		opts.MaxHeight = p.cfg.MaxVideoHeightPx
	}
	if opts.BitrateKbps == 0 {
		opts.BitrateKbps = p.cfg.VideoBitrateKbps
	}
	if opts.ThumbWidth == 0 {
		opts.ThumbWidth = 320
	}
	if opts.ThumbHeight == 0 {
		opts.ThumbHeight = 240
	}
	if opts.ThumbnailAt == 0 {
		opts.ThumbnailAt = time.Second
	}

	in, err := os.CreateTemp(p.tmpDir, "msg-video-in-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(in.Name())
	in.Write(data)
	in.Close()

	var thumb []byte
	var duration time.Duration
	var out []byte
	mimeType := detectMime(data)

	if p.ffmpegOK {
		// Generate thumbnail
		thumb, err = p.extractVideoThumbnail(in.Name(), opts)
		if err != nil {
			p.log.Warn("video thumbnail failed", zap.Error(err))
		}

		// Probe duration
		duration = probeVideoDuration(in.Name())

		// Transcode if not already H.264
		if mimeType != "video/mp4" {
			out, err = p.transcodeVideoFFmpeg(in.Name(), opts)
			if err != nil {
				p.log.Warn("video transcode failed, using original", zap.Error(err))
				out = data
			} else {
				mimeType = "video/mp4"
			}
		} else {
			out = data
		}
	} else {
		// No ffmpeg – pass through, no thumbnail
		out = data
		p.log.Warn("ffmpeg unavailable; video passed through unprocessed")
	}

	if out == nil {
		out = data
	}

	return &ProcessedMedia{
		Hash:      hashBytes(out),
		Data:      out,
		Thumbnail: thumb,
		MimeType:  mimeType,
		Size:      int64(len(out)),
		Duration:  duration,
	}, nil
}

func (p *Processor) extractVideoThumbnail(path string, opts VideoOptions) ([]byte, error) {
	thumbPath := path + "-thumb.jpg"
	defer os.Remove(thumbPath)

	cmd := exec.Command("ffmpeg", "-y",
		"-ss", fmt.Sprintf("%.3f", opts.ThumbnailAt.Seconds()),
		"-i", path,
		"-vframes", "1",
		"-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease", opts.ThumbWidth, opts.ThumbHeight),
		"-q:v", "5",
		thumbPath,
	)
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return os.ReadFile(thumbPath)
}

func (p *Processor) transcodeVideoFFmpeg(inputPath string, opts VideoOptions) ([]byte, error) {
	out := inputPath + ".mp4"
	defer os.Remove(out)

	cmd := exec.Command("ffmpeg", "-y",
		"-i", inputPath,
		"-c:v", "libx264", "-preset", "fast", "-crf", "23",
		"-vf", fmt.Sprintf("scale=-2:%d", opts.MaxHeight),
		"-b:v", fmt.Sprintf("%dk", opts.BitrateKbps),
		"-c:a", "aac", "-b:a", "128k",
		"-movflags", "+faststart",
		out,
	)
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return os.ReadFile(out)
}

// ─── Generic file ────────────────────────────────────────────────────────────

// ProcessFile validates and packages a generic file upload.
func (p *Processor) ProcessFile(name string, data []byte) (*ProcessedMedia, error) {
	if len(data) == 0 {
		return nil, errors.New("media: empty file")
	}
	ext := strings.ToLower(filepath.Ext(name))
	mimeType := mime.TypeByExtension(ext)
	if mimeType == "" {
		mimeType = detectMime(data)
	}

	return &ProcessedMedia{
		Hash:     hashBytes(data),
		Data:     data,
		MimeType: mimeType,
		FileName: filepath.Base(name),
		Size:     int64(len(data)),
	}, nil
}

// ─── Sticker ─────────────────────────────────────────────────────────────────

// ProcessSticker accepts a small PNG/WebP and ensures it fits sticker limits.
func (p *Processor) ProcessSticker(data []byte) (*ProcessedMedia, error) {
	return p.ProcessImageAuto(data, ImageOptions{
		MaxWidth:    512,
		MaxHeight:   512,
		ThumbWidth:  96,
		ThumbHeight: 96,
	})
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func hashBytes(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h)
}

// detectMime sniffs the first 16 bytes to guess MIME type.
func detectMime(data []byte) string {
	if len(data) < 4 {
		return "application/octet-stream"
	}
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte{0x89, 0x50, 0x4E, 0x47}):
		return "image/png"
	case bytes.HasPrefix(data, []byte{0x47, 0x49, 0x46}):
		return "image/gif"
	case bytes.HasPrefix(data, []byte{0x52, 0x49, 0x46, 0x46}) && len(data) >= 12 &&
		bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp"
	case bytes.HasPrefix(data, []byte{0x4F, 0x67, 0x67, 0x53}):
		return "audio/ogg"
	case bytes.HasPrefix(data, []byte{0xFF, 0xFB}) || bytes.HasPrefix(data, []byte{0xFF, 0xF3}):
		return "audio/mpeg"
	case bytes.HasPrefix(data, []byte{0x66, 0x4C, 0x61, 0x43}):
		return "audio/flac"
	case bytes.HasPrefix(data, []byte{0x1A, 0x45, 0xDF, 0xA3}):
		return "video/webm"
	case bytes.HasPrefix(data, []byte{0x50, 0x4B, 0x03, 0x04}):
		return "application/zip"
	case bytes.HasPrefix(data, []byte{0x25, 0x50, 0x44, 0x46}):
		return "application/pdf"
	default:
		return "application/octet-stream"
	}
}

func estimateDuration(data []byte, bitrateKbps int) time.Duration {
	if bitrateKbps <= 0 {
		return 0
	}
	bytes_ := int64(len(data))
	secs := float64(bytes_*8) / float64(bitrateKbps*1000)
	return time.Duration(secs * float64(time.Second))
}

func probeAudioDuration(path string) time.Duration {
	return probeMediaDuration(path)
}

func probeVideoDuration(path string) time.Duration {
	return probeMediaDuration(path)
}

func probeMediaDuration(path string) time.Duration {
	out, err := exec.Command("ffprobe",
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		path,
	).Output()
	if err != nil {
		return 0
	}
	// Quick-and-dirty parse – avoids importing a full JSON library here.
	start := bytes.Index(out, []byte(`"duration": "`))
	if start < 0 {
		return 0
	}
	start += 14
	end := bytes.IndexByte(out[start:], '"')
	if end < 0 {
		return 0
	}
	var secs float64
	fmt.Sscanf(string(out[start:start+end]), "%f", &secs)
	return time.Duration(secs * float64(time.Second))
}

// Ensure io is used (for potential stream-based implementations)
var _ io.Reader = nil

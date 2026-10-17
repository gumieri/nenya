package tiktoken

import (
	"encoding/base64"
	"strings"
)

// Image token accounting (NENYA-157). A vision request's images were counted
// as the literal "[image]" (about two tokens), which under-counted a picture by
// three orders of magnitude and let a vision payload slip past every budget
// derived from the estimate. The cost below is Anthropic's published
// approximation — tokens = (width x height) / 750 — derived from the image's
// own header inside the inlined base64 payload, which needs no decode of the
// pixels and no network access.
const (
	// pixelsPerToken is the published area-to-token ratio (750 pixels per
	// token).
	pixelsPerToken = 750
	// MinImageTokens is the floor for any image block: providers bill at
	// least a small fixed cost even for a tiny picture.
	MinImageTokens = 85
	// MaxImageTokens is the ceiling: providers downscale an oversized image to
	// their maximum resolution before pricing it, so the published maximum
	// (1590 tokens, Anthropic's documented per-image cap) is what the model
	// bills regardless of the raw pixel count.
	MaxImageTokens = 1590
	// DefaultImageTokens is the cost of an image whose dimensions cannot be
	// determined: a remote URL (the pixels are not in the request) or an
	// unrecognized container. It approximates a 900x920 image.
	DefaultImageTokens = 1105
	// imageHeaderProbeBytes is how much of the encoded payload is decoded to
	// read the dimensions. PNG, GIF, and WebP declare them in the first 40
	// bytes, but a JPEG's frame header sits after its metadata segments, and a
	// phone camera's EXIF block (with an embedded thumbnail) is routinely tens
	// of kilobytes, so the probe is generous: 64 KiB of base64 is 48 KiB of
	// image header, still O(1) with respect to the payload (a 4 MiB image costs
	// the same work as a small one) and far cheaper than decoding any pixels.
	// Residual: a JPEG whose metadata exceeds that still falls back to the
	// default, which under-prices a large photo.
	imageHeaderProbeBytes = 65536
)

// ImageTokens estimates the token cost of one image block from its media type
// and inlined base64 payload (either bare base64 or a `data:` URL). An empty or
// remote payload, an unknown container, or a header whose dimensions cannot be
// read yields DefaultImageTokens, never zero: an image the estimator cannot
// measure must still be budgeted. The media type argument wins over the URL's
// declaration, but the container signature decides the parser — a mislabeled
// payload is parsed by what it actually is.
func ImageTokens(mediaType, payload string) int {
	if payload == "" {
		return DefaultImageTokens
	}
	encoded, declaredType := splitDataURL(payload)
	if encoded == "" {
		// A data URL without a base64 payload carries nothing measurable.
		return DefaultImageTokens
	}
	if mediaType == "" {
		mediaType = declaredType
	}
	width, height := imageDimensions(mediaType, encoded)
	if width <= 0 || height <= 0 {
		return DefaultImageTokens
	}
	return AreaTokens(width, height)
}

// AreaTokens converts a pixel area to a token cost: the published ratio,
// clamped to [MinImageTokens, MaxImageTokens] for a real area, and
// DefaultImageTokens for a non-positive one (an unmeasurable image still costs
// something).
func AreaTokens(width, height int) int {
	if width <= 0 || height <= 0 {
		return DefaultImageTokens
	}
	area := int64(width) * int64(height)
	tokens := int((area + pixelsPerToken - 1) / pixelsPerToken)
	if tokens < MinImageTokens {
		return MinImageTokens
	}
	if tokens > MaxImageTokens {
		return MaxImageTokens
	}
	return tokens
}

// splitDataURL splits a `data:<mime>;base64,<payload>` URL into its encoded
// payload and declared media type. A bare base64 string is returned as-is with
// an empty media type; a non-data URL (http/https) is returned as an opaque
// payload that then fails to decode (and so costs the default), and a data URL
// without a base64 payload returns an empty payload.
func splitDataURL(value string) (encoded, mediaType string) {
	// RFC 2397 schemes are case-insensitive.
	if len(value) < 5 || !strings.EqualFold(value[:5], "data:") {
		return value, ""
	}
	comma := strings.IndexByte(value, ',')
	if comma < 0 {
		return "", ""
	}
	meta := value[len("data:"):comma]
	if !strings.Contains(meta, "base64") {
		return "", ""
	}
	mediaType = meta
	if semi := strings.IndexByte(meta, ';'); semi >= 0 {
		mediaType = meta[:semi]
	}
	return value[comma+1:], mediaType
}

// imageDimensions reads the pixel dimensions out of a base64-encoded container
// header. Only the leading header is decoded, so the cost is independent of
// the image's size.
func imageDimensions(mediaType, encoded string) (int, int) {
	header := decodeHeader(encoded)
	if len(header) < 10 {
		return 0, 0
	}
	// The signature decides: a media type only steers an unrecognized
	// container to the right reader.
	switch {
	case hasPNGSignature(header):
		return pngDimensions(header)
	case hasJPEGSignature(header):
		return jpegDimensions(header)
	case hasGIFSignature(header):
		return gifDimensions(header)
	case hasWebPSignature(header):
		return webpDimensions(header)
	}
	switch mediaType {
	case "image/png":
		return pngDimensions(header)
	case "image/jpeg", "image/jpg":
		return jpegDimensions(header)
	case "image/gif":
		return gifDimensions(header)
	case "image/webp":
		return webpDimensions(header)
	default:
		return 0, 0
	}
}

// decodeHeader base64-decodes the leading imageHeaderProbeBytes of the payload,
// aligned to a 4-byte quantum so a truncated input still decodes.
func decodeHeader(encoded string) []byte {
	probe := encoded
	if len(probe) > imageHeaderProbeBytes {
		probe = probe[:imageHeaderProbeBytes]
	}
	if pad := len(probe) % 4; pad != 0 {
		probe = probe[:len(probe)-pad]
	}
	decoded, err := base64.StdEncoding.DecodeString(probe)
	if err == nil {
		return decoded
	}
	// Tolerate an unpadded or URL-safe payload rather than lose the
	// measurement: both spellings appear in the wild.
	trimmed := strings.TrimRight(probe, "=")
	if decoded, err = base64.RawStdEncoding.DecodeString(trimmed); err == nil {
		return decoded
	}
	if decoded, err = base64.RawURLEncoding.DecodeString(trimmed); err == nil {
		return decoded
	}
	return nil
}

func hasPNGSignature(b []byte) bool {
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	if len(b) < len(png) {
		return false
	}
	for i, c := range png {
		if b[i] != c {
			return false
		}
	}
	return true
}

// pngDimensions reads the IHDR chunk: the 8-byte signature, a 4-byte length, a
// 4-byte type, then width and height as big-endian uint32.
func pngDimensions(b []byte) (int, int) {
	const want = 24
	if len(b) < want {
		return 0, 0
	}
	width := int(be32(b[16:20]))
	height := int(be32(b[20:24]))
	return saneDimensions(width, height)
}

func hasJPEGSignature(b []byte) bool {
	return len(b) >= 3 && b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff
}

// jpegDimensions walks the JPEG segment chain to the first start-of-frame
// marker, whose payload carries precision, height, and width. Segments are
// length-prefixed, so the walk skips over EXIF and other metadata without
// decoding any pixels.
func jpegDimensions(b []byte) (int, int) {
	for i := 2; i+3 < len(b); {
		if b[i] != 0xff {
			return 0, 0
		}
		marker := b[i+1]
		if marker == 0xff { // fill byte
			i++
			continue
		}
		switch {
		case marker == 0xd8 || (marker >= 0xd0 && marker <= 0xd9) || marker == 0x01:
			i += 2 // standalone marker, no payload
			continue
		case marker == 0xda: // start of scan: the frame header always precedes it
			return 0, 0
		}
		if i+4 > len(b) {
			return 0, 0
		}
		if isSOFMarker(marker) {
			return sofDimensions(b, i)
		}
		length := int(be16(b[i+2 : i+4]))
		if length < 2 {
			return 0, 0
		}
		i += 2 + length
	}
	return 0, 0
}

// isSOFMarker reports whether marker starts a JPEG frame header: SOF0..SOF15
// carry the dimensions, except the DHT (C4), JPG (C8), and DAC (CC) markers.
func isSOFMarker(marker byte) bool {
	return marker >= 0xc0 && marker <= 0xcf && marker != 0xc4 && marker != 0xc8 && marker != 0xcc
}

// sofDimensions reads a frame header: precision(1) height(2) width(2) after the
// marker and its length.
func sofDimensions(b []byte, i int) (int, int) {
	if i+9 > len(b) {
		return 0, 0
	}
	height := int(be16(b[i+5 : i+7]))
	width := int(be16(b[i+7 : i+9]))
	return saneDimensions(width, height)
}

func hasGIFSignature(b []byte) bool {
	return len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a")
}

// gifDimensions reads the logical screen descriptor: width and height as
// little-endian uint16 starting at byte 6.
func gifDimensions(b []byte) (int, int) {
	if len(b) < 10 {
		return 0, 0
	}
	return saneDimensions(int(le16(b[6:8])), int(le16(b[8:10])))
}

func hasWebPSignature(b []byte) bool {
	return len(b) >= 16 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP"
}

// webpDimensions reads the container chunk: an extended-format VP8X header
// stores width and height minus one as 24-bit little-endian values, and a lossless
// VP8L header packs them as two 14-bit values.
func webpDimensions(b []byte) (int, int) {
	// 30 bytes cover every variant's header; the VP8L branch itself needs 25,
	// and a shorter slice simply falls to the default.
	if len(b) < 30 {
		return 0, 0
	}
	switch string(b[12:16]) {
	case "VP8X":
		width := int(le24(b[24:27])) + 1
		height := int(le24(b[27:30])) + 1
		return saneDimensions(width, height)
	case "VP8L":
		bits := uint32(b[21]) | uint32(b[22])<<8 | uint32(b[23])<<16 | uint32(b[24])<<24
		width := int(bits&0x3fff) + 1
		height := int((bits>>14)&0x3fff) + 1
		return saneDimensions(width, height)
	case "VP8 ":
		return vp8Dimensions(b)
	default:
		return 0, 0
	}
}

// vp8Dimensions reads a lossy (simple) WebP frame header: a 3-byte frame tag,
// the 3-byte start code 0x9d 0x01 0x2a, then the width and height as 14-bit
// little-endian values. This is the most common WebP form, so leaving it
// unparsed would cost every ordinary WebP the default.
func vp8Dimensions(b []byte) (int, int) {
	const start = 20 + 3 + 3
	if len(b) < start+4 {
		return 0, 0
	}
	if b[23] != 0x9d || b[24] != 0x01 || b[25] != 0x2a {
		return 0, 0
	}
	width := int(le16(b[26:28]) & 0x3fff)
	height := int(le16(b[28:30]) & 0x3fff)
	return saneDimensions(width, height)
}

// saneDimensions rejects a header whose values cannot be a real image, so a
// truncated or misidentified payload falls to the default cost.
func saneDimensions(width, height int) (int, int) {
	const maxEdge = 1 << 16
	if width <= 0 || height <= 0 || width > maxEdge || height > maxEdge {
		return 0, 0
	}
	return width, height
}

func be16(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }
func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
func le16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }
func le24(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
}

package tiktoken

import (
	"encoding/base64"
	"strings"
	"testing"
)

func pngHeader(width, height uint32) []byte {
	b := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	b = append(b, 0, 0, 0, 13) // IHDR length
	b = append(b, 'I', 'H', 'D', 'R')
	b = append(b, byte(width>>24), byte(width>>16), byte(width>>8), byte(width))
	b = append(b, byte(height>>24), byte(height>>16), byte(height>>8), byte(height))
	return b
}

func jpegHeader(width, height uint16) []byte {
	b := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10} // SOI + APP0 with a 16-byte payload
	b = append(b, []byte("JFIF\x00\x01\x02\x00\x00\x01\x00\x01\x00\x00")...)
	b = append(b, 0xff, 0xc0, 0x00, 0x11, 0x08) // SOF0
	b = append(b, byte(height>>8), byte(height), byte(width>>8), byte(width))
	return b
}

func gifHeader(width, height uint16) []byte {
	b := []byte("GIF89a")
	b = append(b, byte(width), byte(width>>8), byte(height), byte(height>>8))
	return b
}

// webpLossyHeader builds a simple (lossy) WebP: RIFF/WEBP, a "VP8 " chunk
// whose payload is a frame tag, the start code, and the 14-bit dimensions.
func webpLossyHeader(width, height uint16) []byte {
	b := []byte("RIFF")
	b = append(b, 0, 0, 0, 0)
	b = append(b, []byte("WEBP")...)
	b = append(b, []byte("VP8 ")...)
	b = append(b, 10, 0, 0, 0)      // chunk size
	b = append(b, 0x00, 0x00, 0x00) // frame tag
	b = append(b, 0x9d, 0x01, 0x2a) // start code
	b = append(b, byte(width), byte(width>>8), byte(height), byte(height>>8))
	return b
}

// jpegWithEXIF builds a JPEG whose APP1/EXIF segment is longer than the old
// 256-character probe, so the frame header sits deeper in the payload.
func jpegWithEXIF(metadataBytes int, width, height uint16) []byte {
	b := []byte{0xff, 0xd8, 0xff, 0xe1}
	length := metadataBytes + 2
	b = append(b, byte(length>>8), byte(length))
	b = append(b, make([]byte, metadataBytes)...)
	b = append(b, 0xff, 0xc0, 0x00, 0x11, 0x08)
	b = append(b, byte(height>>8), byte(height), byte(width>>8), byte(width))
	return b
}

func webpHeader(width, height uint32) []byte {
	b := []byte("RIFF")
	b = append(b, 0, 0, 0, 0)
	b = append(b, []byte("WEBP")...)
	b = append(b, []byte("VP8X")...)
	b = append(b, 10, 0, 0, 0) // chunk size
	b = append(b, 0, 0, 0, 0)  // flags + reserved
	w, h := width-1, height-1
	b = append(b, byte(w), byte(w>>8), byte(w>>16))
	b = append(b, byte(h), byte(h>>8), byte(h>>16))
	return b
}

func encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func TestImageTokens_Containers(t *testing.T) {
	tests := []struct {
		name      string
		mediaType string
		header    []byte
		want      int
	}{
		{"png", "image/png", pngHeader(1000, 800), AreaTokens(1000, 800)},
		{"jpeg", "image/jpeg", jpegHeader(1200, 600), AreaTokens(1200, 600)},
		{"gif", "image/gif", gifHeader(400, 300), AreaTokens(400, 300)},
		{"webp extended", "image/webp", webpHeader(800, 600), AreaTokens(800, 600)},
		{"webp lossy", "image/webp", webpLossyHeader(800, 600), AreaTokens(800, 600)},
		{"jpeg behind a 4 KiB exif block", "image/jpeg", jpegWithEXIF(4096, 1200, 600), AreaTokens(1200, 600)},
		{"png without a media type", "", pngHeader(1000, 800), AreaTokens(1000, 800)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ImageTokens(tt.mediaType, encode(tt.header)); got != tt.want {
				t.Fatalf("ImageTokens = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestImageTokens_FallsBackToDefault(t *testing.T) {
	tests := []struct {
		name      string
		mediaType string
		payload   string
	}{
		{"empty payload", "image/png", ""},
		{"remote url", "image/png", "https://example.invalid/cat.png"},
		{"unknown container", "image/tiff", encode([]byte("II*\x00garbage that is not an image header"))},
		{"declared but not encodable", "image/png", "!!!not base64!!!"},
		{"data url without base64", "image/png", "data:image/png,rawbytes"},
		{"truncated header", "image/png", encode([]byte{0x89, 'P', 'N', 'G'})},
		{"zero dimensions", "image/png", encode(pngHeader(0, 800))},
		{"absurd dimensions", "image/png", encode(pngHeader(1<<17, 10))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ImageTokens(tt.mediaType, tt.payload); got != DefaultImageTokens {
				t.Fatalf("ImageTokens = %d, want the default %d", got, DefaultImageTokens)
			}
		})
	}
}

func TestImageTokens_DataURL(t *testing.T) {
	payload := "data:image/png;base64," + encode(pngHeader(1000, 800))
	if got := ImageTokens("", payload); got != AreaTokens(1000, 800) {
		t.Fatalf("data URL: ImageTokens = %d, want %d", got, AreaTokens(1000, 800))
	}
	// The container signature decides the parser even when the caller's media
	// type disagrees: this is PNG bytes declared as JPEG.
	if got := ImageTokens("image/jpeg", payload); got != AreaTokens(1000, 800) {
		t.Fatalf("mismatched media type: ImageTokens = %d, want %d", got, AreaTokens(1000, 800))
	}
	// A payload that is neither a known signature nor the declared type falls
	// back to the default.
	if got := ImageTokens("image/jpeg", encode([]byte("not an image at all"))); got != DefaultImageTokens {
		t.Fatalf("unknown container: ImageTokens = %d, want the default", got)
	}
}

// TestImageTokens_UnpaddedAndURLSafe pins the tolerant decode: an unpadded or
// URL-safe base64 payload is still measured instead of falling to the default.
func TestImageTokens_UnpaddedAndURLSafe(t *testing.T) {
	// One extra byte makes the base64 carry padding; the parser only needs
	// the 24-byte header.
	header := append(pngHeader(1000, 800), 0x00)
	padded := base64.StdEncoding.EncodeToString(header)
	unpadded := strings.TrimRight(padded, "=")
	if unpadded == padded {
		t.Fatalf("fixture has no padding to strip: %q", padded)
	}
	if got := ImageTokens("image/png", unpadded); got != AreaTokens(1000, 800) {
		t.Fatalf("unpadded payload: ImageTokens = %d, want %d", got, AreaTokens(1000, 800))
	}

	// Trailing 0xff bytes force the URL-safe alphabet (- and _) without
	// disturbing the 24-byte header the parser reads.
	urlSafe := base64.URLEncoding.EncodeToString(append(pngHeader(1000, 800), 0xff, 0xff, 0xff))
	if !strings.ContainsAny(urlSafe, "-_") {
		t.Fatalf("fixture must exercise the URL-safe alphabet: %q", urlSafe)
	}
	if got := ImageTokens("image/png", urlSafe); got != AreaTokens(1000, 800) {
		t.Fatalf("URL-safe payload: ImageTokens = %d, want %d", got, AreaTokens(1000, 800))
	}
}

func TestAreaTokens_Clamps(t *testing.T) {
	tests := []struct {
		name          string
		width, height int
		want          int
	}{
		{"published ratio", 1000, 800, 1067},
		{"tiny image hits the floor", 10, 10, MinImageTokens},
		{"huge image hits the ceiling", 4000, 4000, MaxImageTokens},
		{"invalid area", 0, 0, DefaultImageTokens},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AreaTokens(tt.width, tt.height); got != tt.want {
				t.Fatalf("AreaTokens(%d, %d) = %d, want %d", tt.width, tt.height, got, tt.want)
			}
		})
	}
}

// TestImageTokens_IsBounded ensures the header probe cannot grow with the
// payload: a 4 MiB image costs the same work as a small one.
func TestImageTokens_IsBounded(t *testing.T) {
	huge := encode(append(pngHeader(1000, 800), make([]byte, 4<<20)...))
	if got := ImageTokens("image/png", huge); got != AreaTokens(1000, 800) {
		t.Fatalf("ImageTokens on a 4 MiB payload = %d, want %d", got, AreaTokens(1000, 800))
	}
	if len(huge) <= imageHeaderProbeBytes {
		t.Fatal("fixture is not large enough to exercise the probe bound")
	}
	// The probe itself must stay bounded: whatever the payload size, only the
	// declared prefix is decoded.
	decoded := decodeHeader(strings.Repeat("A", 4<<20))
	if len(decoded) > imageHeaderProbeBytes/4*3+3 {
		t.Fatalf("decodeHeader decoded %d bytes, exceeding the probe bound", len(decoded))
	}
}

func TestSplitDataURL(t *testing.T) {
	encoded, mediaType := splitDataURL("data:image/webp;base64,AAAA")
	if encoded != "AAAA" || mediaType != "image/webp" {
		t.Fatalf("splitDataURL = (%q, %q)", encoded, mediaType)
	}
	if encoded, mediaType := splitDataURL("AAAA"); encoded != "AAAA" || mediaType != "" {
		t.Fatalf("bare base64 = (%q, %q)", encoded, mediaType)
	}
	if encoded, _ := splitDataURL("https://example.invalid/x.png"); encoded == "" {
		t.Fatal("a non-data URL is returned as an opaque payload")
	}
	if encoded, _ := splitDataURL("data:image/png;base64"); encoded != "" {
		t.Fatal("a data URL without a payload has no encoded part")
	}
	if encoded, _ := splitDataURL("DATA:image/png;base64,AAAA"); encoded != "AAAA" {
		t.Fatalf("a case-variant scheme must still split: %q", encoded)
	}
}

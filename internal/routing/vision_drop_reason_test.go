package routing

import "testing"

// TestVisionDropReasonMalformed pins the AUD-41 fix (NENYA-244): an image
// part with neither a URL nor a media type is MALFORMED, not a format/media
// mismatch. Pre-fix it fell through to "format_media", blaming the target
// format for an image that never named one — the operator-facing counter
// then pointed at a format-capability problem that did not exist.
func TestVisionDropReasonMalformed(t *testing.T) {
	cases := []struct {
		name string
		site string
		ref  imageRef
		want string
	}{
		{
			name: "input_image without url or mime is malformed",
			site: "request",
			ref:  imageRef{kind: "input_image"},
			want: "malformed_image",
		},
		{
			name: "anthropic base64 without media_type is malformed",
			site: "tool_result",
			ref:  imageRef{kind: "image"},
			want: "malformed_image",
		},
		{
			name: "valid data-url image is kept",
			site: "request",
			ref:  imageRef{kind: "image_url", mime: "image/png"},
			want: "",
		},
		{
			name: "remote url keeps the openai reason path",
			site: "request",
			ref:  imageRef{kind: "image_url", remote: true},
			want: "",
		},
		{
			name: "remote url on anthropic tool_result is still format_remote_url",
			site: "tool_result",
			ref:  imageRef{kind: "image_url", remote: true},
			want: "format_remote_url",
		},
		{
			name: "a real media mismatch is still format_media",
			site: "tool_result",
			ref:  imageRef{kind: "image", mime: "image/xyz-unknown"},
			want: "format_media",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			format := "openai"
			if tc.want == "format_remote_url" {
				format = "anthropic"
			}
			if got := visionDropReason(tc.site, true, format, tc.ref); got != tc.want {
				t.Fatalf("visionDropReason(%q, vision, %q, %+v) = %q, want %q", tc.site, format, tc.ref, got, tc.want)
			}
		})
	}
}

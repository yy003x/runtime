package contract

import (
	"encoding/base64"
	"strings"
	"testing"
)

var tinyPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82,
}

func TestParseAndValidateImageContentParts(t *testing.T) {
	encoded, err := DataImageURL("image/png", tinyPNG)
	if err != nil {
		t.Fatal(err)
	}
	mediaType, data, locator, err := ParseImageURL(encoded)
	if err != nil || mediaType != "image/png" || locator != "" || data == "" {
		t.Fatalf("data url parse: media=%q locator=%q err=%v", mediaType, locator, err)
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		t.Fatal(err)
	}
	message := Message{
		Role: RoleUser,
		ContentParts: []ContentPart{
			{Type: ContentPartText, Text: "what is this"},
			{Type: ContentPartImageURL, ImageURL: &ImageURL{URL: encoded}},
		},
	}
	if err := message.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, _, locator, err := ParseImageURL("https://example.com/a.png"); err != nil || locator != "https://example.com/a.png" {
		t.Fatalf("https parse: %q %v", locator, err)
	}
}

func TestMessageValidationRejectsInvalidContentParts(t *testing.T) {
	encoded, err := DataImageURL("image/png", tinyPNG)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		message Message
	}{
		{
			name: "content_and_parts",
			message: Message{
				Role: RoleUser, Content: "hello",
				ContentParts: []ContentPart{{Type: ContentPartText, Text: "hello"}},
			},
		},
		{
			name: "assistant_parts",
			message: Message{
				Role:         RoleAssistant,
				ContentParts: []ContentPart{{Type: ContentPartText, Text: "hello"}},
			},
		},
		{
			name: "unknown_type",
			message: Message{
				Role:         RoleUser,
				ContentParts: []ContentPart{{Type: "image", Text: "no"}},
			},
		},
		{
			name: "declared_png_is_not_png",
			message: Message{
				Role: RoleUser,
				ContentParts: []ContentPart{{
					Type: ContentPartImageURL,
					ImageURL: &ImageURL{
						URL: "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("not-an-image")),
					},
				}},
			},
		},
		{
			name: "empty_text_part",
			message: Message{
				Role:         RoleUser,
				ContentParts: []ContentPart{{Type: ContentPartText, Text: " "}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.message.Validate(); err == nil {
				t.Fatal("invalid content_parts were accepted")
			}
		})
	}
	if err := (Message{
		Role: RoleUser, ContentParts: []ContentPart{
			{Type: ContentPartImageURL, ImageURL: &ImageURL{URL: encoded}},
		},
	}).Validate(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(encoded, "data:image/png;base64,") {
		t.Fatalf("encoded=%q", encoded)
	}
}

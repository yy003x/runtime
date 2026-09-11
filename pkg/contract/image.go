package contract

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
)

var imageMediaTypes = map[string]string{
	"image/jpeg": "image/jpeg",
	"image/jpg":  "image/jpeg",
	"image/png":  "image/png",
	"image/gif":  "image/gif",
	"image/webp": "image/webp",
}

// ParseImageURL accepts a Chat Completions image_url value: an http(s) locator
// passed through to the Provider, or a data:image/<type>;base64,... payload.
// For data URLs it returns the canonical media type and the raw base64 payload
// without the data: prefix. Runtime never fetches remote URLs.
func ParseImageURL(raw string) (mediaType, data, locator string, err error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", "", "", fmt.Errorf("image_url.url is required")
	}
	if strings.HasPrefix(value, "data:") {
		mediaType, data, err = parseDataImageURL(value)
		return mediaType, data, "", err
	}
	parsed, parseErr := url.Parse(value)
	if parseErr != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", "", "", fmt.Errorf("image_url.url must be an absolute HTTP(S) URL or a data:image base64 value")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", "", "", fmt.Errorf("image_url.url must use HTTP or HTTPS")
	}
	return "", "", value, nil
}

func parseDataImageURL(value string) (string, string, error) {
	payload := strings.TrimPrefix(value, "data:")
	mediaType, encoded, found := strings.Cut(payload, ";base64,")
	if !found || strings.TrimSpace(encoded) == "" {
		return "", "", fmt.Errorf("image_url.data URL must use data:image/<type>;base64,<data>")
	}
	canonical, exists := imageMediaTypes[strings.ToLower(strings.TrimSpace(mediaType))]
	if !exists {
		return "", "", fmt.Errorf("image_url.data URL media type must be jpeg, png, gif, or webp")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(decoded) == 0 {
		return "", "", fmt.Errorf("image_url.data URL must contain standard base64")
	}
	if len(decoded) > MaxDecodedImageBytes {
		return "", "", fmt.Errorf("image_url decoded payload exceeds %d bytes", MaxDecodedImageBytes)
	}
	detected := DetectImageMediaType(decoded)
	if detected == "" {
		return "", "", fmt.Errorf("image_url.data URL must contain jpeg, png, gif, or webp bytes")
	}
	if detected != canonical {
		return "", "", fmt.Errorf("image_url.data URL media type does not match decoded bytes")
	}
	return canonical, encoded, nil
}

// DetectImageMediaType returns image/jpeg, image/png, image/gif, or image/webp
// from magic bytes. Unknown payloads return an empty string.
func DetectImageMediaType(data []byte) string {
	switch {
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg"
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "image/gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	default:
		return ""
	}
}

// DataImageURL builds a canonical data URL from raw image bytes.
func DataImageURL(mediaType string, data []byte) (string, error) {
	canonical, exists := imageMediaTypes[strings.ToLower(strings.TrimSpace(mediaType))]
	if !exists || len(data) == 0 {
		return "", fmt.Errorf("image bytes and media type are required")
	}
	if len(data) > MaxDecodedImageBytes {
		return "", fmt.Errorf("image exceeds %d bytes", MaxDecodedImageBytes)
	}
	detected := DetectImageMediaType(data)
	if detected == "" {
		return "", fmt.Errorf("image bytes must be jpeg, png, gif, or webp")
	}
	if detected != canonical {
		return "", fmt.Errorf("image media type does not match decoded bytes")
	}
	return "data:" + canonical + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

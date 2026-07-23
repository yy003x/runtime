package llm

import (
	"fmt"
	"net/url"
	"strings"
)

// ResolveCompatibleEndpoint resolves an OpenAI/Anthropic-compatible resource
// against a provider base URL. A base URL may stop before the API version or
// include an explicit version segment such as /v1; the version is added only
// when it is absent.
func ResolveCompatibleEndpoint(baseURL, resource string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("invalid API base_url: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("invalid API base_url: absolute http(s) URL is required")
	}
	if parsed.Fragment != "" {
		return "", fmt.Errorf("invalid API base_url: fragment is not allowed")
	}

	resourcePath, err := normalizeResourcePath(resource)
	if err != nil {
		return "", err
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	if pathHasSuffix(basePath, resourcePath) {
		parsed.Path = basePath
		parsed.RawPath = ""
		return parsed.String(), nil
	}
	if !isVersionSegment(lastPathSegment(basePath)) {
		basePath += "/v1"
	}
	parsed.Path = strings.TrimRight(basePath, "/") + "/" + resourcePath
	parsed.RawPath = ""
	return parsed.String(), nil
}

func normalizeResourcePath(resource string) (string, error) {
	resource = strings.Trim(strings.TrimSpace(resource), "/")
	if resource == "" {
		return "", fmt.Errorf("invalid API resource: path is required")
	}
	for _, segment := range strings.Split(resource, "/") {
		if segment == "" || segment == "." || segment == ".." || strings.ContainsAny(segment, "?#") {
			return "", fmt.Errorf("invalid API resource: %q", resource)
		}
	}
	return resource, nil
}

func pathHasSuffix(basePath, resourcePath string) bool {
	baseSegments := splitPath(basePath)
	resourceSegments := splitPath(resourcePath)
	if len(baseSegments) < len(resourceSegments) {
		return false
	}
	offset := len(baseSegments) - len(resourceSegments)
	for index := range resourceSegments {
		if baseSegments[offset+index] != resourceSegments[index] {
			return false
		}
	}
	return true
}

func lastPathSegment(value string) string {
	segments := splitPath(value)
	if len(segments) == 0 {
		return ""
	}
	return segments[len(segments)-1]
}

func splitPath(value string) []string {
	raw := strings.Split(strings.Trim(value, "/"), "/")
	segments := make([]string, 0, len(raw))
	for _, segment := range raw {
		if segment != "" {
			segments = append(segments, segment)
		}
	}
	return segments
}

func isVersionSegment(value string) bool {
	if len(value) < 2 || value[0] != 'v' {
		return false
	}
	for _, char := range value[1:] {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

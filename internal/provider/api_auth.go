package provider

import (
	"net/url"
	"strings"
)

type apiAuth struct {
	Header string
	Prefix string
}

func defaultAPIAuth(protocol, baseURL string) apiAuth {
	if protocol != "anthropic" {
		return apiAuth{Header: "Authorization", Prefix: "Bearer "}
	}
	if isOpenRouterEndpoint(baseURL) {
		return apiAuth{Header: "Authorization", Prefix: "Bearer "}
	}
	return apiAuth{Header: "x-api-key"}
}

func isOpenRouterEndpoint(baseURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	return host == "openrouter.ai" || strings.HasSuffix(host, ".openrouter.ai")
}

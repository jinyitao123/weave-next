package runtimehost

import (
	"net/url"
	"strings"
)

// SafeEndpointOrigin exposes provider location without paths or credentials.
func SafeEndpointOrigin(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

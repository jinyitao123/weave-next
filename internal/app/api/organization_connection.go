package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/labstack/echo/v4"
)

type organizationConnection struct {
	Version     int    `json:"version"`
	ForgeOrigin string `json:"forgeOrigin"`
	WeaveOrigin string `json:"weaveOrigin"`
}

var connectionHostLabel = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
var connectionNumericLabel = regexp.MustCompile(`^(?:[0-9]+|0x[0-9a-fA-F]+)$`)

func (s *Server) handleOrganizationConnection(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	if s.Config == nil || s.Config.OrganizationConnectionFile == "" {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "organization_connection_unavailable"})
	}
	content, err := readOrganizationConnection(s.Config.OrganizationConnectionFile)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "organization_connection_invalid"})
	}
	c.Response().Header().Set("Content-Disposition", `attachment; filename="organization-connection.json"`)
	c.Response().Header().Set("X-Content-Type-Options", "nosniff")
	return c.Blob(http.StatusOK, echo.MIMEApplicationJSON, content)
}

func readOrganizationConnection(filename string) ([]byte, error) {
	before, err := os.Lstat(filename)
	if err != nil || !before.Mode().IsRegular() || before.Size() > 8192 {
		return nil, errors.New("invalid connection file")
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, errors.New("connection file changed")
	}
	data, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil || len(data) > 8192 || !utf8.Valid(data) {
		return nil, errors.New("invalid connection bytes")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, errors.New("connection must be an object")
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || (name != "version" && name != "forgeOrigin" && name != "weaveOrigin") || fields[name] != nil {
			return nil, errors.New("invalid connection fields")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF || len(fields) != 3 {
		return nil, errors.New("invalid connection document")
	}
	var version float64
	var forge, weave string
	if json.Unmarshal(fields["version"], &version) != nil || version != 1 || json.Unmarshal(fields["forgeOrigin"], &forge) != nil || json.Unmarshal(fields["weaveOrigin"], &weave) != nil {
		return nil, errors.New("invalid connection values")
	}
	forge, err = connectionOrigin(forge)
	if err != nil {
		return nil, err
	}
	weave, err = connectionOrigin(weave)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(organizationConnection{1, forge, weave}); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func connectionOrigin(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if len(value) > 2048 || strings.ContainsAny(value, "\\?#") || strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return "", errors.New("invalid origin")
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || strings.Contains(parsed.Hostname(), "%") || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("invalid origin")
	}
	port := parsed.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", errors.New("invalid port")
		}
		port = strconv.Itoa(n)
	}
	host := strings.ToLower(parsed.Hostname())
	if address, err := netip.ParseAddr(host); err == nil {
		host = address.String()
		if address.Is4In6() {
			b := address.As16()
			host = fmt.Sprintf("::ffff:%x:%x", uint16(b[12])<<8|uint16(b[13]), uint16(b[14])<<8|uint16(b[15]))
		}
	} else {
		labels := strings.Split(strings.TrimRight(host, "."), ".")
		if len(strings.TrimRight(host, ".")) > 253 || connectionNumericLabel.MatchString(labels[len(labels)-1]) {
			return "", errors.New("invalid host")
		}
		for _, label := range labels {
			if !connectionHostLabel.MatchString(label) {
				return "", errors.New("invalid host")
			}
		}
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" && !(parsed.Scheme == "http" && port == "80") && !(parsed.Scheme == "https" && port == "443") {
		host += ":" + port
	}
	return parsed.Scheme + "://" + host, nil
}

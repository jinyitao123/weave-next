package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/config"
)

func TestOrganizationConnectionDownloadPermissionsAndFreshBytes(t *testing.T) {
	file := filepath.Join(t.TempDir(), "connection.json")
	write := func(forge string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(`{"version":1,"forgeOrigin":"`+forge+`","weaveOrigin":"https://teams.example.test"}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("http://forge.example.test:8080")
	s := adminConsoleServer(&config.Config{OrganizationConnectionFile: file})
	call := func(role string) *httptest.ResponseRecorder {
		return serveAdmin(s, adminRequest("GET", "/v1/admin/organization-connection", "", func(r *http.Request) {
			if role != "" {
				token, err := s.signJWT("workspace", "employee", []string{role})
				if err != nil {
					t.Fatal(err)
				}
				r.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: token})
			}
		}))
	}
	for role, status := range map[string]int{"": 401, "member": 403, "developer": 403, "admin": 200, "owner": 200} {
		if response := call(role); response.Code != status {
			t.Fatalf("role %q returned %d", role, response.Code)
		}
	}
	first := call("admin")
	if first.Header().Get("Content-Disposition") != `attachment; filename="organization-connection.json"` || first.Header().Get("Cache-Control") != "no-store" || first.Header().Get("Content-Type") != "application/json" {
		t.Fatal("download headers missing")
	}
	if !bytes.Equal(first.Body.Bytes(), call("admin").Body.Bytes()) {
		t.Fatal("unchanged configuration changed bytes")
	}
	write("https://new.example.test")
	updated := call("admin")
	if updated.Code != 200 || bytes.Equal(first.Body.Bytes(), updated.Body.Bytes()) || !strings.Contains(updated.Body.String(), "https://new.example.test") {
		t.Fatal("updated installation projection was not read")
	}
	os.Remove(file)
	missing := call("admin")
	if missing.Code != 503 || strings.Contains(missing.Body.String(), "forgeOrigin") {
		t.Fatal("missing state produced a connection document")
	}
}

func TestOrganizationConnectionBytesMatchInstallerSerialization(t *testing.T) {
	for _, pair := range [][2]string{{"http://forge.example.test:8080", "https://teams.example.test"}, {"http://[2001:db8::1]:8080", "https://xn--fiqs8s.example"}, {"http://[::ffff:c000:201]:8080", "http://127.0.0.1:8081"}} {
		value := organizationConnection{1, pair[0], pair[1]}
		compact, _ := json.Marshal(value)
		file := filepath.Join(t.TempDir(), "connection.json")
		if err := os.WriteFile(file, compact, 0600); err != nil {
			t.Fatal(err)
		}
		actual, err := readOrganizationConnection(file)
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command("python3", "-c", "import sys,json; v=json.load(sys.stdin);sys.stdout.buffer.write((json.dumps(v,ensure_ascii=False,indent=2)+'\\n').encode('utf-8'))")
		command.Stdin = bytes.NewReader(compact)
		expected, err := command.Output()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(actual, expected) {
			t.Fatalf("serialization differs for %v", pair)
		}
	}
}

func TestOrganizationConnectionRefusesInvalidFiles(t *testing.T) {
	valid := `{"version":1,"forgeOrigin":"https://forge.example.test","weaveOrigin":"https://teams.example.test"}`
	cases := []string{`{"version":1}`, strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1), strings.Replace(valid, `"version":1`, `"version":1,"password":"must-not-be-exported"`, 1), valid + `{}`, strings.Repeat(" ", 8193), string([]byte{0xff}), `[]`}
	for _, origin := range []string{"", "https://user:secret@example.test", "https://example.test/path", "https://example.test?", "https://example.test#", "https://example.test\\path", "http://example.test:99999", "http://bad_host", "http://127.1", "http://[fe80::1%25en0]"} {
		cases = append(cases, strings.Replace(valid, "https://forge.example.test", origin, 1))
	}
	for i, data := range cases {
		file := filepath.Join(t.TempDir(), "connection.json")
		os.WriteFile(file, []byte(data), 0600)
		if _, err := readOrganizationConnection(file); err == nil {
			t.Errorf("invalid case %d accepted", i)
		}
	}
	file := filepath.Join(t.TempDir(), "connection.json")
	os.WriteFile(file, []byte(valid), 0600)
	t.Run("symlink", func(t *testing.T) {
		link := file + ".link"
		if err := os.Symlink(file, link); err != nil {
			if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(1314)) {
				t.Skip("Windows account lacks symbolic-link privilege")
			}
			t.Fatal(err)
		}
		if _, err := readOrganizationConnection(link); err == nil {
			t.Fatal("symlink accepted")
		}
	})
	if _, err := readOrganizationConnection(filepath.Dir(file)); err == nil {
		t.Fatal("directory accepted")
	}
	s := adminConsoleServer(nil)
	token, _ := s.signJWT("workspace", "employee", []string{"admin"})
	response := serveAdmin(s, adminRequest("GET", "/v1/admin/organization-connection", "", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }))
	if response.Code != 503 || !strings.Contains(response.Body.String(), "organization_connection_unavailable") {
		t.Fatal("unconfigured deployment did not explain the failure")
	}
}

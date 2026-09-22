package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifyForgeFilesReadsExactFrozenBytes(t *testing.T) {
	content := []byte("contract version one")
	digest := sha256.Sum256(content)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/storage/files/file-1" || r.Header.Get("Authorization") != "Bearer employee-token" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write(content)
	}))
	defer server.Close()
	resource := dispatchInputResource{Type: "forge-file", ID: "file-1", Name: "合同.md", Bytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:])}
	if err := verifyForgeFiles(t.Context(), server.URL, "employee-token", []dispatchInputResource{resource}); err != nil {
		t.Fatal(err)
	}
	resource.SHA256 = string(make([]byte, 64))
	if err := verifyForgeFiles(t.Context(), server.URL, "employee-token", []dispatchInputResource{resource}); err == nil {
		t.Fatal("expected a mismatched frozen digest to be rejected")
	}
}

package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

func TestVerifyForgeFilesUsesExactFrozenOriginalSourceAndSHA(t *testing.T) {
	ownerBytes := []byte("%PDF-1.7\nowner original")
	approvalBytes := []byte("%PDF-1.7\napproval original")
	ownerSHA := sha256.Sum256(ownerBytes)
	approvalSHA := sha256.Sum256(approvalBytes)
	var paths []string
	successfulReads := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer employee-token" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body []byte
		var digest string
		if request.URL.Path == "/api/v1/workbench/materials/file-owner/original" {
			body, digest = ownerBytes, hex.EncodeToString(ownerSHA[:])
		} else if request.URL.Path == "/api/v1/approvals/requests/approval-1/workbench-context/files/file-approval/original" {
			body, digest = approvalBytes, hex.EncodeToString(approvalSHA[:])
		} else {
			http.NotFound(writer, request)
			return
		}
		paths = append(paths, request.URL.Path)
		if request.Header.Get("If-Match") != `"`+digest+`"` || request.Header.Get("Accept-Encoding") != "identity" {
			http.Error(writer, "stale source", http.StatusPreconditionFailed)
			return
		}
		successfulReads++
		writer.Header().Set("Content-Type", "application/pdf")
		writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
		writer.Header().Set("ETag", `"`+digest+`"`)
		writer.Header().Set("X-Content-SHA256", digest)
		_, _ = writer.Write(body)
	}))
	defer server.Close()

	resources := []dispatchInputResource{
		{Type: "forge-file", SourceKind: "owner", ID: "file-owner", Name: "owner.pdf", MediaType: "application/pdf", Bytes: int64(len(ownerBytes)), SHA256: hex.EncodeToString(ownerSHA[:])},
		{Type: "forge-file", SourceKind: "approval", RequestID: "approval-1", ID: "file-approval", Name: "approval.pdf", MediaType: "application/pdf", Bytes: int64(len(approvalBytes)), SHA256: hex.EncodeToString(approvalSHA[:])},
	}
	if err := verifyForgeFiles(t.Context(), server.URL, "employee-token", resources); err != nil {
		t.Fatal(err)
	}
	if successfulReads != 2 {
		t.Fatalf("expected exactly two validated original reads, got %d", successfulReads)
	}
	if strings.Join(paths, ",") != "/api/v1/workbench/materials/file-owner/original,/api/v1/approvals/requests/approval-1/workbench-context/files/file-approval/original" {
		t.Fatalf("binary reads did not use their frozen source routes: %s", strings.Join(paths, ","))
	}

	paths = nil
	resources[0].SHA256 = fmt.Sprintf("%064x", 1)
	if err := verifyForgeFiles(t.Context(), server.URL, "employee-token", resources[:1]); err == nil {
		t.Fatal("accepted an owner original under a changed If-Match digest")
	}
	if successfulReads != 2 {
		t.Fatalf("stale If-Match returned an original body: successful reads=%d", successfulReads)
	}
	missingSource := resources[0]
	missingSource.SourceKind = ""
	priorPathCount := len(paths)
	if err := verifyForgeFiles(t.Context(), server.URL, "employee-token", []dispatchInputResource{missingSource}); err == nil {
		t.Fatal("binary source without a frozen sourceKind was accepted")
	}
	if len(paths) != priorPathCount {
		t.Fatalf("missing source identity fell back to another route: %v", paths)
	}
}

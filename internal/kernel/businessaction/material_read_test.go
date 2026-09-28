package businessaction

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
)

func materialSnapshotForTest(materialID, name, mediaType string, original []byte, text, status, extractor string, limitations []string) taskMaterialSnapshot {
	originalBytes, extractedBytes := int64(len(original)), int64(len([]byte(text)))
	if limitations == nil {
		limitations = []string{}
	}
	coverage := json.RawMessage(`{"pdfPageCount":2,"pdfTextPageCount":1,"pdfPagesWithoutText":[2]}`)
	return taskMaterialSnapshot{
		MaterialID: materialID, Name: name, MediaType: mediaType, Bytes: &originalBytes,
		SHA256: dispatchInputDigestBytes(original),
		Extraction: &materialExtractionSnapshot{
			Status: status, MediaType: "text/plain; charset=utf-8", Bytes: &extractedBytes,
			SHA256: dispatchInputDigestBytes([]byte(text)), SourceSHA256: dispatchInputDigestBytes(original),
			Content: text, Extractor: extractor, Coverage: coverage, Limitations: limitations,
		},
	}
}

func TestPrepareExecutionTaskRemovesExtractionBodyAndKeepsFrozenManifest(t *testing.T) {
	original := []byte("frozen source file")
	content := "CONFIDENTIAL-EXTRACTION-BODY：合同正文"
	material := materialSnapshotForTest("eeeeeeeeeeeeeeeeeeeeeeee", "合同.txt", "text/plain", original,
		content, "complete", "utf8", nil)
	task, err := json.Marshal(taskMaterialEnvelope{
		Goal: "核对本次材料中的履约期限", MaterialHandling: "仅依据已冻结的材料",
		BusinessDataHandling: "不得提交业务动作", Materials: []taskMaterialSnapshot{material},
	})
	if err != nil {
		t.Fatal(err)
	}
	originalTask := string(task)
	resource := FrozenMaterialResource{
		Type: "forge-file", MaterialID: material.MaterialID, FileID: "forge-file-text",
		Name: material.Name, MediaType: material.MediaType, Bytes: int64(len(original)), SHA256: material.SHA256,
	}
	projected, recognized, err := PrepareExecutionTask(string(task), []FrozenMaterialResource{resource})
	if err != nil || !recognized {
		t.Fatalf("material task projection not recognized: recognized=%v err=%v", recognized, err)
	}
	if strings.Contains(projected, content) || strings.Contains(projected, "\"content\"") ||
		!strings.Contains(projected, "read_frozen_material") || !strings.Contains(projected, material.MaterialID) ||
		!strings.Contains(projected, material.SHA256) || !strings.Contains(projected, material.Extraction.SHA256) ||
		!strings.Contains(projected, "complete") || !strings.Contains(projected, "originalVerificationStatus=unavailable") ||
		!strings.Contains(projected, "不得提交业务动作") {
		t.Fatalf("execution projection either leaked body or lost frozen metadata: %s", projected)
	}
	// The durable input/hash remain the original caller facts; projection only
	// changes the separate runtime task that is sent to Loom.
	if string(task) != originalTask || !strings.Contains(originalTask, content) {
		t.Fatal("test fixture did not retain the original frozen extraction input")
	}

	plain := "把日期格式规范化"
	unchanged, recognized, err := PrepareExecutionTask(plain, nil)
	if err != nil || recognized || unchanged != plain {
		t.Fatalf("non-material text task changed semantics: got=%q recognized=%v err=%v", unchanged, recognized, err)
	}
	for _, taskWithoutMaterials := range []string{"", `{"goal":"仅整理这段说明","materials":[]}`} {
		unchanged, recognized, err := PrepareExecutionTask(taskWithoutMaterials, nil)
		if err != nil || recognized || unchanged != taskWithoutMaterials {
			t.Fatalf("empty/no-material task changed semantics: got=%q recognized=%v err=%v", unchanged, recognized, err)
		}
	}

	wrongResource := resource
	wrongResource.SHA256 = strings.Repeat("f", 64)
	if _, recognized, err := PrepareExecutionTask(string(task), []FrozenMaterialResource{wrongResource}); err == nil || !recognized {
		t.Fatalf("mismatched Forge resource was not rejected: recognized=%v err=%v", recognized, err)
	}

	// The shared contract leaves materialId optional for some text resources.
	// Keep those readable through their same-row fileId without exposing text
	// bodies or pretending the fileId is a materialId.
	withoutMaterialID := material
	withoutMaterialID.MaterialID = ""
	textTask, err := json.Marshal(taskMaterialEnvelope{Goal: "读取旧文本材料", Materials: []taskMaterialSnapshot{withoutMaterialID}})
	if err != nil {
		t.Fatal(err)
	}
	textResource := resource
	textResource.MaterialID = ""
	textProjection, recognized, err := PrepareExecutionTask(string(textTask), []FrozenMaterialResource{textResource})
	if err != nil || !recognized || strings.Contains(textProjection, content) || !strings.Contains(textProjection, `"fileId":"forge-file-text"`) {
		t.Fatalf("optional materialId text projection lost its scoped fallback: recognized=%v projected=%s err=%v", recognized, textProjection, err)
	}
	files := bindTaskMaterialExtractions(string(textTask), []delegatedResource{{
		Type: "forge-file", ID: textResource.FileID, Name: textResource.Name, MediaType: textResource.MediaType,
		Bytes: textResource.Bytes, SHA256: textResource.SHA256,
	}}, dispatchInputDigestBytes(textTask))
	read, err := readFrozenMaterial(files, materialReadArguments{FileID: textResource.FileID, SHA256: textResource.SHA256})
	if err != nil || read.Status != "complete" || !strings.Contains(read.Content, content) {
		t.Fatalf("optional materialId fallback could not read its frozen text: result=%+v err=%v", read, err)
	}
}

func TestPrepareBinaryMaterialFreezesApprovalSourceButKeepsItOutOfModelManifest(t *testing.T) {
	original := []byte("%PDF-1.7\napproval original bytes")
	content := "审批附件中可提取的条款"
	material := materialSnapshotForTest("abababababababababababab", "审批附件.pdf", "application/pdf", original,
		content, "partial", "pdfjs-dist", []string{"page-without-text"})
	task, err := json.Marshal(taskMaterialEnvelope{Goal: "核对审批附件条款", Materials: []taskMaterialSnapshot{material}})
	if err != nil {
		t.Fatal(err)
	}
	resource := FrozenMaterialResource{
		Type: "forge-file", SourceKind: "approval", RequestID: "approval-request-1",
		MaterialID: material.MaterialID, FileID: "approval-file-1", Name: material.Name,
		MediaType: material.MediaType, Bytes: int64(len(original)), SHA256: material.SHA256,
	}
	projected, recognized, err := PrepareExecutionTask(string(task), []FrozenMaterialResource{resource})
	if err != nil || !recognized {
		t.Fatalf("approval PDF source projection failed: recognized=%v err=%v", recognized, err)
	}
	if strings.Contains(projected, content) || strings.Contains(projected, "approval-request-1") || strings.Contains(projected, `"sourceKind"`) {
		t.Fatalf("binary source context or extraction body leaked into Loom prompt: %s", projected)
	}
	delegated := delegatedResource{
		Type: resource.Type, SourceKind: resource.SourceKind, RequestID: resource.RequestID,
		MaterialID: resource.MaterialID, ID: resource.FileID, Name: resource.Name,
		MediaType: resource.MediaType, Bytes: resource.Bytes, SHA256: resource.SHA256,
	}
	files := bindTaskMaterialExtractions(string(task), []delegatedResource{delegated}, dispatchInputDigestBytes(task))
	file := files[resource.FileID]
	if !file.Available || file.SourceKind != "approval" || file.RequestID != resource.RequestID {
		t.Fatalf("frozen approval route identity was not bound server-side: %+v", file)
	}
	chunk, err := readFrozenMaterial(files, materialReadArguments{MaterialID: resource.MaterialID, SHA256: resource.SHA256})
	if err != nil || chunk.Status != "partial" || chunk.Content != content {
		t.Fatalf("original verification must preserve extracted text and partial status: result=%+v err=%v", chunk, err)
	}

	resource.SourceKind, resource.RequestID = "", ""
	if _, recognized, err := PrepareExecutionTask(string(task), []FrozenMaterialResource{resource}); err == nil || !recognized {
		t.Fatalf("accepted binary material without a frozen source route: recognized=%v err=%v", recognized, err)
	}
}

func TestFrozenMaterialReadCannotCrossInputManifest(t *testing.T) {
	originalA, originalB := []byte("original A bytes"), []byte("original B bytes")
	materialA := materialSnapshotForTest("aaaaaaaaaaaaaaaaaaaaaaaa", "合同A.pdf", "application/pdf", originalA,
		"甲文件第一段。"+strings.Repeat("A内容。", 8), "partial", "pdfjs-dist", []string{"page-without-text"})
	materialB := materialSnapshotForTest("bbbbbbbbbbbbbbbbbbbbbbbb", "合同B.pdf", "application/pdf", originalB,
		"B-SECRET另一份合同内容", "complete", "pdfjs-dist", nil)
	taskA, err := json.Marshal(taskMaterialEnvelope{Materials: []taskMaterialSnapshot{materialA}})
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := json.Marshal(taskMaterialEnvelope{Materials: []taskMaterialSnapshot{materialB}})
	if err != nil {
		t.Fatal(err)
	}
	resourceA := delegatedResource{
		Type: "forge-file", SourceKind: "owner", MaterialID: materialA.MaterialID, ID: "forge-file-A", Name: materialA.Name,
		MediaType: materialA.MediaType, Bytes: int64(len(originalA)), SHA256: materialA.SHA256,
	}
	resourceB := delegatedResource{
		Type: "forge-file", SourceKind: "owner", MaterialID: materialB.MaterialID, ID: "forge-file-B", Name: materialB.Name,
		MediaType: materialB.MediaType, Bytes: int64(len(originalB)), SHA256: materialB.SHA256,
	}
	filesA := bindTaskMaterialExtractions(string(taskA), []delegatedResource{resourceA}, dispatchInputDigestBytes(taskA))
	if !filesA[resourceA.ID].Available {
		t.Fatalf("valid input A extraction was rejected: %+v", filesA[resourceA.ID])
	}
	maxBytes := 19
	resultA, err := readFrozenMaterial(filesA, materialReadArguments{MaterialID: resourceA.MaterialID, SHA256: resourceA.SHA256, MaxBytes: &maxBytes})
	if err != nil || resultA.Status != "partial" || resultA.Source != "workbench-extraction" ||
		resultA.OriginalVerificationStatus != "unavailable" || resultA.OriginalVerificationCode != "original_not_verified" ||
		resultA.MaterialID != resourceA.MaterialID || resultA.SHA256 != resourceA.SHA256 || resultA.Extractor != "pdfjs-dist" ||
		resultA.Coverage == nil || len(resultA.Limitations) != 1 || !resultA.HasMore || resultA.NextOffset == nil ||
		!utf8.ValidString(resultA.Content) || resultA.Content == "" {
		t.Fatalf("valid input A read lost its bound provenance or partial status: result=%+v err=%v", resultA, err)
	}

	// Input B is also well formed, but its file id/hash are absent from A's
	// immutable delegation resources, so a model cannot switch to that material.
	if !strings.Contains(string(taskB), "B-SECRET") || resourceB.ID == resourceA.ID {
		t.Fatal("cross-input fixture is not distinct")
	}
	resultB, err := readFrozenMaterial(filesA, materialReadArguments{MaterialID: resourceB.MaterialID, SHA256: resourceB.SHA256})
	if err != nil || resultB.Status != "unavailable" || resultB.Reason != "material_not_in_current_run" ||
		strings.Contains(resultB.Content, "B-SECRET") || resultB.MaterialID != "" {
		t.Fatalf("input A exposed input B material: result=%+v err=%v", resultB, err)
	}

	wrongHash, err := readFrozenMaterial(filesA, materialReadArguments{MaterialID: resourceA.MaterialID, SHA256: resourceB.SHA256})
	if err != nil || wrongHash.Status != "unavailable" || wrongHash.Content != "" {
		t.Fatalf("material read accepted a digest mismatch: result=%+v err=%v", wrongHash, err)
	}
}

func TestFrozenMaterialReadRejectsMismatchedExtractionSource(t *testing.T) {
	original := []byte("original file")
	material := materialSnapshotForTest("cccccccccccccccccccccccc", "source.txt", "text/plain", original,
		"unbound extraction", "complete", "utf8", nil)
	material.Extraction.SourceSHA256 = strings.Repeat("d", 64)
	task, err := json.Marshal(taskMaterialEnvelope{Materials: []taskMaterialSnapshot{material}})
	if err != nil {
		t.Fatal(err)
	}
	resource := delegatedResource{
		Type: "forge-file", MaterialID: material.MaterialID, ID: "file-source", Name: material.Name,
		MediaType: material.MediaType, Bytes: int64(len(original)), SHA256: dispatchInputDigestBytes(original),
	}
	files := bindTaskMaterialExtractions(string(task), []delegatedResource{resource}, dispatchInputDigestBytes(task))
	if files[resource.ID].Available {
		t.Fatal("accepted extraction that was not bound to the frozen original SHA-256")
	}
	result, err := readFrozenMaterial(files, materialReadArguments{MaterialID: resource.MaterialID, SHA256: resource.SHA256})
	if err != nil || result.Status != "unavailable" || result.Content != "" || result.Reason != "frozen_extraction_digest_mismatch" {
		t.Fatalf("mismatched source text escaped: result=%+v err=%v", result, err)
	}
}

func TestFrozenMaterialReadDoesNotClaimUnavailableBinaryContentWasRead(t *testing.T) {
	original := []byte("opaque PDF bytes")
	material := materialSnapshotForTest("dddddddddddddddddddddddd", "扫描件.pdf", "application/pdf", original,
		"[PDF 正文没有可提取文本]", "unsupported", "pdfjs-dist", []string{"page-without-text", "embedded-image"})
	task, err := json.Marshal(taskMaterialEnvelope{Materials: []taskMaterialSnapshot{material}})
	if err != nil {
		t.Fatal(err)
	}
	resource := delegatedResource{
		Type: "forge-file", SourceKind: "owner", MaterialID: material.MaterialID, ID: "file-binary", Name: material.Name,
		MediaType: material.MediaType, Bytes: int64(len(original)), SHA256: dispatchInputDigestBytes(original),
	}
	files := bindTaskMaterialExtractions(string(task), []delegatedResource{resource}, dispatchInputDigestBytes(task))
	result, err := readFrozenMaterial(files, materialReadArguments{MaterialID: resource.MaterialID, SHA256: resource.SHA256})
	if err != nil || result.Status != "unavailable" || result.ExtractionStatus != "unsupported" ||
		result.OriginalVerificationStatus != "unavailable" || result.OriginalVerificationCode != "original_not_verified" || result.Content != "" {
		t.Fatalf("unsupported binary source was reported as read: result=%+v err=%v", result, err)
	}
}

func TestFrozenMaterialReadEnforcesUTF8ChunkOffsetsAndLimits(t *testing.T) {
	original := []byte("original source")
	content := "甲🙂乙"
	material := materialSnapshotForTest("ffffffffffffffffffffffff", "utf8.txt", "text/plain", original,
		content, "complete", "utf8", nil)
	task, err := json.Marshal(taskMaterialEnvelope{Materials: []taskMaterialSnapshot{material}})
	if err != nil {
		t.Fatal(err)
	}
	resource := delegatedResource{
		Type: "forge-file", MaterialID: material.MaterialID, ID: "file-utf8", Name: material.Name,
		MediaType: material.MediaType, Bytes: int64(len(original)), SHA256: material.SHA256,
	}
	files := bindTaskMaterialExtractions(string(task), []delegatedResource{resource}, dispatchInputDigestBytes(task))
	if !files[resource.ID].Available {
		t.Fatalf("UTF-8 fixture was not accepted: %+v", files[resource.ID])
	}

	first, err := readFrozenMaterial(files, materialReadArguments{MaterialID: material.MaterialID, SHA256: material.SHA256, MaxBytes: intPointer(4)})
	if err != nil || first.Content != "甲" || first.Offset != 0 || first.NextOffset == nil || *first.NextOffset != len([]byte("甲")) {
		t.Fatalf("first chunk did not stop on a UTF-8 boundary: result=%+v err=%v", first, err)
	}
	second, err := readFrozenMaterial(files, materialReadArguments{
		MaterialID: material.MaterialID, SHA256: material.SHA256, Offset: first.NextOffset, MaxBytes: intPointer(4),
	})
	if err != nil || second.Content != "🙂" || second.Offset != len([]byte("甲")) || second.NextOffset == nil ||
		*second.NextOffset != len([]byte("甲🙂")) {
		t.Fatalf("second UTF-8 chunk was malformed: result=%+v err=%v", second, err)
	}
	last, err := readFrozenMaterial(files, materialReadArguments{
		MaterialID: material.MaterialID, SHA256: material.SHA256, Offset: second.NextOffset,
	})
	if err != nil || last.Content != "乙" || last.HasMore || last.NextOffset != nil {
		t.Fatalf("last chunk was not bounded correctly: result=%+v err=%v", last, err)
	}

	for _, args := range []materialReadArguments{
		{MaterialID: material.MaterialID, SHA256: material.SHA256, Offset: intPointer(1)},
		{MaterialID: material.MaterialID, SHA256: material.SHA256, Offset: intPointer(len([]byte(content)) + 1)},
		{MaterialID: material.MaterialID, SHA256: material.SHA256, MaxBytes: intPointer(0)},
		{MaterialID: material.MaterialID, SHA256: material.SHA256, MaxBytes: intPointer(frozenMaterialReadMax + 1)},
	} {
		if _, err := readFrozenMaterial(files, args); err == nil {
			t.Fatalf("accepted invalid material read range: %+v", args)
		}
	}
	if _, err := readFrozenMaterial(files, materialReadArguments{
		MaterialID: material.MaterialID, SHA256: material.SHA256, MaxBytes: intPointer(1),
	}); err == nil {
		t.Fatal("accepted a chunk too short to include its first UTF-8 character")
	}
}

func intPointer(value int) *int { return &value }

func TestReadForgeOriginalUsesFrozenOwnerOrApprovalRouteAndChecksResponse(t *testing.T) {
	ownerBytes := []byte("%PDF-1.7\nowner bytes")
	approvalBytes := []byte("%PDF-1.7\napproval bytes")
	ownerSHA, approvalSHA := dispatchInputDigestBytes(ownerBytes), dispatchInputDigestBytes(approvalBytes)
	ownerPath := "/api/v1/workbench/materials/file-owner/original"
	approvalPath := "/api/v1/approvals/requests/request-owner-1/workbench-context/files/file-approval/original"
	fixtures := map[string]struct {
		file ForgeOriginalReference
		body []byte
	}{
		ownerPath:    {file: ForgeOriginalReference{FileID: "file-owner", SourceKind: "owner", Bytes: int64(len(ownerBytes)), MediaType: "application/pdf", SHA256: ownerSHA}, body: ownerBytes},
		approvalPath: {file: ForgeOriginalReference{FileID: "file-approval", SourceKind: "approval", RequestID: "request-owner-1", Bytes: int64(len(approvalBytes)), MediaType: "application/pdf", SHA256: approvalSHA}, body: approvalBytes},
	}
	var requestedPaths []string
	badResponse := ""
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestedPaths = append(requestedPaths, request.URL.Path)
		fixture, ok := fixtures[request.URL.Path]
		if !ok || request.Header.Get("Authorization") != "Bearer frozen-employee-session" ||
			request.Header.Get("If-Match") != fixture.file.SHA256 || request.Header.Get("Accept-Encoding") != "identity" {
			http.Error(writer, "not found", http.StatusNotFound)
			return
		}
		mediaType, length, digest := fixture.file.MediaType, len(fixture.body), fixture.file.SHA256
		switch badResponse {
		case "media_type":
			mediaType = "text/plain"
		case "length":
			length++
		case "digest":
			digest = strings.Repeat("f", 64)
		}
		writer.Header().Set("Content-Type", mediaType)
		writer.Header().Set("Content-Length", strconv.Itoa(length))
		writer.Header().Set("ETag", `"`+digest+`"`)
		writer.Header().Set("X-Content-SHA256", digest)
		_, _ = writer.Write(fixture.body)
	}))
	defer server.Close()
	for path, fixture := range fixtures {
		if err := ReadVerifiedForgeOriginal(t.Context(), server.URL, []byte("frozen-employee-session"), fixture.file); err != nil {
			t.Fatalf("frozen source route %s failed validation: %v", path, err)
		}
	}
	if len(requestedPaths) != 2 || requestedPaths[0] == requestedPaths[1] {
		t.Fatalf("owner and approval reads did not remain on distinct routes: %v", requestedPaths)
	}

	for _, kind := range []string{"media_type", "length", "digest"} {
		badResponse = kind
		if err := ReadVerifiedForgeOriginal(t.Context(), server.URL, []byte("frozen-employee-session"), fixtures[ownerPath].file); err == nil {
			t.Fatalf("accepted Forge response with invalid %s", kind)
		}
	}
	wrongIfMatch := fixtures[ownerPath].file
	wrongIfMatch.SHA256 = strings.Repeat("f", 64)
	if err := ReadVerifiedForgeOriginal(t.Context(), server.URL, []byte("frozen-employee-session"), wrongIfMatch); err == nil {
		t.Fatal("accepted an original after Forge rejected its frozen If-Match SHA-256")
	}
	wrongApproval := fixtures[approvalPath].file
	wrongApproval.RequestID = "another-request"
	priorRequestCount := len(requestedPaths)
	if err := ReadVerifiedForgeOriginal(t.Context(), server.URL, []byte("frozen-employee-session"), wrongApproval); err == nil {
		t.Fatal("accepted an approval material under a different request id")
	}
	if len(requestedPaths) != priorRequestCount+1 || requestedPaths[len(requestedPaths)-1] == ownerPath {
		t.Fatalf("approval source must not fall back to the owner route: %v", requestedPaths)
	}
	missingSource := fixtures[approvalPath].file
	missingSource.RequestID = ""
	priorRequestCount = len(requestedPaths)
	if err := ReadVerifiedForgeOriginal(t.Context(), server.URL, []byte("frozen-employee-session"), missingSource); err == nil {
		t.Fatal("approval file was read without its frozen request id")
	}
	if len(requestedPaths) != priorRequestCount {
		t.Fatalf("approval source without a request id issued HTTP: %v", requestedPaths)
	}
}

func TestMaterialReaderWithoutSessionKeyDoesNotRequestForgeOriginal(t *testing.T) {
	reader := &materialReadDispatcher{store: NewStore(nil, nil, nil)}
	status, code := reader.verifyFrozenOriginal(context.Background(), frozenMaterialReadFile{
		SourceKind: "owner", FileID: "file-owner", MediaType: "application/pdf", Bytes: 9, SHA256: strings.Repeat("a", 64), Available: true,
	})
	if status != "unavailable" || code != "forge_delegation_unavailable" {
		t.Fatalf("missing encrypted employee session was misreported: status=%s code=%s", status, code)
	}
}

func TestLoomWithoutMaterialStoreExposesUnavailableReadAndNoCLIReadTool(t *testing.T) {
	ctx := context.Background()
	innerTools := &captureHost{}
	loomOpts, _, err := (Factory{}).attach(ctx,
		frozen.FrozenExecutionBundle{Agent: frozen.FrozenAgentRecord{Engine: "loom"}},
		compiler.FrozenBuildOpts{Tools: innerTools}, io.NopCloser(strings.NewReader("")), nil)
	if err != nil {
		t.Fatal(err)
	}
	loomTools, err := loomOpts.Tools.ListTools(ctx)
	if err != nil || len(loomTools) != 1 || loomTools[0].Name != frozenMaterialReadToolName {
		t.Fatalf("Loom material read fallback missing: tools=%+v err=%v", loomTools, err)
	}
	args, _ := json.Marshal(map[string]string{"materialId": "aaaaaaaaaaaaaaaaaaaaaaaa", "sha256": strings.Repeat("a", 64)})
	result, err := loomOpts.Tools.Dispatch(ctx, contract.ToolCall{ID: "read", Name: frozenMaterialReadToolName, Args: string(args)})
	if err != nil || result.IsError {
		t.Fatalf("missing material store should return a bounded unavailable result: %+v %v", result, err)
	}
	var unavailable materialReadResult
	if err := json.Unmarshal([]byte(result.Content), &unavailable); err != nil || unavailable.Status != "unavailable" ||
		unavailable.OriginalVerificationStatus != "unavailable" || unavailable.Reason != "execution_scope_unavailable" || unavailable.Content != "" {
		t.Fatalf("missing store was misreported: result=%+v err=%v", unavailable, err)
	}

	cliOpts, _, err := (Factory{}).attach(ctx,
		frozen.FrozenExecutionBundle{Agent: frozen.FrozenAgentRecord{Engine: "codex"}},
		compiler.FrozenBuildOpts{Tools: &captureHost{}}, io.NopCloser(strings.NewReader("")), nil)
	if err != nil {
		t.Fatal(err)
	}
	cliTools, err := cliOpts.Tools.ListTools(ctx)
	if err != nil || len(cliTools) != 0 {
		t.Fatalf("non-Loom runtimes must not inherit this material reader: tools=%+v err=%v", cliTools, err)
	}
}

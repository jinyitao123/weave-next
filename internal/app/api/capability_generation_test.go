package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/labstack/echo/v4"
)

type capabilityGenerationModel struct{ request contract.ChatRequest }

func (m *capabilityGenerationModel) Chat(_ context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	m.request = request
	return &contract.ChatResponse{Content: `{
		"name":"供应商报价核对","description":"核对报价并交付采购建议",
		"input_fields":[{"key":"quotation","label":"供应商报价单","description":"待核对报价","type":"string","required":true}],
		"output_fields":[{"key":"recommendation","label":"采购建议","description":"核对后的建议","type":"string","required":true}],
		"roles":[{"name":"采购专员","responsibilities":"核对品类和数量"},{"name":"财务复核员","responsibilities":"复核价格和税率"}],
		"steps":[{"name":"核对品类数量","role_index":0,"kind":"worker","instruction":"核对品类和数量并列出差异","depends_on":[],"uses_input":true,"uses_steps":[]},{"name":"复核价格税率","role_index":1,"kind":"worker","instruction":"复核价格和税率并提出建议","depends_on":[0],"uses_input":true,"uses_steps":[0]}]
	}`}, nil
}

func (m *capabilityGenerationModel) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	stream := make(chan contract.StreamChunk)
	close(stream)
	return stream, nil
}

func TestHandleGenerateCapabilityBuildsValidatedDraft(t *testing.T) {
	model := &capabilityGenerationModel{}
	router := llmrouter.New("design-model")
	router.Register("design-model", model)
	server := &Server{Models: llmrouter.NewResolver(router)}
	e := echo.New()
	body := bytes.NewBufferString(`{"prompt":"核对供应商报价单并形成采购建议","model":"design-model"}`)
	request := httptest.NewRequest("POST", "/v1/capabilities/generate", body)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	c := e.NewContext(request, response)
	c.Set("tenant", "workspace-1")

	if err := server.handleGenerateCapability(c); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if model.request.Model != "design-model" || model.request.Schema == nil || len(model.request.Tools) != 0 {
		t.Fatalf("unexpected generation request: %+v", model.request)
	}
	var result struct {
		Definition capability.Definition `json:"definition"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Definition.CapabilityID == "" || result.Definition.Name != "供应商报价核对" || len(result.Definition.Roles) != 2 || len(result.Definition.Steps) != 2 {
		t.Fatalf("unexpected generated definition: %+v", result.Definition)
	}
	published, err := capability.Publish(result.Definition, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capability.Compile(published); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeGeneratedCapabilityRejectsUnknownFields(t *testing.T) {
	var proposal generatedCapabilityProposal
	if err := decodeGeneratedCapability("```json\n{\"name\":\"x\",\"unknown\":true}\n```", &proposal); err == nil {
		t.Fatal("expected unknown field rejection")
	}
}

func TestGeneratedObjectSchemaRejectsUnsafeOrDuplicateKeys(t *testing.T) {
	for _, fields := range [][]generatedCapabilityField{
		{{Key: "bad key", Label: "字段", Type: "string"}},
		{{Key: "field", Label: "字段一", Type: "string"}, {Key: "field", Label: "字段二", Type: "string"}},
	} {
		if _, err := generatedObjectSchema(fields); err == nil {
			t.Fatalf("expected invalid fields: %+v", fields)
		}
	}
}

package businessaction

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

const forgeEmployeeBearer = "Bearer employee-delegation-token"

func TestReadActionCatalogRestoresNativeFileAndObjectOverrideMetadata(t *testing.T) {
	capabilities := []string{
		"forge:action:forge_sales_contract.contract_submit_material_package",
		"forge:action:forge_sales_lead.sales_lead_convert_to_opportunity",
		"forge:action:forge_quotation.quotation_adjust_line_price",
	}
	host := &captureHost{result: &contract.ToolResult{Content: `{"totalCount":4,"actions":[` +
		`{"name":"contract_submit_material_package","objectName":"forge_sales_contract","requiresRecord":true,"params":[{"name":"primary_file_id","type":"string","required":true},{"name":"material_file_ids","type":"string","required":true}]},` +
		`{"name":"sales_lead_convert_to_opportunity","objectName":"forge_sales_lead","requiresRecord":true,"params":[{"name":"amount","type":"string","required":false},{"name":"expected_close_on","type":"string","required":false}]},` +
		`{"name":"quotation_adjust_line_price","objectName":"forge_quotation","requiresRecord":true,"params":[{"name":"line_id","type":"string","required":true},{"name":"expected_pricing_version","type":"number","required":true},{"name":"taxed_unit_price","type":"number","required":true},{"name":"idempotency_key","type":"string","required":true}]},` +
		`{"name":"unselected_contract_action","objectName":"forge_sales_contract","requiresRecord":true,"params":[{"name":"file_id","type":"string","required":true}]}]}`}}
	metadataByPath := map[string]string{
		"/api/v1/meta/objects/forge_sales_contract": objectMetadataJSON("forge_sales_contract", []any{
			map[string]any{"name": "contract_submit_material_package", "params": []any{
				map[string]any{"name": "primary_file_id", "type": "file", "multiple": false, "required": true},
				map[string]any{"name": "material_file_ids", "type": "file", "multiple": true, "required": true},
			}},
			map[string]any{"name": "unselected_contract_action", "params": []any{
				map[string]any{"name": "file_id", "type": "file", "required": true},
			}},
		}, map[string]any{}),
		"/api/v1/meta/objects/forge_sales_lead": objectMetadataJSON("forge_sales_lead", []any{
			map[string]any{"name": "sales_lead_convert_to_opportunity", "params": []any{
				map[string]any{"field": "amount", "objectOverride": "forge_sales_opportunity", "required": false},
				map[string]any{"field": "expected_close_on", "objectOverride": "forge_sales_opportunity", "required": false},
			}},
		}, map[string]any{"status": map[string]any{"type": "select"}}),
		"/api/v1/meta/objects/forge_sales_opportunity": objectMetadataJSON("forge_sales_opportunity", []any{}, map[string]any{
			"amount":            map[string]any{"type": "currency"},
			"expected_close_on": map[string]any{"type": "date"},
		}),
		"/api/v1/meta/objects/forge_quotation": objectMetadataJSON("forge_quotation", []any{
			map[string]any{"name": "quotation_adjust_line_price", "params": []any{
				map[string]any{"name": "line_id", "type": "text", "required": true},
				map[string]any{"name": "expected_pricing_version", "type": "number", "required": true},
				map[string]any{"name": "taxed_unit_price", "type": "number", "required": true},
				map[string]any{"name": "idempotency_key", "type": "text", "required": true},
			}},
		}, map[string]any{}),
	}
	seenPaths := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != forgeEmployeeBearer || r.Header.Get("Accept") != "application/json" {
			http.Error(w, "wrong metadata request context", http.StatusUnauthorized)
			return
		}
		body, ok := metadataByPath[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		seenPaths[r.URL.Path]++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	reader := forgeObjectMetadataReader{baseURL: *baseURL, authorization: []byte(forgeEmployeeBearer), client: server.Client()}
	catalog, err := readActionCatalog(context.Background(), host, capabilities, reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog) != len(capabilities) {
		t.Fatalf("catalog contains unrequested metadata: %+v", catalog)
	}
	contractAction := catalog["forge_sales_contract.contract_submit_material_package"]
	if len(contractAction.Params) != 2 || contractAction.Params[0].Type != "file" || contractAction.Params[0].Multiple ||
		contractAction.Params[1].Type != "file" || !contractAction.Params[1].Multiple {
		t.Fatalf("native file type/cardinality was not restored from the object declaration: %+v", contractAction.Params)
	}
	leadAction := catalog["forge_sales_lead.sales_lead_convert_to_opportunity"]
	if len(leadAction.Params) != 2 || leadAction.Params[0].Type != "number" || leadAction.Params[1].Type != "string" {
		t.Fatalf("objectOverride field types were not resolved without string fallback: %+v", leadAction.Params)
	}
	quoteAction := catalog["forge_quotation.quotation_adjust_line_price"]
	if len(quoteAction.Params) != 4 || quoteAction.Params[0].Type != "string" || quoteAction.Params[1].Type != "number" ||
		quoteAction.Params[2].Type != "number" || quoteAction.Params[3].Type != "string" {
		t.Fatalf("native quote scalar types regressed: %+v", quoteAction.Params)
	}
	if _, ok := catalog["forge_sales_contract.unselected_contract_action"]; ok {
		t.Fatal("declared metadata widened the employee-visible action allowlist")
	}
	for path := range metadataByPath {
		if seenPaths[path] != 1 {
			t.Fatalf("metadata path %q was read %d times; want one read", path, seenPaths[path])
		}
	}
	if host.call.Name != "list_actions" || host.call.Args != `{}` {
		t.Fatalf("catalog source did not first use native list_actions: %+v", host.call)
	}
}

func TestReadActionCatalogFailsClosedWhenMetadataIsDeniedOrFieldMasked(t *testing.T) {
	capability := "forge:action:forge_sales_lead.sales_lead_convert_to_opportunity"
	host := &captureHost{result: &contract.ToolResult{Content: `{"actions":[{"name":"sales_lead_convert_to_opportunity","objectName":"forge_sales_lead","requiresRecord":true,"params":[{"name":"amount","type":"string","required":false}]}]}`}}

	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "metadata authentication required", status: http.StatusUnauthorized, body: `{"error":"auth required"}`},
		{name: "metadata denied", status: http.StatusForbidden, body: `{"error":"forbidden"}`},
		{name: "override field masked", status: http.StatusOK, body: objectMetadataJSON("forge_sales_lead", []any{
			map[string]any{"name": "sales_lead_convert_to_opportunity", "params": []any{
				map[string]any{"field": "amount", "objectOverride": "forge_sales_opportunity", "required": false},
			}},
		}, map[string]any{})},
	} {
		t.Run(test.name, func(t *testing.T) {
			seen := map[string]bool{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != forgeEmployeeBearer {
					http.Error(w, "wrong employee context", http.StatusUnauthorized)
					return
				}
				seen[r.URL.Path] = true
				if test.name == "override field masked" && r.URL.Path == "/api/v1/meta/objects/forge_sales_opportunity" {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(objectMetadataJSON("forge_sales_opportunity", []any{}, map[string]any{})))
					return
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			baseURL, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			reader := forgeObjectMetadataReader{baseURL: *baseURL, authorization: []byte(forgeEmployeeBearer), client: server.Client()}
			_, err = readActionCatalog(context.Background(), host, []string{capability}, reader)
			if err == nil {
				t.Fatal("catalog degraded missing/denied declared metadata to MCP string parameters")
			}
			if len(seen) == 0 {
				t.Fatal("native declared metadata route was not consulted")
			}
		})
	}
}

func objectMetadataJSON(objectName string, actions []any, fields map[string]any) string {
	payload, _ := json.Marshal(map[string]any{
		"type":        objectNameForWrapper,
		"name":        objectName,
		"sortability": map[string]any{},
		"item":        map[string]any{"name": objectName, "actions": actions, "fields": fields},
	})
	return string(payload)
}

const objectNameForWrapper = "object"

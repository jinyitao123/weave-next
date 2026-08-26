package teamtemplates

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jinyitao123/weave/internal/build/teamtemplate"
)

func TestStaticCatalogSamplesMatchRepositoryAndCompile(t *testing.T) {
	catalog := NewStaticCatalog()
	items := catalog.List()
	wantNames := []string{"code-review", "content-production", "human-final-review", "market-research"}
	gotNames := make([]string, 0, len(items))
	for _, item := range items {
		gotNames = append(gotNames, item.Name)
		repositoryYAML, err := os.ReadFile(filepath.Join("..", "..", "..", "templates", item.Name+".yaml"))
		if err != nil {
			t.Fatalf("read root sample %q: %v", item.Name, err)
		}
		if item.YAML != string(repositoryYAML) {
			t.Fatalf("generated sample %q is out of sync with templates/", item.Name)
		}
		compiled, err := teamtemplate.CompileYAML([]byte(item.YAML))
		if err != nil {
			t.Fatalf("compile sample %q: %v", item.Name, err)
		}
		if compiled.Template.Name != item.Name || compiled.Template.DisplayName != item.DisplayName {
			t.Fatalf("sample metadata = %#v, template = %#v", item, compiled.Template)
		}
		if item.Name == "human-final-review" && item.DeclarativeSpec == nil {
			t.Fatal("human-final-review sample must include its declarative workflow")
		}
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("sample names = %#v, want %#v", gotNames, wantNames)
	}
}

func TestStaticCatalogAppliesOverridesThroughStrictCompiler(t *testing.T) {
	catalog := NewStaticCatalog()
	yamlBytes, err := catalog.Resolve("market-research", map[string]any{
		"display_name": "消费市场调研团队",
		"budget":       map[string]any{"max_cost_usd": 4.0},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	compiled, err := teamtemplate.CompileYAML(yamlBytes)
	if err != nil {
		t.Fatalf("CompileYAML() error = %v", err)
	}
	if compiled.Template.DisplayName != "消费市场调研团队" || compiled.Brief.TotalBudget.MaxCostUSD != 4 {
		t.Fatalf("compiled override = %#v", compiled.Template)
	}
}

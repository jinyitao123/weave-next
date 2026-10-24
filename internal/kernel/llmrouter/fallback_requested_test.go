package llmrouter

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

// orderLLM records the model of every attempt and fails the listed models.
type orderLLM struct {
	tried  []string
	failed map[string]bool
}

func (o *orderLLM) Chat(_ context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	o.tried = append(o.tried, req.Model)
	if o.failed[req.Model] {
		return nil, errors.New("HTTP 400 unsupported request")
	}
	return &contract.ChatResponse{Content: req.Model}, nil
}

func (o *orderLLM) Stream(_ context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	o.tried = append(o.tried, req.Model)
	if o.failed[req.Model] {
		return nil, errors.New("HTTP 400 unsupported request")
	}
	return make(chan contract.StreamChunk), nil
}

// A declarative step that names a bundled model gets that model; the others
// are only its fallbacks (#115).
func TestFallbackStartsWithTheModelTheRequestNames(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, c := range []struct {
			requested string
			failed    []string
			want      []string
		}{
			{"cheap", nil, []string{"cheap"}},
			{"cheap", []string{"cheap"}, []string{"cheap", "primary"}},
			{"spare", []string{"spare", "primary"}, []string{"spare", "primary", "cheap"}},
			{"primary", nil, []string{"primary"}},
			{"", []string{"primary"}, []string{"primary", "cheap"}},
			{"elsewhere", nil, []string{"primary"}},
		} {
			inner := &orderLLM{failed: map[string]bool{}}
			for _, model := range c.failed {
				inner.failed[model] = true
			}
			llm := NewFallbackLLM(inner, "primary", []string{"cheap", "spare"}, 1)
			var err error
			if stream {
				_, err = llm.Stream(t.Context(), contract.ChatRequest{Model: c.requested})
			} else {
				_, err = llm.Chat(t.Context(), contract.ChatRequest{Model: c.requested})
			}
			if err != nil || !reflect.DeepEqual(inner.tried, c.want) {
				t.Fatalf("stream=%v requested %q: tried %v, err %v; want %v", stream, c.requested, inner.tried, err, c.want)
			}
		}
	}
}

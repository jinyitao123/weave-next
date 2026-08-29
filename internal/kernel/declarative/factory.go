// Package declarative provides a graph factory that builds Loom graphs
// from JSON-based GraphDefinition, without requiring Go code.
package declarative

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/streamctx"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/memory"
	"github.com/jinyitao123/weave/internal/kernel/otel"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// Register registers the declarative graph factory with the compiler.
func Register() {
	compiler.RegisterGraphFactory("declarative", buildGraph)
}

func buildGraph(tenant string, rec *registry.AgentRecord, llm contract.LLM, tools contract.ToolDispatcher, opts compiler.CompileOpts) (*loom.Graph, error) {
	def := rec.GraphDefinition
	if def == nil || len(def.Steps) == 0 {
		// Fallback to a simple chat step if no definition.
		g := loom.NewGraph(
			tenant+":"+rec.Name,
			"chat",
			loom.WithStepBudget(100),
			loom.WithCheckpointHistory(50),
		)
		g.AddStep("chat", stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{Model: rec.Model, MaxIterations: 20}), loom.End())
		installGraphHooks(g, tenant, rec, opts)
		return g, nil
	}

	g := loom.NewGraph(
		tenant+":"+rec.Name,
		def.Entry,
		loom.WithStepBudget(100),
		loom.WithCheckpointHistory(50),
	)

	var topo []loom.StepInfo
	for _, sd := range def.Steps {
		step := buildStep(sd, llm, tools, rec, opts)
		router := buildRouter(sd)
		g.AddStep(sd.Name, wrapWithSSE(step, sd.Display), router)
		topo = append(topo, buildStepInfo(sd))
	}
	g.SetTopology(topo)

	installGraphHooks(g, tenant, rec, opts)
	return g, nil
}

// installGraphHooks keeps both declarative construction branches on the same
// caller-hook and observability contract.
func installGraphHooks(
	graph *loom.Graph,
	tenant string,
	rec *registry.AgentRecord,
	opts compiler.CompileOpts,
) {
	before := append([]loom.StepHook(nil), opts.BeforeStepHooks...)
	after := make([]loom.StepHook, 0, len(opts.AfterStepHooks)+2)
	if opts.Store != nil {
		before = append(before, otel.TraceStart(tenant))
		after = append(after, otel.TraceEnd(opts.Store))
		after = append(after, otel.AuditHook(opts.Store, tenant, rec.Name))
	}
	after = append(after, opts.AfterStepHooks...)
	if len(before) > 0 || len(after) > 0 {
		graph.SetHooks(loom.HookPoints{Before: before, After: after})
	}
}

// buildStep creates a Loom Step from a StepDefinition.
func buildStep(sd registry.StepDefinition, llm contract.LLM, tools contract.ToolDispatcher, rec *registry.AgentRecord, opts compiler.CompileOpts) loom.Step {
	switch sd.Type {
	case "chat":
		return buildChatStep(sd.Config, llm, tools, rec)
	case "llm_call":
		return buildLLMCallStep(sd.Config, llm)
	case "llm_check":
		return buildLLMCheckStep(sd.Config, llm)
	case "yield":
		return buildYieldStep(sd.Config)
	case "transform":
		return buildTransformStep(sd.Config)
	case "worker":
		return buildWorkerStep(sd.Config, opts.AgentRunner)
	case "builtin":
		return buildBuiltinStep(sd.Config, llm, tools, rec, opts)
	default:
		return func(_ context.Context, s loom.State) (loom.State, error) {
			return s, fmt.Errorf("unknown step type: %s", sd.Type)
		}
	}
}

// ── Worker step ──

func buildWorkerStep(config map[string]any, runner compiler.AgentRunner) loom.Step {
	worker := getStr(config, "worker", "")
	tmpl := getStr(config, "prompt_template", "")
	inputKeys := getStrSlice(config, "input_keys")
	outputKey := getStr(config, "output_key", "worker_output")

	return func(ctx context.Context, s loom.State) (loom.State, error) {
		if runner == nil {
			return s, fmt.Errorf("declarative worker step requires AgentRunner dependency")
		}
		prompt := fillTemplate(tmpl, s, inputKeys)
		result, err := runner.Run(ctx, worker, prompt)
		if err != nil {
			return s, fmt.Errorf("worker %q: %w", worker, err)
		}
		if result.Yielded {
			return s, fmt.Errorf("worker %q yielded with type %q", worker, result.YieldType)
		}
		s[outputKey] = result.Output
		return s, nil
	}
}

// ── Chat step ──

func buildChatStep(config map[string]any, llm contract.LLM, tools contract.ToolDispatcher, rec *registry.AgentRecord) loom.Step {
	model := getStr(config, "model", rec.Model)
	sysPrompt := getStr(config, "system_prompt", "")
	maxIter := getInt(config, "max_iterations", 20)
	var outputSchema *json.RawMessage
	if rec.OutputSchema != nil {
		schema := append(json.RawMessage(nil), (*rec.OutputSchema)...)
		outputSchema = &schema
	}

	return stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		Model:         model,
		SystemPrompt:  sysPrompt,
		MaxIterations: maxIter,
		MaxTokens:     rec.MaxOutputTokens,
		OutputSchema:  outputSchema,
	})
}

// ── LLM Call step ──

func buildLLMCallStep(config map[string]any, llm contract.LLM) loom.Step {
	model := getStr(config, "model", "deepseek-v4-flash")
	tmpl := getStr(config, "prompt_template", "")
	inputKeys := getStrSlice(config, "input_keys")
	outputKey := getStr(config, "output_key", "output")
	stream := getBool(config, "stream", true)

	// Optional extract for condition routing.
	var extractKey, extractMode string
	var keywordsTrue []string
	if ext, ok := config["extract"].(map[string]any); ok {
		extractKey = getStr(ext, "key", "")
		extractMode = getStr(ext, "mode", "keyword")
		keywordsTrue = getStrSlice(ext, "keywords_true")
	}

	return func(ctx context.Context, s loom.State) (loom.State, error) {
		prompt := fillTemplate(tmpl, s, inputKeys)

		resp, err := llm.Chat(ctx, contract.ChatRequest{
			Model:    model,
			Messages: []contract.Message{{Role: "user", Content: prompt}},
		})
		if err != nil {
			return s, fmt.Errorf("llm_call: %w", err)
		}

		s[outputKey] = resp.Content

		// Stream to client if SSE available.
		if stream {
			if send, ok := s["__sse_send"].(func(string, any)); ok {
				send("chunk", map[string]string{"content": resp.Content})
			}
		}

		// Extract judgment if configured.
		if extractKey != "" {
			s[extractKey] = extractBool(resp.Content, extractMode, keywordsTrue)
		}

		return s, nil
	}
}

// ── LLM Check step ──

func buildLLMCheckStep(config map[string]any, llm contract.LLM) loom.Step {
	model := getStr(config, "model", "deepseek-v4-flash")
	tmpl := getStr(config, "prompt_template", "")
	inputKeys := getStrSlice(config, "input_keys")
	outputKey := getStr(config, "output_key", "check_result")
	mode := getStr(config, "extract_mode", "json")
	jsonField := getStr(config, "json_field", "result")

	return func(ctx context.Context, s loom.State) (loom.State, error) {
		// Suppress streaming for check steps.
		ctx = streamctx.SuppressStream(ctx)

		prompt := fillTemplate(tmpl, s, inputKeys)
		resp, err := llm.Chat(ctx, contract.ChatRequest{
			Model:    model,
			Messages: []contract.Message{{Role: "user", Content: prompt}},
		})
		if err != nil {
			return s, fmt.Errorf("llm_check: %w", err)
		}

		switch mode {
		case "json":
			s[outputKey] = extractJSONBool(resp.Content, jsonField)
		case "keyword":
			keywords := getStrSlice(config, "keywords_true")
			s[outputKey] = extractBool(resp.Content, "keyword", keywords)
		default:
			s[outputKey] = strings.Contains(strings.ToUpper(resp.Content), "YES")
		}

		return s, nil
	}
}

// ── Yield step ──

func buildYieldStep(config map[string]any) loom.Step {
	yieldType := getStr(config, "yield_type", "await_input")
	return func(_ context.Context, s loom.State) (loom.State, error) {
		s["__yield"] = true
		// "after_step": Resume must route past this step, not re-run it
		// (re-running would yield again and the run could never continue).
		s["__yield_phase"] = "after_step"
		s["yield_type"] = yieldType
		return s, nil
	}
}

// ── Transform step ──

func buildTransformStep(config map[string]any) loom.Step {
	return func(_ context.Context, s loom.State) (loom.State, error) {
		ops, _ := config["operations"].([]any)
		for _, opRaw := range ops {
			op, _ := opRaw.(map[string]any)
			switch getStr(op, "op", "") {
			case "set":
				s[getStr(op, "target", "")] = op["value"]
			case "concat":
				keys := getStrSlice(op, "keys")
				sep := getStr(op, "separator", "\n")
				var parts []string
				for _, k := range keys {
					if v, ok := s[k].(string); ok {
						parts = append(parts, v)
					}
				}
				s[getStr(op, "target", "")] = strings.Join(parts, sep)
			case "copy":
				s[getStr(op, "target", "")] = s[getStr(op, "source", "")]
			}
		}
		return s, nil
	}
}

// ── Builtin step ──

func buildBuiltinStep(config map[string]any, llm contract.LLM, tools contract.ToolDispatcher, rec *registry.AgentRecord, opts compiler.CompileOpts) loom.Step {
	builtinType := getStr(config, "builtin_type", "")
	switch builtinType {
	case "guard":
		if rec.Guard != nil && rec.Guard.Enabled {
			return compiler.BuildGuardStep(rec.Guard)
		}
		return func(_ context.Context, s loom.State) (loom.State, error) { return s, nil }
	case "prompt_assemble":
		// Shared live prompt semantics (SystemPrompt → Identity.Core fallback,
		// grounding, always-active/builtin direct injection, matchable subset,
		// and matcher selection). Skills are already resolved by CompileAgent
		// before custom factory dispatch, so rec.Spec.Skills is authoritative
		// and this step never performs its own store lookup.
		prepared := compiler.PreparePrompt(rec, opts.Embedder)
		return stdlib.NewPromptAssembleStep(stdlib.PromptConfig{
			Identity:              prepared.Identity,
			Skills:                prepared.MatchableSkills,
			ProfilesMap:           rec.Spec.Profiles,
			MaxSystemPromptTokens: 8000,
			SkillMatcher:          prepared.Matcher,
		})
	case "memory_retrieve":
		if opts.MemoryService != nil {
			topK := 5
			if rec.MemoryConfig != nil && rec.MemoryConfig.TopK > 0 {
				topK = rec.MemoryConfig.TopK
			}
			return memory.NewRetrieveStep(memory.RetrieveConfig{
				Service: opts.MemoryService,
				TopK:    topK,
			})
		}
		return func(_ context.Context, s loom.State) (loom.State, error) { return s, nil }
	default:
		return func(_ context.Context, s loom.State) (loom.State, error) {
			return s, fmt.Errorf("unknown builtin type: %s", builtinType)
		}
	}
}

// ── Router ──

func buildRouter(sd registry.StepDefinition) loom.Router {
	if sd.Condition != nil {
		trueTarget := ""
		if sd.Condition.TrueStep != nil {
			trueTarget = *sd.Condition.TrueStep
		}
		falseTarget := ""
		if sd.Condition.FalseStep != nil {
			falseTarget = *sd.Condition.FalseStep
		}
		maxLoops := getInt(sd.Config, "max_loops", 5)
		counterKey := "__loop_count_" + sd.Name
		return func(_ context.Context, s loom.State) (string, error) {
			// Track loop count for this condition step.
			count, _ := s[counterKey].(int)
			count++
			s[counterKey] = count

			v, _ := s[sd.Condition.Key].(bool)
			if v {
				return trueTarget, nil
			}
			// The max_loops budget is a hard refusal, never a forgery: a
			// condition that never became true must fail loudly instead of
			// being coerced onto the true branch (T21 — the old force-true
			// behavior turned a failed evidence check into a fabricated
			// success after enough retries).
			if count >= maxLoops {
				return "", fmt.Errorf(
					"declarative condition %q exceeded max_loops=%d with %q still false; refusing to force the true branch",
					sd.Name, maxLoops, sd.Condition.Key,
				)
			}
			return falseTarget, nil
		}
	}
	if sd.Next == nil || *sd.Next == "" {
		return loom.End()
	}
	return loom.Always(*sd.Next)
}

// ── Topology info ──

func buildStepInfo(sd registry.StepDefinition) loom.StepInfo {
	info := loom.StepInfo{Name: sd.Name, Detail: sd.Display}
	if sd.Condition != nil {
		trueT, falseT := "", ""
		if sd.Condition.TrueStep != nil {
			trueT = *sd.Condition.TrueStep
		}
		if sd.Condition.FalseStep != nil {
			falseT = *sd.Condition.FalseStep
		}
		info.Edges = []loom.Edge{{To: trueT, Label: "true"}, {To: falseT, Label: "false"}}
	} else if sd.Next != nil && *sd.Next != "" {
		info.Edges = []loom.Edge{{To: *sd.Next}}
	} else {
		info.Edges = []loom.Edge{{To: ""}}
	}
	return info
}

// ── SSE wrapper ──

func wrapWithSSE(inner loom.Step, display string) loom.Step {
	return func(ctx context.Context, s loom.State) (loom.State, error) {
		if sse := streamctx.EventSenderFromContext(ctx); sse != nil {
			_ = sse.SendEvent("step_start", map[string]string{"step": display})
			s["__sse_send"] = func(eventType string, data any) {
				_ = sse.SendEvent(eventType, data)
			}
		}
		result, err := inner(ctx, s)
		delete(s, "__sse_send")
		if result != nil {
			delete(result, "__sse_send")
		}
		return result, err
	}
}

// ── Helpers ──

func fillTemplate(tmpl string, s loom.State, keys []string) string {
	result := tmpl
	for _, k := range keys {
		val := ""
		if v, ok := s[k].(string); ok {
			val = v
		} else if v, ok := s[k]; ok {
			b, _ := json.Marshal(v)
			val = string(b)
		}
		result = strings.ReplaceAll(result, "{{"+k+"}}", val)
	}
	return result
}

func extractBool(text, mode string, keywords []string) bool {
	lower := strings.ToLower(text)
	switch mode {
	case "keyword":
		for _, kw := range keywords {
			if strings.Contains(lower, strings.ToLower(kw)) {
				return true
			}
		}
		return false
	default:
		return strings.Contains(strings.ToUpper(text), "YES") || strings.Contains(text, "true")
	}
}

func extractJSONBool(text, field string) bool {
	// Try to find JSON in the text.
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start >= 0 && end > start {
		var obj map[string]any
		if err := json.Unmarshal([]byte(text[start:end+1]), &obj); err == nil {
			if v, ok := obj[field].(bool); ok {
				return v
			}
		}
	}
	return false
}

func getStr(m map[string]any, key, fallback string) string {
	if v, ok := m[key].(string); ok && v != "" {
		return v
	}
	return fallback
}

func getInt(m map[string]any, key string, fallback int) int {
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	return fallback
}

func getBool(m map[string]any, key string, fallback bool) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return fallback
}

func getStrSlice(m map[string]any, key string) []string {
	if v, ok := m[key].([]any); ok {
		var result []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				result = append(result, s)
			}
		}
		return result
	}
	return nil
}

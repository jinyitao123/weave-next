package grounding

import "strings"

// PlatformRule is the anti-fabrication grounding rule appended to EVERY agent
// identity, regardless of how its prompt is assembled: the standard compiler,
// declarative/custom graph factories, and the CLI-engine materialization path
// (opencode/codex/claude) all ground through this one constant. It pins claims
// about system-side effects (submissions, dispatches, writes)
// to a real tool result from the current run, closing the "claimed it was
// queued but never called the tool" fabrication class.
//
// The platform speaks one language for its injected scaffolding; that decision
// is Chinese (the product's language), shared with the owner-memory honesty
// notices so a single assembled context never mixes languages.
const PlatformRule = "凡陈述系统侧的动作状态——如已提交、已派发、已写入——必须以本轮真实工具调用的返回为据；" +
	"没有对应的工具调用就不得声称已完成，如实说明尚未执行。"

// GroundIdentity appends PlatformRule to an agent identity core. It is the one
// place the rule is attached, so standard/declarative/CLI paths cannot drift.
// An empty core yields the rule alone (no leading blank lines).
func GroundIdentity(core string) string {
	core = strings.TrimRight(core, " \t\r\n")
	if core == "" {
		return PlatformRule
	}
	return core + "\n\n" + PlatformRule
}

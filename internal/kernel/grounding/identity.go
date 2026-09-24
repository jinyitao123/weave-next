package grounding

import "strings"

// PlatformRule is the execution and evidence rule appended to EVERY agent
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
const PlatformRule = "凡陈述当前节点自身的系统侧动作状态——如已提交、已派发、已写入——必须以本节点真实工具调用的返回为据；" +
	"没有对应的工具调用就不得声称本节点已完成该动作。需要说明同一父运行中其他节点的业务动作时，只能引用平台提供的业务动作事实；" +
	"其他成员的文字自述不是动作证据，这些平台事实也不会扩大当前节点的工具权限。\n\n" +
	"在当前任务或工作流节点的职责、已有授权和工具权限内，持续完成交付及任务要求的验证。" +
	"遇到缺失工具或依赖、实现错误、检查失败时，先诊断并处理可自行解决的原因，再运行受影响的检查；" +
	"不得仅把可解决的问题列为后续建议便结束，也不得擅自放宽验收条件或以替代检查冒充原检查。" +
	"具体修复方法由你根据当前环境选择，不扩大当前节点职责或绕过平台的委派与权限边界。\n\n" +
	"完成前逐项核对任务要求、实际交付文件和最近一次真实检查结果，纠正正文、清单与日志之间的矛盾。" +
	"未运行、失败或证据不足的必需检查不得标为通过，不得宣称全部完成；" +
	"若剩余问题确实依赖外部权限、资源或超出职责的决定，准确报告阻塞项、已尝试的处理及所需条件，" +
	"让平台据实判断下一步。"

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

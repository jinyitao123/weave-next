/** Display names reported by runtime engine capability records. */
export function engineLabel(engine?: string): string {
  switch ((engine || "").toLowerCase()) {
    case "":
    case "loom":
      return "内置";
    case "claude":
      return "Claude Code";
    case "codex":
      return "Codex";
    case "opencode":
      return "OpenCode";
    default:
      return "外部引擎";
  }
}

export type ToolAction = "read" | "execute" | "write" | "generic";

export function toolAction(name: string): ToolAction {
  if (/^(?:tf_)?(?:get|list)_/i.test(name)) return "read";
  if (/(?:^|_)(?:create|write|save|update|patch|put|apply|delete|remove|publish|submit)(?:_|$)/i.test(name)) return "write";
  if (/(?:^|_)(?:list|get|read|search|query|fetch|inspect|find|lookup|load)(?:_|$)/i.test(name)) return "read";
  if (/(?:^|_)(?:exec|execute|run|command|shell|bash|test|build)(?:_|$)/i.test(name)) return "execute";
  return "generic";
}

const TOOL_NAME_LABELS: Record<string, string> = {
  delegate: "委派成员",
  dispatch_parallel: "并行分派",
};

/**
 * 工具名 → 用户可读称呼。已知的内部编排工具映射为动作描述；
 * 未知名称按动作类别回退为通用说法，原始工具名不出现在界面上。
 */
export function toolDisplayName(name: string): string {
  const known = TOOL_NAME_LABELS[name];
  if (known) return known;
  switch (toolAction(name)) {
    case "read":
      return "读取信息";
    case "write":
      return "写入更新";
    case "execute":
      return "运行命令";
    default:
      return "调用工具";
  }
}

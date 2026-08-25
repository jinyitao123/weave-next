export type ToolAction = "read" | "execute" | "write" | "generic";

export function toolAction(name: string): ToolAction {
  if (/^(?:tf_)?(?:get|list)_/i.test(name)) return "read";
  if (/(?:^|_)(?:create|write|save|update|patch|put|apply|delete|remove|publish|submit)(?:_|$)/i.test(name)) return "write";
  if (/(?:^|_)(?:list|get|read|search|query|fetch|inspect|find|lookup|load)(?:_|$)/i.test(name)) return "read";
  if (/(?:^|_)(?:exec|execute|run|command|shell|bash|test|build)(?:_|$)/i.test(name)) return "execute";
  return "generic";
}

import type { ExecutionSegment } from "../../api";

const blueprintWaitingPlaceholders = new Set([
  "团队方案已生成，等待继续。回复“可以”继续执行。",
  "团队方案已生成，等待继续。回复\"可以\"继续执行。",
  "团队方案已生成。请在下方卡片中点击「继续构建」。",
  "团队方案已生成。请点击方案卡片上的「继续构建」开始；如需调整，直接说明修改意见。",
  "团队方案已生成。请在下方卡片中点击「继续构建」。",
]);

export function extractedAnswerSegmentID(segments: ExecutionSegment[]): string {
  for (let index = segments.length - 1; index >= 0; index--) {
    const segment = segments[index];
    if (segment.type === "text" && segment.content.trim()) return segment.id;
  }
  return "";
}

export function finalAnswerFromExecutionSegments(content: string, segments: ExecutionSegment[], preferTraceAnswer: boolean, hasStructuredTerminalOutcome = false): string {
  if (hasStructuredTerminalOutcome) return "";
  const segmentID = extractedAnswerSegmentID(segments);
  if (!segmentID) return "";
  const segment = segments.find((item) => item.id === segmentID);
  if (!segment || segment.type !== "text") return "";
  const answer = segment.content.trim();
  if (!answer) return "";
  const visibleContent = content.trim();
  if (preferTraceAnswer || !visibleContent) return answer;
  if (visibleContent === answer) return "";
  if (isBlueprintWaitingPlaceholder(visibleContent)) return answer;
  return "";
}

export function isBlueprintWaitingPlaceholder(content: string): boolean {
  return blueprintWaitingPlaceholders.has(content.trim());
}

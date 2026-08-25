import type { PropsWithChildren } from "react";

interface BadgeProps extends PropsWithChildren {
  tone?: "neutral" | "accent" | "success" | "warning" | "danger";
}

/**
 * 统一状态徽标原语，替代既有 .status-chip / .stage-status / .run-resume header span /
 * .agent-fact-status 四套手写实现。染色使用 color-mix 14% 色调模式，文字为对应语义色，
 * 保证双主题可读。
 */
export function Badge({ tone = "neutral", children }: BadgeProps) {
  return <span className={`ui-badge ui-badge--${tone}`}>{children}</span>;
}

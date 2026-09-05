import type { PropsWithChildren } from "react";

interface BadgeProps extends PropsWithChildren {
  tone?: "neutral" | "accent" | "success" | "warning" | "danger";
}

/** Runtime status tone; colors come from the shared semantic tokens. */
export function Badge({ tone = "neutral", children }: BadgeProps) {
  return <span className={`ui-badge ui-badge--${tone}`}>{children}</span>;
}

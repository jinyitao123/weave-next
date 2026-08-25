import type { PropsWithChildren, ReactNode } from "react";

interface CardProps extends PropsWithChildren {
  padding?: "regular" | "compact";
  /** 可交互卡片：hover 抬升 + 焦点环。仅在整个卡片可点击时使用，且内部不再放交互控件。 */
  interactive?: boolean;
  header?: ReactNode;
  footer?: ReactNode;
  /** 页面级定制类名（间距、栅格等布局用途），样式细节仍走原语。 */
  className?: string;
}

/**
 * 统一卡片原语，替代既有 .stage-card / .workflow-launcher / .run-input-card /
 * .team-detail-card / .mcp-detail-card 五套手写容器。
 * 规则：卡片不嵌卡片；卡片内的分区用分隔线（.ui-card__section）而非再套一层卡片。
 */
export function Card({ padding = "regular", interactive = false, header, footer, className, children }: CardProps) {
  const classNames = [`ui-card${padding === "compact" ? " ui-card--compact" : ""}${interactive ? " ui-card--interactive" : ""}`, className].filter(Boolean).join(" ");
  return (
    <section className={classNames} tabIndex={interactive ? 0 : undefined}>
      {header && <header className="ui-card__header">{header}</header>}
      <div className="ui-card__body">{children}</div>
      {footer && <footer className="ui-card__footer">{footer}</footer>}
    </section>
  );
}

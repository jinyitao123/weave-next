import { LoaderCircle } from "lucide-react";
import { forwardRef } from "react";
import type { ButtonHTMLAttributes, PropsWithChildren } from "react";

interface ButtonProps extends PropsWithChildren, Omit<ButtonHTMLAttributes<HTMLButtonElement>, "className"> {
  variant?: "primary" | "secondary" | "danger" | "ghost" | "ghost-danger";
  size?: "regular" | "small";
  loading?: boolean;
}

/**
 * 统一按钮原语。六态（normal/hover/pressed/focus-visible/active/disabled）由
 * .ui-button 样式固化；实色变体前景一律 var(--on-accent)，compact 断点内触控目标 ≥44px。
 * loading 时禁用并显示旋转指示，替代手写 disabled + spinner 组合。
 */
export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button({ variant = "secondary", size = "regular", loading = false, disabled, children, type = "button", ...rest }, ref) {
  const className = `ui-button ui-button--${variant}${size === "small" ? " ui-button--small" : ""}${loading ? " ui-button--loading" : ""}`;
  return (
    <button ref={ref} type={type} className={className} disabled={disabled || loading} aria-busy={loading || undefined} {...rest}>
      {loading && <LoaderCircle className="ui-button__spinner" size={16} aria-hidden="true" />}
      {children}
    </button>
  );
});

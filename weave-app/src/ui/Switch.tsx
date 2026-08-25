import type { ButtonHTMLAttributes, ReactNode } from "react";

interface SwitchProps extends Omit<ButtonHTMLAttributes<HTMLButtonElement>, "onChange" | "type" | "role" | "value"> {
  checked: boolean;
  onChange: (checked: boolean) => void;
  /** 轨道后的可选可视文案；多数场景由外层 label 提供可见文本与点击关联。 */
  label?: ReactNode;
}

/**
 * 统一布尔开关原语。button role="switch" + aria-checked 语义，Tab 聚焦与
 * Space/Enter 切换走原生 button 行为；样式由 .ui-switch 固化（开启态轨道
 * var(--accent)、关闭态 var(--separator-strong)、focus-visible 用 var(--focus)），
 * compact 断点内用 ::before 把可点区撑到 ≥44px。
 */
export function Switch({ checked, onChange, disabled, label, className, ...rest }: SwitchProps) {
  const classNames = ["ui-switch", checked ? "ui-switch--on" : "", className].filter(Boolean).join(" ");
  return (
    <button type="button" role="switch" aria-checked={checked} className={classNames} disabled={disabled} onClick={() => onChange(!checked)} {...rest}>
      <span className="ui-switch__track" aria-hidden="true"><span className="ui-switch__thumb" /></span>
      {label != null && <span className="ui-switch__label">{label}</span>}
    </button>
  );
}

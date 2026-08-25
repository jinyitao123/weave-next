import { useId, type ReactNode } from "react";

interface FieldControlProps {
  id: string;
  "aria-describedby"?: string;
  "aria-invalid"?: true;
}

interface FieldProps {
  label: string;
  htmlFor?: string;
  help?: ReactNode;
  error?: ReactNode;
  /**
   * 推荐 render-prop 用法：拿到应挂在控件上的 id / aria-describedby / aria-invalid。
   * 传普通节点时按原样渲染（仅获得 label 关联之外的布局，aria 关联需自行处理）。
   */
  children: ReactNode | ((control: FieldControlProps) => ReactNode);
}

/**
 * 统一表单字段原语：label 与控件关联，错误/帮助文本通过 aria-describedby 指回控件，
 * 错误时自动置 aria-invalid。替代各页面手写 id 拼接与无关联的 role="alert"。
 */
export function Field({ label, htmlFor, help, error, children }: FieldProps) {
  const autoId = useId();
  const controlId = htmlFor ?? autoId;
  const helpId = useId();
  const errorId = useId();
  const describedBy = [error ? errorId : null, help && !error ? helpId : null].filter(Boolean).join(" ") || undefined;

  return (
    <div className={`ui-field${error ? " ui-field--error" : ""}`}>
      <label className="ui-field__label" htmlFor={controlId}>{label}</label>
      {typeof children === "function"
        ? children({ id: controlId, "aria-describedby": describedBy, "aria-invalid": error ? true : undefined })
        : children}
      {error && <p className="ui-field__error" id={errorId} role="alert">{error}</p>}
      {help && !error && <p className="ui-field__help" id={helpId}>{help}</p>}
    </div>
  );
}

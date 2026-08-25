import { AlertCircle, LoaderCircle, RotateCw } from "lucide-react";
import { Button } from "./Button";

export function LoadingView({ label = "正在读取工作区" }: { label?: string }) {
  return (
    <div className="state-view" role="status">
      <LoaderCircle className="spin" size={24} />
      <p>{label}</p>
    </div>
  );
}

export function ErrorNotice({ message, onRetry }: { message: string; onRetry?: () => void }) {
  return (
    <div className="notice notice--error" role="alert">
      <AlertCircle size={16} />
      <span>{message}</span>
      {onRetry && (
        <Button variant="ghost" onClick={onRetry}>
          <RotateCw size={16} /> 重试
        </Button>
      )}
    </div>
  );
}

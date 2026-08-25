import type { ReactNode } from "react";
import { Button } from "./Button";
import { Modal } from "./Modal";

interface ConfirmModalProps {
  open: boolean;
  title: string;
  /** 说明后果的一句话，例如「删除后不可恢复」。 */
  description?: string;
  /** 确认按钮文案，默认「确认删除」。 */
  confirmLabel?: string;
  /** 危险性操作默认 danger；普通确认可传 primary。 */
  tone?: "danger" | "primary";
  busy?: boolean;
  onConfirm(): void;
  onClose(): void;
  children?: ReactNode;
}

/** 全站统一的破坏性操作确认弹窗，替代各处原生 window.confirm。 */
export function ConfirmModal({ open, title, description, confirmLabel = "确认删除", tone = "danger", busy = false, onConfirm, onClose, children }: ConfirmModalProps) {
  return (
    <Modal
      open={open}
      title={title}
      description={description}
      onClose={() => { if (!busy) onClose(); }}
      footer={<>
        <Button disabled={busy} onClick={onClose}>取消</Button>
        <Button variant={tone} loading={busy} onClick={onConfirm}>{confirmLabel}</Button>
      </>}
    >
      {children}
    </Modal>
  );
}

import { RefreshCw } from "lucide-react";
import { DeliverableBody, useDeliverables } from "../../features/conversation/DeliverablePanel";
import { Button } from "../../ui/Button";
import { useWorkspace } from "../../workspace/WorkspaceContext";

interface ProjectDeliverablesTabProps {
  projectId: string;
}

export function ProjectDeliverablesTab({ projectId }: ProjectDeliverablesTabProps) {
  const workspace = useWorkspace();
  const state = useDeliverables(projectId, undefined, workspace.invalidationVersion, true);
  return (
    <div className="project-deliverables-tab">
      <header className="project-tab-heading">
        <div>
          <h2>交付物</h2>
          <p>项目下不可变的阶段产物与最终产物；「改进」会开启一条新会话继续迭代。</p>
        </div>
        <Button variant="ghost" size="small" onClick={() => void state.refresh()}><RefreshCw size={16} />重读</Button>
      </header>
      <DeliverableBody state={state} projectId={projectId} previewOpen improveTo={`/project/${encodeURIComponent(projectId)}/conversations`} />
    </div>
  );
}

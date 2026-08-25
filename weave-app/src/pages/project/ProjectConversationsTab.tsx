import { ConversationPage } from "../ConversationPage";

interface ProjectConversationsTabProps {
  projectId: string;
  conversationId?: string;
  conversationIntent?: "create_team";
  threadRootId?: string;
  deliverablesOpen?: boolean;
  onDeliverablesOpenChange?: (open: boolean) => void;
}

export function ProjectConversationsTab({ projectId, conversationId, conversationIntent, threadRootId, deliverablesOpen, onDeliverablesOpenChange }: ProjectConversationsTabProps) {
  return (
    <div className="project-conversations project-conversations--full">
      <section className="project-conversations__main">
        <ConversationPage projectId={projectId} conversationId={conversationId} conversationIntent={conversationIntent} threadRootId={threadRootId} deliverablesOpen={deliverablesOpen} onDeliverablesOpenChange={onDeliverablesOpenChange} />
      </section>
    </div>
  );
}

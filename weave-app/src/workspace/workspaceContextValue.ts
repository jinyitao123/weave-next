import { createContext } from "react";
import type {
  AgentRecord,
  Conversation,
  CreateProjectInput,
  MoveProjectInput,
  PlatformFeatures,
  Project,
  Team,
  UnreadConversation,
  UpdateProjectInput,
} from "../api";

export interface WorkspaceContextValue {
  features: PlatformFeatures;
  agents: AgentRecord[];
  avatars: AgentRecord[];
  teams: Team[];
  projects: Project[];
  conversations: Conversation[];
  unread: UnreadConversation[];
  loading: boolean;
  connected: boolean;
  error: string | null;
  refresh(): Promise<void>;
  refreshConversations(projectId?: string): Promise<void>;
  renameConversation(conversationId: string, title: string): Promise<Conversation>;
  invalidationVersion: number;
  markConversationRead(conversationId: string, lastMessageId: string): Promise<void>;
  createProject(input: CreateProjectInput): Promise<Project>;
  ensureUnclassifiedProject(avatarId: string): Promise<Project>;
  updateProject(id: string, input: UpdateProjectInput): Promise<Project>;
  archiveProject(id: string): Promise<void>;
  restoreProject(id: string): Promise<Project>;
  moveProject(id: string, input: MoveProjectInput): Promise<Project>;
}

export const WorkspaceContext = createContext<WorkspaceContextValue | null>(null);

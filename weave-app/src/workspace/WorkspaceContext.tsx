import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type PropsWithChildren } from "react";
import {
  api,
  apiBaseUrl,
  apiErrorMessage,
  EventStreamClient,
  type AgentRecord,
  type Conversation,
  type CreateProjectInput,
  type MoveProjectInput,
  type Project,
  type Team,
  type UnreadConversation,
  type UpdateProjectInput,
} from "../api";
import { platform } from "../platform";
import { useAuth } from "../auth/AuthContext";
import { isPlatformAsset, TEAM_ARCHITECT_AGENT_NAME } from "./identity";

interface WorkspaceContextValue {
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

const WorkspaceContext = createContext<WorkspaceContextValue | null>(null);

export function WorkspaceProvider({ children }: PropsWithChildren) {
  const { status, logout } = useAuth();
  const [agents, setAgents] = useState<AgentRecord[]>([]);
  const [avatars, setAvatars] = useState<AgentRecord[]>([]);
  const [teams, setTeams] = useState<Team[]>([]);
  const [projects, setProjects] = useState<Project[]>([]);
  const [conversations, setConversations] = useState<Conversation[]>([]);
  const [unread, setUnread] = useState<UnreadConversation[]>([]);
  const [loading, setLoading] = useState(true);
  const [connected, setConnected] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [invalidationVersion, setInvalidationVersion] = useState(0);
  const mounted = useRef(true);

  const refresh = useCallback(async () => {
    if (status !== "authenticated") return;
    setLoading(true);
    try {
      const [agentRecords, teamArchitect, teamRecords, projectResponse, conversationRecords, unreadRecords] = await Promise.all([
        api.listAgents(),
        api.getAgent(TEAM_ARCHITECT_AGENT_NAME).catch(() => null),
        api.listTeams(),
        api.listProjects({ includeArchived: true }),
        api.listConversations(),
        api.listUnread(),
      ]);
      if (!mounted.current) return;
      const workspaceAgents = teamArchitect && !agentRecords.some((agent) => agent.id === teamArchitect.id)
        ? [...agentRecords, teamArchitect]
        : agentRecords;
      setAgents(workspaceAgents.filter((agent) => !agent.deleted));
      setAvatars(workspaceAgents.filter((agent) => agent.role === "avatar" && !agent.deleted && !isPlatformAsset(agent)));
      setTeams(teamRecords);
      setProjects(projectResponse.projects);
      setConversations(conversationRecords);
      setUnread(unreadRecords);
      setError(null);
    } catch (requestError) {
      if (mounted.current) setError(apiErrorMessage(requestError));
    } finally {
      if (mounted.current) setLoading(false);
    }
  }, [status]);
  const refreshConversations = useCallback(async (projectId?: string) => {
    if (status !== "authenticated") return;
    const records = await api.listConversations(projectId);
    if (!mounted.current) return;
    if (!projectId) {
      setConversations(records);
      return;
    }
    setConversations((current) => [
      ...current.filter((conversation) => conversation.project_id !== projectId),
      ...records,
    ].sort((left, right) => Date.parse(right.updated_at) - Date.parse(left.updated_at)));
  }, [status]);

  useEffect(() => {
    mounted.current = true;
    void refresh();
    return () => {
      mounted.current = false;
    };
  }, [refresh]);

  useEffect(() => {
    if (status !== "authenticated") return;
    const stream = new EventStreamClient({
      baseUrl: apiBaseUrl,
      tokens: platform.tokens,
      onConnectionChange: setConnected,
      refreshToken: () => api.refreshToken(),
      onAuthError: () => void logout(),
      onEvent(event) {
        if (event.type === "team_build_run_updated" || event.type === "team_build_operation_updated") {
          setInvalidationVersion((version) => version + 1);
          void refreshConversations().catch(() => undefined);
        }
        if (event.type === "team_published") {
          setInvalidationVersion((version) => version + 1);
          void refresh().catch(() => undefined);
          void refreshConversations().catch(() => undefined);
        }
        if (event.type === "message") {
          setInvalidationVersion((version) => version + 1);
          void refreshConversations().catch(() => undefined);
        }
        if (event.type === "message" || event.type === "unread") {
          if (event.type === "unread") setInvalidationVersion((version) => version + 1);
          void api.listUnread().then(setUnread).catch(() => undefined);
        }
      },
    });
    stream.start();
    return () => stream.stop();
  }, [logout, refresh, refreshConversations, status]);

  const replaceProject = useCallback((project: Project) => {
    setProjects((current) => {
      const exists = current.some(({ id }) => id === project.id);
      const next = exists ? current.map((item) => item.id === project.id ? project : item) : [project, ...current];
      return next.sort((left, right) => Date.parse(right.updated_at) - Date.parse(left.updated_at));
    });
  }, []);
  const markConversationRead = useCallback(async (conversationId: string, lastMessageId: string) => {
    await api.markConversationRead(conversationId, lastMessageId);
    setUnread(await api.listUnread());
  }, []);

  const value = useMemo<WorkspaceContextValue>(() => ({
    agents,
    avatars,
    teams,
    projects,
    conversations,
    unread,
    loading,
    connected,
    error,
    refresh,
    refreshConversations,
    async renameConversation(conversationId, title) {
      const conversation = await api.renameConversation(conversationId, title);
      await refreshConversations(conversation.project_id);
      return conversation;
    },
    invalidationVersion,
    markConversationRead,
    async createProject(input) {
      const project = await api.createProject(input);
      replaceProject(project);
      return project;
    },
    async ensureUnclassifiedProject(avatarId) {
      const team = teams.find((item) => item.lead_avatar_id === avatarId);
      const project = team
        ? await api.ensureUnclassifiedProject(team.id, "team")
        : await api.ensureUnclassifiedProject(avatarId, "avatar");
      replaceProject(project);
      return project;
    },
    async updateProject(id, input) {
      const project = await api.updateProject(id, input);
      replaceProject(project);
      return project;
    },
    async archiveProject(id) {
      await api.archiveProject(id);
      const projectResponse = await api.listProjects({ includeArchived: true });
      setProjects(projectResponse.projects);
    },
    async restoreProject(id) {
      const project = await api.restoreProject(id);
      replaceProject(project);
      return project;
    },
    async moveProject(id, input) {
      const project = await api.moveProject(id, input);
      replaceProject(project);
      return project;
    },
  }), [agents, avatars, connected, conversations, error, invalidationVersion, loading, markConversationRead, projects, refresh, refreshConversations, replaceProject, teams, unread]);

  return <WorkspaceContext.Provider value={value}>{children}</WorkspaceContext.Provider>;
}

export function useWorkspace(): WorkspaceContextValue {
  const value = useContext(WorkspaceContext);
  if (!value) throw new Error("useWorkspace must be used within WorkspaceProvider");
  return value;
}

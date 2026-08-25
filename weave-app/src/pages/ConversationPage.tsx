import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type ChangeEvent, type CSSProperties, type FormEvent, type KeyboardEvent as ReactKeyboardEvent, type MouseEvent as ReactMouseEvent, type PointerEvent as ReactPointerEvent, type ReactNode } from "react";
import { ArrowLeft, Bot, ChevronDown, LoaderCircle, LockKeyhole, MessageSquarePlus, MessagesSquare, Paperclip, Plus, Send, Square, UserPlus, Users, X } from "lucide-react";
import { useNavigate } from "react-router-dom";
import "../styles/conversation.css";
import { api, apiErrorMessage, normalizeThrownError, type AssistantMessageMetadata, type Attachment, type BlueprintRevisionToken, type Conversation, type Message, type MessageAttachment, type ProjectResource, type Runtime, type ThreadSummary } from "../api";
import { ConversationMessage, PendingTurnMessages, type PendingTurn } from "../features/conversation/ConversationMessage";
import { applyChatTurnStreamEvent } from "../features/conversation/chatTurnEvents";
import { pollAttachedWorkflowProgress, reconnectPendingTurn as reconnectPendingChatTurn, type ChatTurnRecoveryContext } from "../features/conversation/chatTurnRecovery";
import { buildChatTurnRequest, chatTurnBlockedReason, createPendingTurn } from "../features/conversation/chatTurnRequest";
import { handleChatTurnTerminal, type ChatTurnTerminalContext } from "../features/conversation/chatTurnTerminal";
import { DeliverableBody } from "../features/conversation/DeliverablePanel";
import { useDeliverables } from "../features/conversation/useDeliverables";
import { MarkdownText } from "../features/conversation/MarkdownText";
import { RuntimeAssignmentNotice } from "../features/conversation/RuntimeAssignmentNotice";
import { useChatTurnController } from "../features/conversation/useChatTurnController";
import { Button } from "../ui/Button";
import { Card } from "../ui/Card";
import { ErrorNotice, LoadingView } from "../ui/StatusViews";
import { useWorkspace } from "../workspace/useWorkspace";
import { isPlatformAsset, isUnclassifiedProject, projectDisplayName, TEAM_ARCHITECT_AGENT_NAME } from "../workspace/identity";

const cliEngines: Record<string, true> = { claude: true, codex: true, opencode: true };
const runtimeUnavailableLabels: Record<string, string> = {
  runtime_disabled: "已停用", runtime_revoked: "已撤销", runtime_offline: "离线", engine_unavailable: "不支持当前引擎",
};
const DEFAULT_DELIVERABLE_PANEL_WIDTH = 440;
const MIN_DELIVERABLE_PANEL_WIDTH = 320;
const MAX_DELIVERABLE_PANEL_WIDTH = 720;

interface TimelineScrollSnapshot {
  routeKey: string;
  anchor: string;
  anchorOffset: number;
  scrollTop: number;
}

function captureTimelineScroll(element: HTMLDivElement, routeKey: string): TimelineScrollSnapshot {
  const containerTop = element.getBoundingClientRect().top;
  const anchors = Array.from(element.querySelectorAll<HTMLElement>("[data-scroll-anchor]"));
  const anchor = anchors.find((candidate) => candidate.getBoundingClientRect().bottom > containerTop + 1);
  return {
    routeKey,
    anchor: anchor?.dataset.scrollAnchor || "",
    anchorOffset: anchor ? anchor.getBoundingClientRect().top - containerTop : 0,
    scrollTop: element.scrollTop,
  };
}

function restoreTimelineScroll(element: HTMLDivElement, snapshot: TimelineScrollSnapshot): void {
  if (snapshot.anchor) {
    const anchor = Array.from(element.querySelectorAll<HTMLElement>("[data-scroll-anchor]"))
      .find((candidate) => candidate.dataset.scrollAnchor === snapshot.anchor);
    if (anchor) {
      const currentOffset = anchor.getBoundingClientRect().top - element.getBoundingClientRect().top;
      element.scrollTop += currentOffset - snapshot.anchorOffset;
      return;
    }
  }
  element.scrollTop = snapshot.scrollTop;
}

function deliverablePanelMaximum(workspace: HTMLDivElement | null): number {
  const workspaceWidth = workspace?.getBoundingClientRect().width || MAX_DELIVERABLE_PANEL_WIDTH / 0.6;
  return Math.max(MIN_DELIVERABLE_PANEL_WIDTH, Math.min(MAX_DELIVERABLE_PANEL_WIDTH, workspaceWidth * 0.6));
}

function clampDeliverablePanelWidth(width: number, workspace: HTMLDivElement | null): number {
  return Math.max(MIN_DELIVERABLE_PANEL_WIDTH, Math.min(width, deliverablePanelMaximum(workspace)));
}

function matchingRootConversations(thread: Conversation, conversations: Conversation[]): Conversation[] {
  return conversations.filter((conversation) =>
    !conversation.parent_message_id &&
    conversation.project_id === thread.project_id &&
    conversation.agent_id === thread.agent_id &&
    conversation.user_id === thread.user_id &&
    conversation.channel === thread.channel,
  );
}

function chatWriteErrorMessage(error: unknown): string {
  const normalized = normalizeThrownError(error);
  const code = normalized.code || normalized.message;
  if (code === "conversation_agent_mismatch") {
    return "当前历史会话属于迁移前的旧团队负责人，已转为只读；请在该项目下开始新会话，与当前团队继续。";
  }
  if (code === "project_avatar_mismatch") {
    return "当前项目已迁移到其他团队，原负责人不能继续写入；请在该项目下开始新会话，与当前团队继续。";
  }
  return apiErrorMessage(normalized);
}

export interface ConversationPageProps {
  /** 项目 ID（由项目容器从路由参数注入）。 */
  projectId: string;
  /** 选中的会话 ID；缺省表示开始新会话。 */
  conversationId?: string;
  /** 新会话入口意图；持久会话创建后以服务端 Conversation.intent 为准。 */
  conversationIntent?: "create_team";
  /** 线程主会话 ID（thread_root 深链）。 */
  threadRootId?: string;
  /** 交付物侧栏打开状态（受控）；缺省时组件内部自管理。 */
  deliverablesOpen?: boolean;
  /** 交付物侧栏开关回调（配合受控状态）。 */
  onDeliverablesOpenChange?: (open: boolean) => void;
}

export function ConversationPage({ projectId, conversationId = "", conversationIntent, threadRootId: requestedThreadRootId = "", deliverablesOpen: deliverablesOpenProp, onDeliverablesOpenChange }: ConversationPageProps) {
  const { agents, conversations, ensureUnclassifiedProject, invalidationVersion, loading, markConversationRead, projects, refreshConversations, teams } = useWorkspace();
  const navigate = useNavigate();
  const [teamNavigationBusy, setTeamNavigationBusy] = useState("");
  const [teamNavigationError, setTeamNavigationError] = useState<string | null>(null);
  const selectedProject = projects.find((project) => project.id === projectId);
  const activeTeams = teams.filter((team) => {
    if (team.status !== "active") return false;
    const lead = agents.find((agent) => agent.id === team.lead_avatar_id);
    return !!lead && !lead.deleted && !isPlatformAsset(lead);
  });
  const selectedTeam = activeTeams.find((team) => team.id === selectedProject?.team_id);
  const projectAvatar = agents.find((agent) => agent.id === selectedProject?.avatar_id && !agent.deleted);
  const teamLeadAvatar = agents.find((agent) => agent.id === selectedTeam?.lead_avatar_id && !agent.deleted);
  const selectedAvatar = [projectAvatar, teamLeadAvatar].find((agent) => agent?.role === "avatar") || projectAvatar || teamLeadAvatar;
  const projectConversations = useMemo(() => conversations.filter((conversation) => conversation.project_id === projectId), [conversations, projectId]);
  const selectedConversation = projectConversations.find((conversation) => conversation.id === conversationId);
  const requestedThreadRoot = projectConversations.find((conversation) => conversation.id === requestedThreadRootId && !conversation.parent_message_id);
  const rootCandidates = useMemo(() => selectedConversation?.parent_message_id ? matchingRootConversations(selectedConversation, projectConversations) : [], [projectConversations, selectedConversation]);
  const matchedThreadRoot = selectedConversation?.parent_message_id && rootCandidates.length === 1 ? rootCandidates[0] : undefined;
  const routeThreadRootCandidates = !selectedConversation && requestedThreadRoot ? projectConversations.filter((conversation) => conversation.id === requestedThreadRoot.id && !conversation.parent_message_id) : [];
  const routeThreadRoot = routeThreadRootCandidates.length === 1 ? routeThreadRootCandidates[0] : undefined;
  const selectedRoot = selectedConversation?.parent_message_id ? matchedThreadRoot : selectedConversation || routeThreadRoot;
  const threadSummaryRootIds = useMemo(() => {
    const ids = new Set<string>();
    if (requestedThreadRoot?.id) ids.add(requestedThreadRoot.id);
    if (selectedConversation?.parent_message_id) {
      for (const root of rootCandidates) ids.add(root.id);
    } else if (selectedConversation && !selectedConversation.parent_message_id) {
      ids.add(selectedConversation.id);
    }
    return Array.from(ids).sort();
  }, [requestedThreadRoot?.id, rootCandidates, selectedConversation]);
  const threadSummaryRootIdsKey = threadSummaryRootIds.join(":");
  const rootResolutionError = selectedConversation?.parent_message_id && rootCandidates.length !== 1
    ? rootCandidates.length === 0 ? "暂时无法定位这个讨论的主会话，请返回会话列表重试。" : "暂时无法定位这个讨论的主会话，请返回会话列表重试。"
    : requestedThreadRootId && (!requestedThreadRoot || routeThreadRootCandidates.length !== 1) ? "暂时无法定位这个讨论的主会话，请返回会话列表重试。"
      : conversationId && !selectedConversation && !requestedThreadRootId ? "当前会话不在已加载的服务端会话事实中，无法判断它是主会话还是线程。" : null;
  const [messages, setMessages] = useState<Message[]>([]);
  const [resources, setResources] = useState<ProjectResource[]>([]);
  const [selectedAttachments, setSelectedAttachments] = useState<string[]>([]);
  const [uploadedAttachments, setUploadedAttachments] = useState<Attachment[]>([]);
  const [uploadingAttachments, setUploadingAttachments] = useState(false);
  const [attachmentUploadError, setAttachmentUploadError] = useState<string | null>(null);
  const [draft, setDraft] = useState(() => sessionStorage.getItem(`weave.draft.${projectId}:${conversationId}`) || "");
  const draftRef = useRef("");
  const { pending, pendingRef, setPending, clearPending, readStoredPending } = useChatTurnController();
  const [loadingMessages, setLoadingMessages] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [threadSummariesByRoot, setThreadSummariesByRoot] = useState<Record<string, ThreadSummary[]>>({});
  const [threadError, setThreadError] = useState<string | null>(null);
  const [creatingThreadMessageId, setCreatingThreadMessageId] = useState("");
  const [promotingMessageId, setPromotingMessageId] = useState("");
  const [promotedMessageIds, setPromotedMessageIds] = useState<Record<string, true>>({});
  const [promoteErrors, setPromoteErrors] = useState<Record<string, string>>({});
  const [deliverableBump, setDeliverableBump] = useState(0);
  const [deliverablesOpenInternal, setDeliverablesOpenInternal] = useState(false);
  const deliverablesOpen = deliverablesOpenProp ?? deliverablesOpenInternal;
  const setDeliverablesOpen = (open: boolean) => {
    if (onDeliverablesOpenChange) onDeliverablesOpenChange(open);
    else setDeliverablesOpenInternal(open);
  };
  const [deliverablePanelWidth, setDeliverablePanelWidth] = useState(DEFAULT_DELIVERABLE_PANEL_WIDTH);
  const [deliverablePanelDragging, setDeliverablePanelDragging] = useState(false);
  const deliverables = useDeliverables(projectId, conversationId || undefined, invalidationVersion + deliverableBump, !!selectedProject && !!conversationId);
  const [publicationError, setPublicationError] = useState<string | null>(null);
  const [runtimes, setRuntimes] = useState<Runtime[]>([]);
  const [selectedRuntimeId, setSelectedRuntimeId] = useState("");
  const [runtimeError, setRuntimeError] = useState<string | null>(null);
  const isCLIEngine = !!cliEngines[selectedAvatar?.engine || ""];
  const abortRef = useRef<AbortController | null>(null);
	const reconnectPendingTurnRef = useRef<(source?: PendingTurn | null) => Promise<void>>(async () => undefined);
  const attachmentInputRef = useRef<HTMLInputElement>(null);
  const messagesLengthRef = useRef(0);
  const conversationWorkspaceRef = useRef<HTMLDivElement>(null);
  const timelineRef = useRef<HTMLDivElement>(null);
	const pendingTurnRef = useRef<HTMLDivElement>(null);
	const pendingTimelineScrollRef = useRef<TimelineScrollSnapshot | null>(null);
  const panelDragPointerRef = useRef<number | null>(null);
  const panelDragStartRef = useRef<{ pointerX: number; width: number } | null>(null);
  const correctingConversationRef = useRef("");
  const routeKeyRef = useRef(`${projectId}:${conversationId}`);
  const threadRootId = selectedRoot?.id || "";
  const threadSummaries = threadRootId ? threadSummariesByRoot[threadRootId] || [] : [];
  const selectedThreadSummary = threadSummaries.find((summary) => summary.thread_id === conversationId);
  const routeThreadMatchesRoot = !!(routeThreadRoot && selectedThreadSummary &&
    routeThreadRoot.project_id === projectId &&
    routeThreadRoot.agent_id === selectedProject?.avatar_id);
  const isSelectedThread = !!selectedConversation?.parent_message_id || routeThreadMatchesRoot;
  const threadRoot = isSelectedThread ? selectedRoot : undefined;
  const selectedConversationMatchesRoute = !!(selectedConversation &&
    selectedConversation.project_id === projectId &&
    selectedConversation.agent_id === selectedProject?.avatar_id);
  const exactTargetConversation = selectedConversationMatchesRoute ? selectedConversation : routeThreadMatchesRoot ? {
    id: conversationId,
    channel: routeThreadRoot!.channel,
    session_key: undefined,
  } : undefined;
  const conversationAvatarId = selectedConversation?.agent_id || routeThreadRoot?.agent_id;
  const isReadOnlyHistory = !!(conversationId && selectedProject && conversationAvatarId && conversationAvatarId !== selectedProject.avatar_id);
  const isReadOnlySharedConversation = !!(conversationId && (selectedConversation?.read_only || routeThreadRoot?.read_only));
  const isReadOnlyConversation = isReadOnlyHistory || isReadOnlySharedConversation;

  useEffect(() => {
    if (selectedProject) window.localStorage.setItem("weave.lastProjectId", selectedProject.id);
  }, [selectedProject]);

  useEffect(() => {
    const routeKey = `${projectId}:${conversationId}`;
    if (correctingConversationRef.current && correctingConversationRef.current === conversationId) {
      routeKeyRef.current = routeKey;
      correctingConversationRef.current = "";
      return;
    }
    if (routeKeyRef.current === routeKey) return;
    routeKeyRef.current = routeKey;
    abortRef.current?.abort();
    clearPending();
    messagesLengthRef.current = 0;
    setMessages([]);
    setLoadingMessages(false);
    setDraft(sessionStorage.getItem(`weave.draft.${projectId}:${conversationId}`) || "");
    setSelectedAttachments([]);
    setUploadedAttachments([]);
    setAttachmentUploadError(null);
  }, [clearPending, conversationId, projectId]);

  const draftStorageKey = `weave.draft.${projectId}:${conversationId}`;
  useEffect(() => {
    if (draft.trim()) sessionStorage.setItem(draftStorageKey, draft);
    else sessionStorage.removeItem(draftStorageKey);
  }, [draft, draftStorageKey]);

	useEffect(() => {
		if (pendingRef.current || !selectedProject || !selectedAvatar) return;
		const restored = readStoredPending();
		if (!restored) return;
		const matchesConversation = restored.request.conversation_id
			? restored.request.conversation_id === conversationId
			: !conversationId;
		if (restored.request.project_id !== selectedProject.id || restored.request.agent !== selectedAvatar.name || !matchesConversation) return;
		const reconnecting: PendingTurn = {
			...restored,
			state: "sending",
			interrupted: undefined,
			interruptedReason: undefined,
			error: undefined,
			activity: "正在恢复执行状态",
		};
		pendingRef.current = reconnecting;
		setPending(reconnecting);
		void reconnectPendingTurnRef.current(reconnecting);
	}, [conversationId, pendingRef, readStoredPending, selectedAvatar, selectedProject, setPending]);

  useEffect(() => {
    messagesLengthRef.current = messages.length;
  }, [messages.length]);

  async function preserveTimelineScroll<T>(operation: () => Promise<T>): Promise<T> {
    const element = timelineRef.current;
	if (element) pendingTimelineScrollRef.current = captureTimelineScroll(element, routeKeyRef.current);
    try {
      return await operation();
    } finally {
		window.requestAnimationFrame(() => window.requestAnimationFrame(() => {
			const snapshot = pendingTimelineScrollRef.current;
			if (!snapshot || !element || timelineRef.current !== element || snapshot.routeKey !== routeKeyRef.current) return;
			restoreTimelineScroll(element, snapshot);
			pendingTimelineScrollRef.current = null;
		}));
    }
  }

	useLayoutEffect(() => {
		const snapshot = pendingTimelineScrollRef.current;
		const element = timelineRef.current;
		if (!snapshot || !element) return;
		if (snapshot.routeKey !== routeKeyRef.current) {
			pendingTimelineScrollRef.current = null;
			return;
		}
		restoreTimelineScroll(element, snapshot);
		pendingTimelineScrollRef.current = null;
	}, [invalidationVersion, messages, selectedConversation?.build_run?.status]);

	useLayoutEffect(() => {
		if (!pending?.clientRequestId) return;
		pendingTurnRef.current?.scrollIntoView({ block: "start" });
	}, [pending?.clientRequestId]);

  const updateDeliverablePanelWidth = useCallback((width: number) => {
    setDeliverablePanelWidth(clampDeliverablePanelWidth(width, conversationWorkspaceRef.current));
  }, []);

  useEffect(() => {
    if (!deliverablesOpen) return;
    const handleWindowResize = () => {
      setDeliverablePanelWidth((current) => clampDeliverablePanelWidth(current, conversationWorkspaceRef.current));
    };
    handleWindowResize();
    window.addEventListener("resize", handleWindowResize);
    return () => {
      window.removeEventListener("resize", handleWindowResize);
    };
  }, [deliverablesOpen]);

  function startDeliverablePanelDrag(event: ReactPointerEvent<HTMLDivElement>) {
    if (event.button !== 0) return;
    event.preventDefault();
    panelDragPointerRef.current = event.pointerId;
    panelDragStartRef.current = { pointerX: event.clientX, width: deliverablePanelWidth };
    setDeliverablePanelDragging(true);
    event.currentTarget.setPointerCapture(event.pointerId);
  }

  function dragDeliverablePanel(event: ReactPointerEvent<HTMLDivElement>) {
    const start = panelDragStartRef.current;
    if (panelDragPointerRef.current !== event.pointerId || !start) return;
    updateDeliverablePanelWidth(start.width + start.pointerX - event.clientX);
  }

  function stopDeliverablePanelDrag(event: ReactPointerEvent<HTMLDivElement>) {
    if (panelDragPointerRef.current !== event.pointerId) return;
    panelDragPointerRef.current = null;
    panelDragStartRef.current = null;
    setDeliverablePanelDragging(false);
    if (event.currentTarget.hasPointerCapture(event.pointerId)) event.currentTarget.releasePointerCapture(event.pointerId);
  }

  function resizeDeliverablePanelWithKeyboard(event: ReactKeyboardEvent<HTMLDivElement>) {
    if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return;
    event.preventDefault();
    updateDeliverablePanelWidth(deliverablePanelWidth + (event.key === "ArrowLeft" ? 24 : -24));
  }

  const refreshThreads = useCallback(async (rootConversationId: string, signal?: AbortSignal) => {
    try {
      const response = await api.listThreads(rootConversationId, signal);
      setThreadSummariesByRoot((current) => ({ ...current, [rootConversationId]: response.thread_summaries }));
      setThreadError(null);
    } catch (requestError) {
      if (!(requestError instanceof DOMException && requestError.name === "AbortError")) {
        setThreadError(apiErrorMessage(requestError));
      }
    }
  }, []);

  useEffect(() => {
    if (!projectId || threadSummaryRootIds.length === 0) {
      setThreadSummariesByRoot({});
      setThreadError(rootResolutionError);
      return;
    }
    const controller = new AbortController();
    void Promise.all(threadSummaryRootIds.map(async (rootConversationId) => {
      const response = await api.listThreads(rootConversationId, controller.signal);
      return [rootConversationId, response.thread_summaries] as const;
    })).then((entries) => {
      setThreadSummariesByRoot(Object.fromEntries(entries));
      setThreadError(rootResolutionError);
    }).catch((requestError) => {
      if (!controller.signal.aborted) setThreadError(apiErrorMessage(requestError));
    });
    return () => controller.abort();
  }, [invalidationVersion, projectId, rootResolutionError, threadSummaryRootIds, threadSummaryRootIdsKey]);

  useEffect(() => {
    if (!requestedThreadRootId || !threadRootId || !conversationId || selectedConversation) return;
    if (!selectedThreadSummary) {
      setThreadError("主会话的服务端线程摘要中不存在当前线程，无法安全关联。");
    } else if (!routeThreadMatchesRoot) {
      setThreadError("当前线程与主会话的已加载服务端归属事实不一致，无法安全关联。");
    } else {
      setThreadError(null);
    }
  }, [conversationId, requestedThreadRootId, routeThreadMatchesRoot, selectedConversation, selectedThreadSummary, threadRootId]);

  useEffect(() => {
    setSelectedRuntimeId("");
    if (!isCLIEngine) { setRuntimes([]); setRuntimeError(null); return; }
    const controller = new AbortController();
    void api.listRuntimes(controller.signal).then(({ runtimes: records }) => { setRuntimes(records); setRuntimeError(null); }).catch((error) => { if (!controller.signal.aborted) setRuntimeError(apiErrorMessage(error)); });
    return () => controller.abort();
  }, [isCLIEngine, selectedAvatar?.engine]);

  useEffect(() => {
    if (!projectId) {
      setResources([]);
      return;
    }
    const controller = new AbortController();
    void Promise.all([
      refreshConversations(projectId),
      api.listProjectResources(projectId, controller.signal).then(({ resources: records }) => setResources(records)),
    ]).catch((error) => {
      if (!controller.signal.aborted) setLoadError(apiErrorMessage(error));
    });
    return () => controller.abort();
  }, [projectId, refreshConversations]);

  useEffect(() => {
    if (!conversationId) {
      setMessages([]);
      return;
    }
    const controller = new AbortController();
    const showBlockingLoading = messagesLengthRef.current === 0 && !pendingRef.current;
    if (showBlockingLoading) setLoadingMessages(true);
    setLoadError(null);
    void api.listMessages(conversationId, controller.signal)
      .then((records) => {
        const ordered = records.sort((left, right) => left.seq - right.seq);
        setMessages(ordered);
        const last = ordered.at(-1);
        if (last) void markConversationRead(conversationId, last.id).catch(() => undefined);
      })
      .catch((error) => { if (!controller.signal.aborted) setLoadError(apiErrorMessage(error)); })
      .finally(() => { if (!controller.signal.aborted && showBlockingLoading) setLoadingMessages(false); });
    return () => controller.abort();
  }, [conversationId, invalidationVersion, markConversationRead, pendingRef]);

  useEffect(() => () => abortRef.current?.abort(), []);

  async function createThread(parentMessageId: string) {
    if (!selectedConversation || selectedConversation.read_only || isSelectedThread || selectedConversation.parent_message_id || creatingThreadMessageId) return;
    setCreatingThreadMessageId(parentMessageId);
    setThreadError(null);
    try {
      const response = await api.createThread(selectedConversation.id, { parent_message_id: parentMessageId });
      await refreshConversations(projectId);
      await refreshThreads(selectedConversation.id);
      navigate(`/project/${encodeURIComponent(projectId)}/conversations/${encodeURIComponent(response.thread_id)}?thread_root=${encodeURIComponent(selectedConversation.id)}`);
    } catch (requestError) {
      setThreadError(apiErrorMessage(requestError));
    } finally {
      setCreatingThreadMessageId("");
    }
  }

  async function promoteDeliverable(messageId: string) {
    if (promotingMessageId) return;
    setPromotingMessageId(messageId);
    setPromoteErrors((current) => ({ ...current, [messageId]: "" }));
    try {
      await api.promoteMessageDeliverable(messageId);
      setPromotedMessageIds((current) => ({ ...current, [messageId]: true }));
      setDeliverableBump((value) => value + 1);
    } catch (requestError) {
      setPromoteErrors((current) => ({ ...current, [messageId]: apiErrorMessage(requestError) }));
    } finally {
      setPromotingMessageId("");
    }
  }

  async function reloadPersisted(targetConversationId: string) {
    const expectedRouteKey = routeKeyRef.current;
    try {
      const records = await api.listMessages(targetConversationId);
      if (routeKeyRef.current !== expectedRouteKey) return;
      const ordered = records.sort((left, right) => left.seq - right.seq);
      setMessages(ordered);
      const last = ordered.at(-1);
      if (last) await markConversationRead(targetConversationId, last.id);
      await refreshConversations(projectId);
      if (routeKeyRef.current === expectedRouteKey) setLoadError(null);
    } catch (error) {
      if (routeKeyRef.current === expectedRouteKey) setLoadError(apiErrorMessage(error));
    }
  }

  async function reloadUntilMessage(targetConversationId: string, userMessageId?: string, assistantContent?: string): Promise<Message | undefined> {
    const expectedRouteKey = routeKeyRef.current;
    for (let attempt = 0; attempt < 8; attempt += 1) {
      const records = await api.listMessages(targetConversationId);
      if (routeKeyRef.current !== expectedRouteKey) throw new DOMException("请求已取消", "AbortError");
      const userMessage = userMessageId ? records.find((message) => message.id === userMessageId) : undefined;
      const hasUserMessage = !userMessageId || !!userMessage;
      const assistantMessage = assistantContent ? records.find((message) => (
        message.role === "assistant"
        && (!userMessage || message.seq > userMessage.seq)
        && message.content === assistantContent
      )) : undefined;
      const hasAssistantMessage = !assistantContent || !!assistantMessage;
      if (hasUserMessage && hasAssistantMessage) {
        setMessages(records.sort((left, right) => left.seq - right.seq));
        await refreshConversations(projectId);
        if (routeKeyRef.current !== expectedRouteKey) throw new DOMException("请求已取消", "AbortError");
        return assistantMessage;
      }
      await new Promise<void>((resolve) => window.setTimeout(resolve, 200 * (attempt + 1)));
    }
    throw new Error(assistantContent ? "服务端返回的回复尚未出现在会话中，请刷新重试。" : "服务端返回的消息尚未出现在会话中，请刷新重试。");
  }

  function navigateToConversation(targetConversationId: string, targetRootId?: string, replace = false) {
    const rootQuery = targetRootId ? `&thread_root=${encodeURIComponent(targetRootId)}` : "";
    navigate(`/project/${encodeURIComponent(projectId)}/conversations/${encodeURIComponent(targetConversationId)}${rootQuery}`, { replace });
  }

  function chatTurnRecoveryContext(): ChatTurnRecoveryContext {
    return {
      projectId,
      conversationId,
      isSelectedThread,
      threadRootId,
      pendingRef,
      abortRef,
      setPending,
      clearPending,
      routeKey: () => routeKeyRef.current,
      setCorrectingConversation: (targetConversationId) => {
        correctingConversationRef.current = targetConversationId;
      },
      navigateConversation: navigateToConversation,
      reloadUntilMessage,
      refreshConversations,
    };
  }

  function chatTurnTerminalContext(): ChatTurnTerminalContext {
    return {
      projectId,
      conversationId,
      isSelectedThread,
      threadRootId,
      pendingRef,
      setPending,
      clearPending,
      setMessages,
      setCorrectingConversation: (targetConversationId) => {
        correctingConversationRef.current = targetConversationId;
      },
      navigateConversation: navigateToConversation,
      reloadUntilMessage,
      updateAssistantExecutionSegments: api.updateAssistantExecutionSegments.bind(api),
    };
  }

  async function reconnectPendingTurn(source = pendingRef.current): Promise<void> {
    await reconnectPendingChatTurn(chatTurnRecoveryContext(), source);
  }
  reconnectPendingTurnRef.current = reconnectPendingTurn;

  async function uploadComposerAttachments(event: ChangeEvent<HTMLInputElement>) {
    const files = Array.from(event.target.files || []);
    event.target.value = "";
    if (!files.length) return;
    setUploadingAttachments(true);
    setAttachmentUploadError(null);
    try {
      const results = await Promise.allSettled(files.map((file) => api.uploadAttachment(file)));
      const uploaded = results.flatMap((result) => result.status === "fulfilled" ? [result.value] : []);
      if (uploaded.length > 0) {
        setUploadedAttachments((current) => {
          const known = new Set(current.map((attachment) => attachment.id));
          return [...current, ...uploaded.filter((attachment) => !known.has(attachment.id))];
        });
        setSelectedAttachments((current) => Array.from(new Set([...current, ...uploaded.map((attachment) => attachment.id)])));
      }
      const failed = results.filter((result): result is PromiseRejectedResult => result.status === "rejected");
      if (failed.length > 0) {
        setAttachmentUploadError(`${failed.length} 个文件上传失败：${apiErrorMessage(failed[0].reason)}`);
      }
    } finally {
      setUploadingAttachments(false);
    }
  }

  async function submitTurn(event?: FormEvent, messageOverride?: string) {
    event?.preventDefault();
    const currentPending = pendingRef.current;
    const blockedReason = chatTurnBlockedReason({
      selectedProject,
      selectedAvatar,
      uploadingAttachments,
      conversationId,
      exactTargetConversation,
      currentPending,
    });
    if (blockedReason) {
      setLoadError(blockedReason);
      writeSubmitDebug("blocked", { blockedReason });
      return;
    }
    const request = buildChatTurnRequest({
      currentPending,
      selectedProject: selectedProject!,
      selectedAvatar: selectedAvatar!,
      exactTargetConversation,
      conversationIntent,
      isCLIEngine,
      selectedRuntimeId,
      messageOverride,
      composerValue: composerTextareaRef.current?.value,
      draftRefValue: draftRef.current,
      draft,
      selectedAttachments,
      blueprintChangeRequested,
    });
    if (!request.message) {
      setLoadError("请输入消息内容后再发送。");
      writeSubmitDebug("empty_message", { blockedReason: "empty_message" });
      return;
    }
    writeSubmitDebug("submit", {
      agent: request.agent,
      conversationId: request.conversation_id || "",
      messageLength: request.message.length,
      projectId: request.project_id,
    });
    const nextPending = createPendingTurn({
      request,
      currentPending,
      selectedAttachmentFacts,
    });
    pendingRef.current = nextPending;
    setPending(nextPending);
    setLoadError(null);
    if (!currentPending && !messageOverride) setDraft("");
		const requestRouteKey = routeKeyRef.current;

    try {
      const controller = new AbortController();
      abortRef.current = controller;
		void pollAttachedWorkflowProgress(chatTurnRecoveryContext(), nextPending, controller);
      const result = await api.streamChat(request, (streamEvent) => {
        const applied = pendingRef.current
          ? applyChatTurnStreamEvent(pendingRef.current, streamEvent, request.agent)
          : null;
        if (!applied) return;
        setPending(applied.pending);
        if (applied.shouldRefreshConversations) void refreshConversations();
      }, controller.signal);
      await handleChatTurnTerminal(chatTurnTerminalContext(), request, result);
      setSelectedAttachments([]);
      setBlueprintChangeRequested(false);
    } catch (error) {
		if (routeKeyRef.current !== requestRouteKey) return;
      if (normalizeThrownError(error).kind === "aborted") {
        const stopped = pendingRef.current;
        if (stopped) {
          pendingRef.current = { ...stopped, state: "failed", interrupted: true, interruptedReason: "stopped", error: "已停止等待。服务端仍在执行，可继续等待结果；恢复为草稿会创建一个新请求。" };
          setPending(pendingRef.current);
        }
      } else {
        const writeMessage = chatWriteErrorMessage(error);
        const normalized = normalizeThrownError(error);
        const serverStillRunning = normalized.code === "client_request_in_progress";
        setPending((current) => {
          const failed = current ? { ...current, state: "failed" as const, interrupted: serverStillRunning, interruptedReason: serverStillRunning ? "disconnected" as const : undefined, error: serverStillRunning ? "连接已断开，该请求仍在服务端执行。" : writeMessage } : current;
          pendingRef.current = failed;
          return failed;
        });
      }
    } finally {
      abortRef.current = null;
    }
  }

  const attachmentFactsByID = useMemo(() => {
    const facts = new Map<string, MessageAttachment>();
    for (const resource of resources) {
      if (resource.kind === "attachment" && resource.available) {
        facts.set(resource.resource_ref, { id: resource.resource_ref, filename: resource.display_name });
      }
    }
    for (const attachment of uploadedAttachments) {
      facts.set(attachment.id, { id: attachment.id, filename: attachment.filename });
    }
    return facts;
  }, [resources, uploadedAttachments]);
  const selectedAttachmentFacts = selectedAttachments.flatMap((id) => {
    const attachment = attachmentFactsByID.get(id);
    return attachment ? [attachment] : [];
  });
  const selectableAttachmentFacts = Array.from(attachmentFactsByID.values());

  async function openLeadQuickConversation(avatarId: string, busyKey: string) {
    setTeamNavigationBusy(busyKey);
    setTeamNavigationError(null);
    try {
      const project = await ensureUnclassifiedProject(avatarId);
      navigate(`/project/${encodeURIComponent(project.id)}/conversations${busyKey === TEAM_ARCHITECT_AGENT_NAME ? "?intent=create_team" : ""}`);
    } catch (error) {
      const team = activeTeams.find((item) => item.lead_avatar_id === avatarId);
      const fallback = projects.find((project) => project.team_id === team?.id && !project.archived_at && isUnclassifiedProject(project))
        || projects.find((project) => project.team_id === team?.id && !project.archived_at)
        || projects.find((project) => project.avatar_id === avatarId && !project.archived_at && isUnclassifiedProject(project))
        || projects.find((project) => project.avatar_id === avatarId && !project.archived_at);
      if (fallback) navigate(`/project/${encodeURIComponent(fallback.id)}/conversations${busyKey === TEAM_ARCHITECT_AGENT_NAME ? "?intent=create_team" : ""}`);
      else setTeamNavigationError(apiErrorMessage(error));
    } finally {
      setTeamNavigationBusy("");
    }
  }

  function openTeam(teamId: string) {
    const team = activeTeams.find((item) => item.id === teamId);
    if (team) void openLeadQuickConversation(team.lead_avatar_id, team.id);
  }

  async function submitBuildRunFromCard(buildRunId: string, token: BlueprintRevisionToken) {
    setPublicationError(null);
    try {
      await api.submitTeamBuildRun(buildRunId, {
        authority: "continue_build",
        revision_token: token,
      });
      await preserveTimelineScroll(async () => {
        await refreshConversations(projectId);
      });
    } catch (error) {
      setPublicationError(apiErrorMessage(error));
    }
  }

  function openTeamArchitect() {
    const architect = agents.find((agent) => agent.name === TEAM_ARCHITECT_AGENT_NAME && agent.role === "avatar" && !agent.deleted);
    if (!architect) {
      setTeamNavigationError("元团队尚未就绪，请联系管理员检查 __team_architect 资产。");
      return;
    }
    void openLeadQuickConversation(architect.id, TEAM_ARCHITECT_AGENT_NAME);
  }

  const projectChipItems: ChipMenuItem[] = projects.flatMap((project) => {
    if (project.archived_at) return [];
    const team = activeTeams.find((item) => item.id === project.team_id);
    if (!team) return [];
    const owner = agents.find((agent) => agent.id === project.avatar_id);
    return [{ id: project.id, label: projectDisplayName(project, teams, agents), hint: team.name || owner?.display_name || owner?.name }];
  });
  const teamChipItems: ChipMenuItem[] = activeTeams.map((team) => {
    const lead = agents.find((agent) => agent.id === team.lead_avatar_id);
    return { id: team.id, label: team.name, hint: lead?.display_name || lead?.name };
  });
  const runtimeChipItems: ChipMenuItem[] = [
    { id: "", label: "自动选择" },
    ...runtimes.map((runtime) => {
      const reason = runtime.revoked_at || runtime.deleted_at ? "runtime_revoked" : !runtime.enabled ? "runtime_disabled" : !runtime.online ? "runtime_offline" : !runtime.engines.includes(selectedAvatar?.engine || "") ? "engine_unavailable" : "";
      return { id: runtime.id, label: runtime.name, disabled: !!reason, hint: reason ? runtimeUnavailableLabels[reason] || "不可用" : "可用" };
    }),
  ];

  const composerTextareaRef = useRef<HTMLTextAreaElement>(null);
  const sendButtonRef = useRef<HTMLButtonElement>(null);
  const lastSendGestureAtRef = useRef(0);
  const submitTurnRef = useRef(submitTurn);
  submitTurnRef.current = submitTurn;
  draftRef.current = draft;
  const [blueprintChangeRequested, setBlueprintChangeRequested] = useState(false);
  const requestBlueprintChanges = useCallback(() => {
    setBlueprintChangeRequested(true);
    setDraft((current) => current.trim() ? current : "请修改方案：");
    requestAnimationFrame(() => {
      const element = composerTextareaRef.current;
      if (!element) return;
      element.focus();
      const length = element.value.length;
      element.setSelectionRange(length, length);
    });
  }, []);
  const resizeComposerTextarea = useCallback(() => {
    const element = composerTextareaRef.current;
    if (!element) return;
    element.style.height = "0px";
    element.style.overflowY = "hidden";
    const styles = window.getComputedStyle(element);
    const minHeight = Number.parseFloat(styles.minHeight) || 0;
    const maxHeight = Number.parseFloat(styles.maxHeight) || 220;
    const contentHeight = element.scrollHeight;
    element.style.height = `${Math.max(minHeight, Math.min(contentHeight, maxHeight))}px`;
    element.style.overflowY = contentHeight > maxHeight ? "auto" : "hidden";
  }, []);
  useLayoutEffect(() => { resizeComposerTextarea(); }, [draft, resizeComposerTextarea]);
  useEffect(() => {
    window.addEventListener("resize", resizeComposerTextarea);
    return () => window.removeEventListener("resize", resizeComposerTextarea);
  }, [resizeComposerTextarea]);
  function submitComposerGesture(event: ReactMouseEvent<HTMLButtonElement>) {
    attemptSubmitFromGesture(`react:${event.type}`, event);
  }

  function attemptSubmitFromGesture(source: string, event: { preventDefault(): void; type?: string }) {
    event.preventDefault();
    const now = Date.now();
    const draftLength = composerTextareaRef.current?.value.length || draftRef.current.length || draft.length;
    const blockedReason = !selectedProject ? "project_missing"
      : !selectedAvatar ? "avatar_missing"
        : uploadingAttachments ? "uploading"
          : conversationId && !exactTargetConversation ? "conversation_unresolved"
            : pendingRef.current?.state === "sending" ? "pending_sending"
              : "";
    writeSubmitDebug(source, {
      blockedReason,
      draftLength,
      ready: !blockedReason && (pendingRef.current?.state === "failed" || draftLength > 0),
    });
    if (now - lastSendGestureAtRef.current < 750) {
      writeSubmitDebug(`${source}:deduped`, { draftLength });
      return;
    }
    lastSendGestureAtRef.current = now;
    writeSubmitDebug(`${source}:calling_submit`, { draftLength });
    try {
      const submission = submitTurnRef.current(event as unknown as FormEvent);
      writeSubmitDebug(`${source}:submit_call_returned`, { draftLength });
      void submission.catch((error) => {
        const normalized = normalizeThrownError(error);
        writeSubmitDebug(`${source}:submit_error`, {
          code: normalized.code || "",
          error: apiErrorMessage(normalized),
          kind: normalized.kind,
          rawMessage: error instanceof Error ? error.message : String(error),
          rawName: error instanceof Error ? error.name : typeof error,
          status: normalized.status,
        });
      });
    } catch (error) {
      const normalized = normalizeThrownError(error);
      writeSubmitDebug(`${source}:submit_throw`, {
        code: normalized.code || "",
        error: apiErrorMessage(normalized),
        kind: normalized.kind,
        rawMessage: error instanceof Error ? error.message : String(error),
        rawName: error instanceof Error ? error.name : typeof error,
        status: normalized.status,
      });
    }
  }

  function writeSubmitDebug(source: string, extra: Record<string, unknown> = {}) {
    const node = sendButtonRef.current;
    if (!node) return;
    node.dataset.submitDebug = JSON.stringify({
      at: new Date().toISOString(),
      source,
      draftLength: composerTextareaRef.current?.value.length || draftRef.current.length || draft.length,
      hasProject: !!selectedProject,
      hasAvatar: !!selectedAvatar,
      pendingState: pendingRef.current?.state || "",
      ...extra,
    });
  }

  const previousUserCreatedAtByMessageID = useMemo(() => {
    const facts = new Map<string, string>();
    let previousUserCreatedAt = "";
    for (const message of messages) {
      if (message.role === "user") previousUserCreatedAt = message.created_at;
      if (message.role === "assistant" && previousUserCreatedAt) facts.set(message.id, previousUserCreatedAt);
    }
    return facts;
  }, [messages]);

  if (loading && !projects.length && !activeTeams.length) return <LoadingView label="正在读取对话工作区" />;

  const selectedTeamName = selectedTeam?.name
    || (selectedAvatar?.name === TEAM_ARCHITECT_AGENT_NAME ? "元团队" : selectedAvatar?.display_name || selectedAvatar?.name || "");
  const selectedProjectName = selectedProject ? projectDisplayName(selectedProject, teams, agents) : "选择项目";
  const threadTitle = selectedThreadSummary?.thread_title || selectedConversation?.thread_title || "未命名讨论";
  const showThreadEmpty = isSelectedThread && !loadingMessages && !messages.length && !pending;
  const showHero = !isSelectedThread && (!selectedProject || (!loadingMessages && !messages.length && !pending));
  const composerDisabled = isReadOnlyConversation || !selectedProject || !!(conversationId && !exactTargetConversation) || pending?.state === "sending";
  const conversationWorkspaceStyle = { "--deliverable-panel-width": `${deliverablePanelWidth}px` } as CSSProperties;
  const latestBlueprintMessageByRunID = new Map<string, string>();
  messages.forEach((message) => {
    if (message.role !== "assistant" || typeof message.metadata !== "object" || message.metadata === null) return;
    const metadata = message.metadata as AssistantMessageMetadata;
    if (metadata.blueprint_build_run_id) latestBlueprintMessageByRunID.set(metadata.blueprint_build_run_id, message.id);
  });
  return <div ref={conversationWorkspaceRef} className={`conversation-workspace${deliverablesOpen ? " conversation-workspace--deliverables-open" : ""}`} style={conversationWorkspaceStyle}>
    <section className={showHero ? "timeline-pane timeline-pane--hero" : "timeline-pane"}>
      <div ref={timelineRef} className={showThreadEmpty ? "timeline timeline--thread-empty" : "timeline"}>
        {isReadOnlyHistory && <section className="conversation-readonly-banner" role="status"><LockKeyhole size={16} aria-hidden="true" /><p>此会话属于迁移前的旧团队负责人，已转为只读；新消息请在新会话中与当前团队继续。</p><Button size="small" onClick={() => navigate(`/project/${encodeURIComponent(projectId)}/conversations`)}><MessageSquarePlus size={16} />开始新会话</Button></section>}
        {!isReadOnlyHistory && isReadOnlySharedConversation && <section className="conversation-readonly-banner" role="status"><LockKeyhole size={16} aria-hidden="true" /><p>此会话由其他成员创建。你可以查看历史并标记自己的已读，但不能继续写入或创建讨论。</p><Button size="small" onClick={() => navigate(`/project/${encodeURIComponent(projectId)}/conversations`)}><MessageSquarePlus size={16} />开始新会话</Button></section>}
        {isSelectedThread && <section className="thread-context-bar" aria-label="当前讨论">{threadRoot && <Button variant="ghost" size="small" aria-label="返回主会话" onClick={() => navigateToConversation(threadRoot.id)}><ArrowLeft size={16} />主会话</Button>}<div className="thread-context-bar__title"><span>围绕此消息讨论</span><MarkdownText text={threadTitle} /></div><small>{selectedThreadSummary?.reply_count ?? 0} 条回复</small></section>}
        {threadError && <ErrorNotice message={threadError} onRetry={threadRootId ? () => void refreshThreads(threadRootId) : undefined} />}
        {loadError && <ErrorNotice message={loadError} onRetry={conversationId ? () => void reloadPersisted(conversationId) : undefined} />}
        {runtimeError && <ErrorNotice message={runtimeError} />}
        {teamNavigationError && <ErrorNotice message={teamNavigationError} />}
        {publicationError && <ErrorNotice message={publicationError} />}
        {!selectedProject ? <div className="conversation-hero">
          <Users size={28} aria-hidden="true" />
          <h1>选择团队开始对话</h1>
          <p>团队可以承接多个项目；每个新会话保持独立上下文。</p>
          {activeTeams.length > 0 ? <>
            <div className="conversation-team-picker" aria-label="选择团队">
              {activeTeams.map((team) => {
                const lead = agents.find((agent) => agent.id === team.lead_avatar_id);
                return <Button key={team.id} variant="secondary" loading={teamNavigationBusy === team.id} disabled={!!teamNavigationBusy && teamNavigationBusy !== team.id} onClick={() => openTeam(team.id)}><Users size={16} aria-hidden="true" />{team.name}{lead && <small>{lead.display_name || lead.name}</small>}</Button>;
              })}
            </div>
            <Button variant="ghost" size="small" loading={teamNavigationBusy === TEAM_ARCHITECT_AGENT_NAME} disabled={!!teamNavigationBusy && teamNavigationBusy !== TEAM_ARCHITECT_AGENT_NAME} onClick={openTeamArchitect}><UserPlus size={16} aria-hidden="true" />新增团队</Button>
          </> : <>
            <p>还没有可用团队。先告诉元团队你想解决什么问题，它会帮助你设计、评测并发布团队。</p>
            <Button variant="primary" size="small" loading={teamNavigationBusy === TEAM_ARCHITECT_AGENT_NAME} onClick={openTeamArchitect}><UserPlus size={16} aria-hidden="true" />创建第一个团队</Button>
          </>}
        </div> : loadingMessages ? <LoadingView label="正在读取消息" /> : !messages.length && !pending ? isSelectedThread ? <div className="thread-empty-state"><MessagesSquare size={24} aria-hidden="true" /><h2>围绕此消息继续讨论</h2><MarkdownText text={threadTitle} /></div> : <div className="conversation-hero"><Bot size={28} aria-hidden="true" /><h1>想要做什么？</h1><p>直接描述任务，由当前团队受理并给出结果。</p></div> : <div className="message-list">{messages.map((message) => {
          const metadata = message.role === "assistant" && typeof message.metadata === "object" && message.metadata !== null ? message.metadata as AssistantMessageMetadata : {};
          const messageBuildRunID = metadata.blueprint_build_run_id;
          const matchedBuildRun = selectedConversation?.build_run && messageBuildRunID === selectedConversation.build_run.build_run_id ? selectedConversation.build_run : undefined;
          const latestBlueprintMessageID = messageBuildRunID ? latestBlueprintMessageByRunID.get(messageBuildRunID) : undefined;
          const isLatestBlueprintMessage = !!messageBuildRunID && latestBlueprintMessageID === message.id;
			return <div className="message-with-progress" key={message.id} data-scroll-anchor={`message:${message.id}`}>
            <ConversationMessage
              message={message}
              previousUserCreatedAt={previousUserCreatedAtByMessageID.get(message.id)}
              canCreateThread={!isSelectedThread && !!selectedConversation && !selectedConversation.read_only}
              threadBusy={!!creatingThreadMessageId}
              creatingThread={creatingThreadMessageId === message.id}
              promoted={!!promotedMessageIds[message.id]}
              promoteBusy={!!promotingMessageId}
              promoting={promotingMessageId === message.id}
              promoteError={promoteErrors[message.id]}
              teamId={selectedProject?.team_id}
              blueprintBuildRun={matchedBuildRun}
              blueprintCardMode={matchedBuildRun && !isLatestBlueprintMessage ? "reference" : "full"}
              buildProgressInvalidationVersion={invalidationVersion}
              onOpenTeam={openTeam}
              onCreateThread={(messageId) => void createThread(messageId)}
              onPromote={(messageId) => void promoteDeliverable(messageId)}
              onSubmitBuildRun={(buildRunId, token) => void submitBuildRunFromCard(buildRunId, token)}
              onRequestBlueprintChanges={requestBlueprintChanges}
              onReplanBuildRun={() => void submitTurn(undefined, "按修正方案重新规划")}
              onAbandonBuildRun={() => void submitTurn(undefined, "放弃此次构建，不再重新规划。")}
            />
          </div>;
		})}{pending && <div ref={pendingTurnRef} className="pending-turn" data-scroll-anchor={`pending:${pending.clientRequestId}`}><PendingTurnMessages
          pending={pending}
          teamId={selectedProject?.team_id}
          hideUserMessage={!!pending.persistedUserMessageId && messages.some((message) => message.id === pending.persistedUserMessageId)}
          onRestoreDraft={() => {
            clearPending();
            setDraft(pending.content);
            setSelectedAttachments([...(pending.request.attachment_ids || [])]);
            setSelectedRuntimeId(pending.request.runtime_id || "");
          }}
          onRetry={() => pending.interrupted ? void reconnectPendingTurn(pending) : void submitTurn()}
		/></div>}</div>}
      </div>

      <form id="composer" className="composer" onSubmit={(event) => void submitTurn(event)}>
        {pending?.runtimeAssignment && <div className="composer-runtime-assignment"><RuntimeAssignmentNotice assignment={pending.runtimeAssignment} /></div>}
        <Card className="composer-card" padding="compact" footer={<div className="composer__footer">
          <div className="composer__footer-context">
            <Button variant="ghost" size="small" aria-label="上传文件" title="上传文件" disabled={composerDisabled || uploadingAttachments} onClick={() => attachmentInputRef.current?.click()}>
              {uploadingAttachments ? <LoaderCircle className="spin" size={16} aria-hidden="true" /> : <Plus size={16} aria-hidden="true" />}
            </Button>
            <input ref={attachmentInputRef} className="sr-only" type="file" multiple aria-label="选择要上传的文件" disabled={composerDisabled || uploadingAttachments} onChange={(event) => void uploadComposerAttachments(event)} />
            {selectableAttachmentFacts.length > 0 && <details className="composer-attachment-menu">
              <summary className="composer-chip__trigger" aria-disabled={composerDisabled || undefined} onClick={(event) => { if (composerDisabled) event.preventDefault(); }}><Paperclip size={14} aria-hidden="true" /><span>附件{selectedAttachments.length > 0 ? ` ${selectedAttachments.length}` : ""}</span><ChevronDown size={14} aria-hidden="true" /></summary>
              <div className="composer-attachment-menu__items" role="group" aria-label="选择附件">
                {selectableAttachmentFacts.map((attachment) => <label className="composer-attachment-menu__item" key={attachment.id}><input type="checkbox" checked={selectedAttachments.includes(attachment.id)} disabled={composerDisabled} onChange={(event) => setSelectedAttachments((current) => event.target.checked ? Array.from(new Set([...current, attachment.id])) : current.filter((id) => id !== attachment.id))} /><span>{attachment.filename}</span></label>)}
              </div>
            </details>}
            <ChipMenu ariaLabel="团队" icon={<Users size={14} aria-hidden="true" />} current={selectedTeamName || "选择团队"} emptyLabel="暂无可用团队" items={teamChipItems} selectedId={selectedTeam?.id} onSelect={openTeam} />
            <ChipMenu ariaLabel="项目" current={selectedProjectName} emptyLabel="暂无可用项目" items={projectChipItems} selectedId={projectId} onSelect={(id) => navigate(`/project/${encodeURIComponent(id)}/conversations`)} />
            {isCLIEngine && <ChipMenu ariaLabel="运行时" current={runtimes.find((runtime) => runtime.id === selectedRuntimeId)?.name || "自动选择"} emptyLabel="暂无可用 Runtime" items={runtimeChipItems} selectedId={selectedRuntimeId} disabled={isReadOnlyConversation || pending?.state === "sending"} onSelect={setSelectedRuntimeId} />}
          </div>
          {pending?.state === "sending" ? <Button variant="primary" size="small" aria-label="停止等待" title="停止等待" onClick={() => abortRef.current?.abort()}><Square size={14} /></Button> : <Button ref={sendButtonRef} variant="primary" size="small" type="button" aria-label="发送（Enter 发送，Shift+Enter 换行）" title="发送（Enter 发送，Shift+Enter 换行）" disabled={uploadingAttachments || isReadOnlyConversation || !selectedProject || !selectedAvatar || !!(conversationId && !exactTargetConversation) || (!(pending?.state === "failed") && !draft.trim())} data-submit-ready={(!uploadingAttachments && !isReadOnlyConversation && !!selectedProject && !!selectedAvatar && !(conversationId && !exactTargetConversation) && (pending?.state === "failed" || !!draft.trim())) ? "true" : "false"} data-draft-length={draft.length} onClick={submitComposerGesture}><Send size={16} /></Button>}
        </div>}>
          <label className="composer-message"><textarea ref={composerTextareaRef} aria-label="消息" placeholder={isReadOnlyConversation ? "此会话为只读" : isSelectedThread ? "回复此讨论" : selectedProject ? "描述任务，回车发送" : "先从左侧选择一个团队或项目"} value={draft} disabled={composerDisabled} onChange={(event) => setDraft(event.target.value)} onKeyDown={(event) => { if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); void submitTurn(); } }} /></label>
          <AttachmentChips attachments={selectedAttachmentFacts} onRemove={(id) => setSelectedAttachments((current) => current.filter((attachmentID) => attachmentID !== id))} />
          {attachmentUploadError && <p className="composer-upload-error" role="alert">{attachmentUploadError}</p>}
        </Card>
      </form>
    </section>
    {deliverablesOpen && <div className={`deliverable-side-panel__resizer${deliverablePanelDragging ? " is-dragging" : ""}`} role="separator" aria-label="调整交付物侧栏宽度" aria-orientation="vertical" aria-valuemin={MIN_DELIVERABLE_PANEL_WIDTH} aria-valuemax={Math.round(deliverablePanelMaximum(conversationWorkspaceRef.current))} aria-valuenow={Math.round(deliverablePanelWidth)} tabIndex={0} onKeyDown={resizeDeliverablePanelWithKeyboard} onPointerDown={startDeliverablePanelDrag} onPointerMove={dragDeliverablePanel} onPointerUp={stopDeliverablePanelDrag} onPointerCancel={stopDeliverablePanelDrag} />}
    {deliverablesOpen && <aside className="deliverable-side-panel" aria-label="交付物">
      <div className="deliverable-side-panel__body"><DeliverableBody state={deliverables} projectId={projectId} conversationId={conversationId} previewOpen onImprove={() => setDeliverablesOpen(false)} /></div>
    </aside>}
  </div>;
}

function AttachmentChips({ attachments, onRemove }: { attachments: MessageAttachment[]; onRemove?: (id: string) => void }) {
  if (attachments.length === 0) return null;
  return <ul className={onRemove ? "composer-attachments" : "message-attachments"} aria-label={onRemove ? "本轮已选附件" : "消息附件"}>
    {attachments.map((attachment) => <li className={onRemove ? "composer-attachment" : "message-attachment"} key={attachment.id}>
      <Paperclip size={14} aria-hidden="true" />
      <span>{attachment.filename}</span>
      {onRemove && <button type="button" aria-label={`移除附件 ${attachment.filename}`} title="移除附件" onClick={() => onRemove(attachment.id)}><X size={14} aria-hidden="true" /></button>}
    </li>)}
  </ul>;
}

interface ChipMenuItem {
  id: string;
  label: string;
  hint?: string;
  disabled?: boolean;
}

interface ChipMenuProps {
  ariaLabel: string;
  current: string;
  emptyLabel: string;
  items: ChipMenuItem[];
  selectedId?: string;
  disabled?: boolean;
  icon?: ReactNode;
  onSelect: (id: string) => void;
}

/** composer 上沿 chip：button + 绝对定位浮层，Escape / 点击外部关闭，零新依赖。 */
function ChipMenu({ ariaLabel, current, emptyLabel, items, selectedId, disabled, icon, onSelect }: ChipMenuProps) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLSpanElement>(null);
  useEffect(() => {
    if (!open) return;
    const handlePointerDown = (event: PointerEvent) => {
      if (rootRef.current && event.target instanceof Node && !rootRef.current.contains(event.target)) setOpen(false);
    };
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") setOpen(false);
    };
    document.addEventListener("pointerdown", handlePointerDown);
    document.addEventListener("keydown", handleKeyDown);
    return () => {
      document.removeEventListener("pointerdown", handlePointerDown);
      document.removeEventListener("keydown", handleKeyDown);
    };
  }, [open]);
  return <span className="composer-chip" ref={rootRef}>
    <button type="button" className="composer-chip__trigger" aria-label={`${ariaLabel}：${current}`} aria-haspopup="menu" aria-expanded={open} disabled={disabled} onClick={() => setOpen((value) => !value)}>
      {icon}
      <span>{current}</span>
      <ChevronDown size={14} aria-hidden="true" />
    </button>
    {open && <span className="composer-chip__menu" role="menu" aria-label={ariaLabel}>
      {items.length === 0 && <span className="composer-chip__empty">{emptyLabel}</span>}
      {items.map((item) => <button type="button" role="menuitem" className="composer-chip__item" key={item.id || "(auto)"} disabled={item.disabled} aria-current={item.id === selectedId ? true : undefined} onClick={() => { setOpen(false); onSelect(item.id); }}>
        <span>{item.label}</span>
        {item.hint && <small>{item.hint}</small>}
      </button>)}
    </span>}
  </span>;
}

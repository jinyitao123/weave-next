import { Navigate, Route, Routes } from "react-router-dom";
import { ControlCenterRoutes } from "./pages/ControlCenterPage";
import { useAuth } from "./auth/AuthContext";
import { LoginPage } from "./auth/LoginPage";
import { ActivityPage } from "./pages/ActivityPage";
import { InboxPage } from "./pages/InboxPage";
import { LegacyProjectRedirect } from "./pages/LegacyProjectRedirect";
import { RedirectConversation } from "./pages/RedirectConversation";
import { ScheduledPage } from "./pages/ScheduledPage";
import { ProjectContainer } from "./pages/project/ProjectContainer";
import { AppShell } from "./shell/AppShell";
import { LoadingView } from "./ui/StatusViews";
import { WorkspaceProvider } from "./workspace/WorkspaceContext";

export function App() {
  const { status } = useAuth();
  if (status === "restoring") return <main className="boot-screen"><LoadingView label="正在恢复会话" /></main>;
  if (status === "anonymous") return <LoginPage />;

  return (
    <WorkspaceProvider>
      <Routes>
        <Route path="/control/*" element={<ControlCenterRoutes />} />
        <Route element={<AppShell />}>
          <Route index element={<Navigate to="/inbox" replace />} />
          <Route path="/projects" element={<Navigate to="/inbox" replace />} />
          <Route path="/project-data" element={<LegacyProjectRedirect tab="runs" />} />
          <Route path="/project-execution" element={<LegacyProjectRedirect tab="conversations" />} />
          <Route path="/conversation" element={<RedirectConversation />} />
          <Route path="/project/:projectId/conversations" element={<ProjectContainer />} />
          <Route path="/project/:projectId/conversations/:conversationId" element={<ProjectContainer />} />
          <Route path="/project/:projectId/team" element={<ProjectContainer />} />
          <Route path="/project/:projectId/runs" element={<ProjectContainer />} />
          <Route path="/project/:projectId/deliverables" element={<ProjectContainer />} />
          <Route path="/inbox" element={<InboxPage />} />
          <Route path="/activity" element={<ActivityPage />} />
          <Route path="/runtimes" element={<Navigate to="/control/runtimes" replace />} />
          <Route path="/scheduled" element={<ScheduledPage />} />
          <Route path="*" element={<Navigate to="/inbox" replace />} />
        </Route>
      </Routes>
    </WorkspaceProvider>
  );
}

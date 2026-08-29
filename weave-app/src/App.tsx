import { useAuth } from "./auth/useAuth";
import { LoginPage } from "./auth/LoginPage";
import { RuntimesPage } from "./pages/RuntimesPage";
import { LoadingView } from "./ui/StatusViews";

export function App() {
  const { status } = useAuth();
  if (status === "restoring") return <main className="boot-screen"><LoadingView label="正在恢复会话" /></main>;
  if (status === "anonymous") return <LoginPage />;

  return <RuntimesPage />;
}

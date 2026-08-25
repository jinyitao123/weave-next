import { useState, type FormEvent } from "react";
import { ArrowRight, Layers3 } from "lucide-react";
import { apiErrorMessage } from "../api";
import { useAuth } from "./useAuth";
import { Button } from "../ui/Button";
import { Field } from "../ui/Field";
import { ErrorNotice } from "../ui/StatusViews";

export function LoginPage() {
  const { devLogin, login } = useAuth();
  const [tenant, setTenant] = useState("default");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [devSubmitting, setDevSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setSubmitting(true);
    setError(null);
    try {
      await login({ tenant, username, password });
    } catch (requestError) {
      setError(apiErrorMessage(requestError));
    } finally {
      setSubmitting(false);
    }
  }

  async function submitDevLogin() {
    setDevSubmitting(true);
    setError(null);
    try {
      await devLogin(tenant);
    } catch (requestError) {
      setError(apiErrorMessage(requestError));
    } finally {
      setDevSubmitting(false);
    }
  }

  return (
    <main className="login-page">
      <section className="login-panel" aria-labelledby="login-title">
        <div className="brand-mark"><Layers3 size={20} /></div>
        <p className="eyebrow">Conversation OS</p>
        <h1 id="login-title">进入 Weave</h1>
        <p className="login-intro">使用工作区账户继续。浏览器会话只保存在当前标签页会话中。</p>
        {error && <ErrorNotice message={error} />}
        <form className="form-stack" onSubmit={submit}>
          <Field label="工作区">{(control) => <input {...control} value={tenant} onChange={(event) => setTenant(event.target.value)} required autoComplete="organization" />}</Field>
          <Field label="用户名">{(control) => <input {...control} value={username} onChange={(event) => setUsername(event.target.value)} required autoComplete="username" autoFocus />}</Field>
          <Field label="密码">{(control) => <input {...control} type="password" value={password} onChange={(event) => setPassword(event.target.value)} required autoComplete="current-password" />}</Field>
          <Button variant="primary" type="submit" loading={submitting} disabled={!tenant || !username || !password}>
            {submitting ? "正在验证…" : <>登录 <ArrowRight size={16} /></>}
          </Button>
        </form>
        <div className="login-dev-entry">
          <Button type="button" variant="ghost" loading={devSubmitting} disabled={!tenant || submitting} onClick={() => void submitDevLogin()}>
            {devSubmitting ? "正在进入…" : "开发模式进入"}
          </Button>
        </div>
      </section>
      <aside className="login-context" aria-label="产品说明">
        <span>一个工作区</span>
        <strong>对话驱动工作，证据保持可见。</strong>
        <p>登录后可查看后端持久化的工作区数据。</p>
      </aside>
    </main>
  );
}

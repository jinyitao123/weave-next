import { useState, type FormEvent } from "react";
import { ArrowRight, Layers3 } from "lucide-react";
import { apiErrorMessage } from "../api";
import { useAuth } from "./useAuth";
import { Button } from "../ui/Button";
import { Field } from "../ui/Field";
import { ErrorNotice } from "../ui/StatusViews";

export function LoginPage() {
  const { login } = useAuth();
  const [tenant, setTenant] = useState("default");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [submitting, setSubmitting] = useState(false);
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

  return (
    <main className="login-page">
      <section className="login-panel" aria-labelledby="login-title">
        <div className="brand-mark"><Layers3 size={20} /></div>
        <p className="eyebrow">Weave Runtime</p>
        <h1 id="login-title">进入运行时管理台</h1>
        <p className="login-intro">使用工作区账户管理分布在不同机器上的运行节点。</p>
        {error && <ErrorNotice message={error} />}
        <form className="form-stack" onSubmit={submit}>
          <Field label="工作区">{(control) => <input {...control} value={tenant} onChange={(event) => setTenant(event.target.value)} required autoComplete="organization" />}</Field>
          <Field label="用户名">{(control) => <input {...control} value={username} onChange={(event) => setUsername(event.target.value)} required autoComplete="username" autoFocus />}</Field>
          <Field label="密码">{(control) => <input {...control} type="password" value={password} onChange={(event) => setPassword(event.target.value)} required autoComplete="current-password" />}</Field>
          <Button variant="primary" type="submit" loading={submitting} disabled={!tenant || !username || !password}>
            {submitting ? "正在验证…" : <>登录 <ArrowRight size={16} /></>}
          </Button>
        </form>
      </section>
      <aside className="login-context" aria-label="产品说明">
        <span>多运行时</span>
        <strong>让任务在合适的机器上执行。</strong>
        <p>任务协作在 Workbench 中进行；此处只管理运行节点、容量和连接状态。</p>
      </aside>
    </main>
  );
}

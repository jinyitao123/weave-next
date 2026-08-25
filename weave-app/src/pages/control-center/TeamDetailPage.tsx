import { useCallback, useEffect, useState } from "react";
import { ArrowLeft, ChevronLeft, ChevronRight, History } from "lucide-react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api, apiErrorMessage, normalizeThrownError, type TeamRoster, type TeamRunListResponse, type TeamRunRow } from "../../api";
import { Badge } from "../../ui/Badge";
import { Button } from "../../ui/Button";
import { ErrorNotice, LoadingView } from "../../ui/StatusViews";
import { TeamDetailPanel } from "./TeamsPage";
import { WorkflowsPage } from "./WorkflowsPage";

type TeamDetailTab = "roster" | "dispatch" | "workflows" | "runs";

const tabs: Array<[TeamDetailTab, string]> = [
  ["roster", "成员与授权"],
  ["dispatch", "自由派工"],
  ["workflows", "协作流程"],
  ["runs", "运行记录"],
];
const dateTime = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" });
const count = new Intl.NumberFormat("zh-CN");
const money = new Intl.NumberFormat("zh-CN", { style: "currency", currency: "USD", maximumFractionDigits: 4 });
const runPageSize = 20;

function runStatusLabel(status?: string) {
  if (!status) return "状态未知";
  return ({ completed: "已完成", failed: "失败", yielded: "等待输入", running: "进行中", cancelled: "已取消", timed_out: "超时" } as Record<string, string>)[status.toLowerCase()] || "状态未知";
}

function runStatusTone(status?: string): "neutral" | "success" | "warning" | "danger" {
  const normalized = status?.toLowerCase();
  if (normalized === "completed") return "success";
  if (normalized === "failed") return "danger";
  if (normalized === "yielded" || normalized === "running") return "warning";
  return "neutral";
}

function formatDate(value?: string) {
  return value ? dateTime.format(new Date(value)) : "—";
}

function formatDuration(value?: number) {
  if (value === undefined) return "—";
  if (value < 1000) return `${value} 毫秒`;
  return `${(value / 1000).toFixed(1)} 秒`;
}

export function TeamDetailPage() {
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const [tab, setTab] = useState<TeamDetailTab>("roster");
  const [team, setTeam] = useState<TeamRoster | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const loadTeam = useCallback(async () => {
    if (!id) {
      setError("缺少团队 ID。");
      setLoading(false);
      return;
    }
    setLoading(true);
    setError(null);
    try {
      setTeam(await api.getTeam(id));
    } catch (requestError) {
      setTeam(null);
      setError(apiErrorMessage(normalizeThrownError(requestError)));
    } finally {
      setLoading(false);
    }
  }, [id]);

  useEffect(() => { void loadTeam(); }, [loadTeam]);

  return <section className="control-content team-detail-page" aria-labelledby="team-detail-heading">
    <Link className="team-detail-page__return" to="/control/teams"><ArrowLeft size={16} aria-hidden="true" />返回团队名册</Link>
    <header className="team-detail-page__header">
      <div>
        <h2 id="team-detail-heading">{team?.team.name || "团队详情"}</h2>
        <p>{team?.team.objective || (loading ? "正在加载团队信息…" : id)}</p>
      </div>
    </header>
    <div className="team-detail-page__tabs" role="tablist" aria-label="团队详情分区">
      {tabs.map(([value, label]) => <button
        key={value}
        className={tab === value ? "active" : ""}
        type="button"
        role="tab"
        aria-selected={tab === value}
        onClick={() => setTab(value)}
      >{label}</button>)}
    </div>
    {loading ? <LoadingView label="正在加载团队详情" /> : error || !team ? <ErrorNotice message={error || "团队详情未就绪。"} onRetry={() => void loadTeam()} /> : <div className="team-detail-page__body">
      {tab === "roster" && <TeamDetailPanel teamID={id} sections="roster" onArchived={() => navigate("/control/teams")} />}
      {tab === "dispatch" && <TeamDetailPanel teamID={id} sections="dispatch" />}
      {tab === "workflows" && <WorkflowsPage key={id} fixedTeamID={id} />}
      {tab === "runs" && <TeamRunsPanel key={id} teamID={id} />}
    </div>}
  </section>;
}

function TeamRunsPanel({ teamID }: { teamID: string }) {
  const [offset, setOffset] = useState(0);
  const [response, setResponse] = useState<TeamRunListResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const loadRuns = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setResponse(await api.listTeamRuns(teamID, { limit: runPageSize, offset }));
    } catch (requestError) {
      setError(apiErrorMessage(normalizeThrownError(requestError)));
    } finally {
      setLoading(false);
    }
  }, [offset, teamID]);

  useEffect(() => { void loadRuns(); }, [loadRuns]);

  if (loading && !response) return <LoadingView label="正在加载团队运行记录" />;
  if (error && !response) return <ErrorNotice message={error} onRetry={() => void loadRuns()} />;
  if (!response?.runs.length && !error) return <div className="empty-surface team-runs-empty"><History size={28} aria-hidden="true" /><h2>该团队还没有运行记录</h2><p>团队产生运行后，记录会显示在这里。</p></div>;

  const totals = response?.totals;
  const page = Math.floor(offset / runPageSize) + 1;
  return <section className="team-runs" aria-labelledby="team-runs-heading">
    <header className="team-runs__heading">
      <div><h3 id="team-runs-heading">运行记录</h3><p>按团队归属查看运行与独占用量。</p></div>
      {response && !response.diagnostics.aggregation_complete && <Badge tone="warning">聚合未完成</Badge>}
    </header>
    {error && <ErrorNotice message={error} onRetry={() => void loadRuns()} />}
    <dl className="team-runs__totals">
      <div><dt>运行总数</dt><dd>{count.format(response?.total || 0)}</dd></div>
      <div><dt>输入 Tokens</dt><dd>{totals ? count.format(totals.input_tokens) : "待聚合"}</dd></div>
      <div><dt>输出 Tokens</dt><dd>{totals ? count.format(totals.output_tokens) : "待聚合"}</dd></div>
      <div><dt>成本</dt><dd>{totals ? money.format(totals.cost_usd) : "待聚合"}</dd></div>
    </dl>
    <div className="team-runs__table" role="table" aria-label="团队运行记录" aria-busy={loading}>
      <div className="team-runs__row team-runs__row--header" role="row">
        <span role="columnheader">运行</span><span role="columnheader">执行者</span><span role="columnheader">状态</span><span role="columnheader">开始时间</span><span role="columnheader">耗时</span><span role="columnheader">独占用量</span>
      </div>
      {response?.runs.map((run) => <TeamRunListRow key={run.run_id} run={run} />)}
    </div>
    <nav className="team-runs__pagination" aria-label="团队运行记录分页">
      <Button disabled={loading || offset === 0} onClick={() => setOffset(Math.max(0, offset - runPageSize))}><ChevronLeft size={16} aria-hidden="true" />上一页</Button>
      <span>第 {page} 页</span>
      <Button disabled={loading || !response || offset + runPageSize >= response.total} onClick={() => setOffset(offset + runPageSize)}>下一页<ChevronRight size={16} aria-hidden="true" /></Button>
    </nav>
  </section>;
}

function TeamRunListRow({ run }: { run: TeamRunRow }) {
  const usage = run.terminal?.self_exclusive;
  return <div className="team-runs__row" role="row">
    <span role="cell" data-label="运行"><Link className="text-link" to={`/activity?kind=run&id=${encodeURIComponent(run.run_id)}`}>查看运行</Link></span>
    <span role="cell" data-label="Agent">{run.terminal?.agent || "—"}</span>
    <span role="cell" data-label="状态"><Badge tone={runStatusTone(run.terminal?.status)}>{runStatusLabel(run.terminal?.status)}</Badge></span>
    <time role="cell" data-label="开始时间" dateTime={run.terminal?.started_at}>{formatDate(run.terminal?.started_at)}</time>
    <span role="cell" data-label="耗时">{formatDuration(run.terminal?.duration_ms)}</span>
    <span role="cell" data-label="独占用量">{usage ? `输入 ${count.format(usage.input_tokens)} · 输出 ${count.format(usage.output_tokens)}` : "—"}</span>
  </div>;
}

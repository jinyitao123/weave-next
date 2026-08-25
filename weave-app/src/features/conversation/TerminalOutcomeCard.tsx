import type { TerminalOutcome } from "../../api";
import { Button } from "../../ui/Button";
import { terminalOutcomePresentation } from "./terminalOutcome";

interface TerminalOutcomeCardProps {
  outcome: TerminalOutcome;
  onReplan?(): void;
  onAbandon?(): void;
}

export function TerminalOutcomeCard({ outcome, onReplan, onAbandon }: TerminalOutcomeCardProps) {
  const presentation = terminalOutcomePresentation(outcome);
  return <section className={`terminal-outcome-card terminal-outcome-card--${outcome.turn_state}`} aria-label="本轮结论">
    <header>
      <div>
        <small>本轮结论</small>
        <strong>{presentation.title}</strong>
        {presentation.modeDescription && <span className="terminal-outcome-card__mode-note">{presentation.modeDescription}</span>}
      </div>
    </header>
    <p>{presentation.description}</p>
    {!outcome.report && outcome.agent_summary && <aside className="terminal-outcome-card__agent-summary">
      <strong>执行者说明</strong>
      <p>{outcome.agent_summary}</p>
    </aside>}
    {presentation.actions.length > 0 && <footer>
      {presentation.actions.includes("abandon") && onAbandon && <Button variant="ghost" size="small" onClick={onAbandon}>放弃此次构建</Button>}
      {presentation.actions.includes("replan") && onReplan && <Button variant="primary" size="small" onClick={onReplan}>重新规划</Button>}
    </footer>}
  </section>;
}

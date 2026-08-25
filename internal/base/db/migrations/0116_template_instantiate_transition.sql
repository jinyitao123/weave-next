ALTER TABLE weave_team_build_run_transitions
  DROP CONSTRAINT weave_team_build_run_transitions_allowed_check;

-- template_instantiate 策略无 publishing 阶段：round_running 直达 passed。
-- 策略约束由 Go 状态机（MarkTemplateInstantiated 的 WHERE execution_strategy='template_instantiate'）强制。
ALTER TABLE weave_team_build_run_transitions
  ADD CONSTRAINT weave_team_build_run_transitions_allowed_check CHECK (
    (from_status IS NULL AND to_status = 'planning')
    OR (
      from_status = 'planning'
      AND to_status IN ('authorized', 'blocked', 'cancelled')
    )
    OR (
      from_status = 'authorized'
      AND to_status IN ('round_running', 'blocked', 'cancelled')
    )
    OR (
      from_status = 'round_running'
      AND to_status IN ('authorized', 'publishing', 'passed', 'blocked', 'cancelled')
    )
    OR (
      from_status = 'publishing'
      AND to_status IN ('passed', 'blocked', 'cancelled')
    )
  );

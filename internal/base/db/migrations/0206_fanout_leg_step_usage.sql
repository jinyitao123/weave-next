-- What one finished step of a parallel branch spent: the model attempts it
-- confirmed and why any of its usage is unknown. The parent run adds it to its
-- own usage when the group joins. Steps finished before this column existed
-- keep NULL, which the join counts as unknown usage, never as zero.
ALTER TABLE weave_fanout_leg_steps ADD COLUMN IF NOT EXISTS usage JSONB
  CHECK (usage IS NULL OR jsonb_typeof(usage) = 'object');

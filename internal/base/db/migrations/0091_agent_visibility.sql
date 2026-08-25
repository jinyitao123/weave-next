-- IA-T02: agents gain a visibility marker so the console can filter
-- platform-built-in assets (the "__" prefixed meta-team avatars/workers) out
-- of team trees and avatar pickers while exact-name invocation stays
-- available.

-- 1) Add the authoritative visibility column; new agents default to public.
ALTER TABLE weave_agents ADD COLUMN visibility text NOT NULL DEFAULT 'public';

-- 2) Restrict visibility to the supported marker set (0033 role CHECK style).
ALTER TABLE weave_agents ADD CONSTRAINT weave_agents_visibility_check
  CHECK (visibility IN ('public', 'internal_tool', 'platform'));

-- 3) Backfill platform-built-in assets: every existing "__"-prefixed agent is
-- a platform asset.
UPDATE weave_agents SET visibility = 'platform' WHERE name LIKE '\_\_%';

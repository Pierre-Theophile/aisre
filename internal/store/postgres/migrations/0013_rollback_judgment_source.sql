-- SPDX-License-Identifier: Apache-2.0
--
-- A fifth judgment source: `rollback` (004 T155).
--
-- A platform that states a rollback away from a deployment — Vercel's `lastAliasRequest` — records an
-- operator's judgement, made during the incident, that the deployment is the problem. The investigation
-- engine credits the hypothesis naming that deployment's change with it. It is neither the deterministic
-- first wave nor a fact a person typed in, and a ledger read back later must be able to say which of its
-- confidences rested on an operator's action, so it gets its own source rather than borrowing one.
--
-- Only the CHECK widens. No row changes, and the judgments table stays append-only (check-migrations.sh).

ALTER TABLE investigation.judgments DROP CONSTRAINT IF EXISTS judgments_source_check;
ALTER TABLE investigation.judgments ADD CONSTRAINT judgments_source_check
	CHECK (source IN ('first_wave', 'model', 'human_fact', 'exoneration', 'rollback'));

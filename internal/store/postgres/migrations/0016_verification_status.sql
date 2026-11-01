-- SPDX-License-Identifier: Apache-2.0
--
-- Verification on every terminal path (002 FR-022c, ADR-0019 D1, T121).
--
-- The checker and the fresh-context verifier existed and ran on nothing. From this migration on,
-- every concluded investigation says whether its claims were verified, and why not when they were
-- not: `verified` or `not_verified`, with one sentence of detail. A run concluded before this
-- migration has neither column set, which reads as "concluded before verification was wired",
-- and nothing back-fills it: a concluded row is immutable (FR-007).
--
-- `investigation.verifier_findings` was created in 0006 and never written. Its verdict CHECK
-- omitted `uncited`, the verdict only the deterministic checker can give (a claim citing no
-- evidence item at all); the CHECK widens to admit it. No row changes.

ALTER TABLE investigation.investigations
	ADD COLUMN IF NOT EXISTS verification_status text,
	ADD COLUMN IF NOT EXISTS verification_detail text;

ALTER TABLE investigation.investigations DROP CONSTRAINT IF EXISTS investigations_verification_status_check;
ALTER TABLE investigation.investigations ADD CONSTRAINT investigations_verification_status_check
	CHECK (verification_status IS NULL OR verification_status IN ('verified', 'not_verified'));

COMMENT ON COLUMN investigation.investigations.verification_status IS
	'FR-022c: whether the checker and the verifier ran over the run''s claims; NULL on runs concluded before 0016.';
COMMENT ON COLUMN investigation.investigations.verification_detail IS
	'FR-022c: the counts of kept, demoted and removed claims, or why the claims were not verified.';

ALTER TABLE investigation.verifier_findings DROP CONSTRAINT IF EXISTS verifier_findings_verdict_check;
ALTER TABLE investigation.verifier_findings ADD CONSTRAINT verifier_findings_verdict_check
	CHECK (verdict IN ('supported', 'unsupported', 'number_mismatch', 'uncited'));

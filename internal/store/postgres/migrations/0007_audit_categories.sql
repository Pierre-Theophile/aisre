-- SPDX-License-Identifier: Apache-2.0
--
-- 0007_audit_categories: widen the coverage-audit cause vocabulary from twelve values to
-- eighteen (002 FR-070, FR-071b; docs/evaluation/coverage-audit-format.md).
--
-- Why. The twelve values 0006 accepts were transcribed from FR-070's list of causes the
-- feature-001 feeders miss. That list named no member for the most ordinary change-induced
-- causes of all: a deployment, a configuration change, a provider maintenance window, a vendor
-- API change or deprecation, a capacity or quota limit. When the September 2026 audit was
-- transcribed, six of its thirteen incidents had to be filed as `other` -- and `other` at 46 %
-- of a corpus is a category breakdown that ranks no feeder (FR-070) and an unobservable
-- remainder that names no fixture (FR-071b). The six values added below are exactly those
-- causes.
--
-- Additive, in both directions. Nothing is renamed and nothing is removed: every value 0006
-- accepted, 0007 still accepts, in the same order, so a row written before today still reads
-- and still means what it meant. The new values are appended after `credential_leak`, and
-- `other` is kept last so a report lists every named cause before the catch-all. The Go mirror
-- is internal/investigation/audit/vocabulary.go, and a test parses THIS file -- the
-- highest-numbered migration that defines the constraint -- and asserts the two lists are
-- identical in content and in order.
--
-- Nothing here drops, deletes or truncates: `ALTER TABLE ... DROP CONSTRAINT` replaces a CHECK
-- with a wider one, which adds accepted values and removes no row. Every category any existing
-- row carries is still in the list, so the constraint validates against the table as it stands.
-- scripts/check-migrations.sh is the guard that says so.

ALTER TABLE investigation.coverage_audit_items
	DROP CONSTRAINT IF EXISTS coverage_audit_items_category_check;

ALTER TABLE investigation.coverage_audit_items
	ADD CONSTRAINT coverage_audit_items_category_check CHECK (category IS NULL OR category IN (
		-- the original twelve (0006), unchanged and in their published order
		'flag_flip',
		'iac_apply',
		'db_migration',
		'certificate_expiry',
		'scheduled_job',
		'third_party_outage',
		'traffic_shift',
		'latent_bug',
		'client_side_configuration',
		'business_data_change',
		'credential_leak',
		-- added by 0007: the change-induced causes the twelve could only spell `other`
		'deployment',
		'configuration_change',
		'cloud_maintenance',
		'vendor_api_change',
		'capacity_limit',
		'infrastructure_incident',
		-- the catch-all stays last, and after 0007 should be rare
		'other'
	));

COMMENT ON COLUMN investigation.coverage_audit_items.category IS
	'Cause category from the published eighteen-value set (FR-070); NULL only where the audit recorded no category.';

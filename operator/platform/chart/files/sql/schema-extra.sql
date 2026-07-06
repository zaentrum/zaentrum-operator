-- Runtime objects the cds-generated schema-postgres.sql omits but the manager-api's
-- raw SQL depends on. In prod these come from db/migrations/* applied ON TOP of the
-- cds base schema (cds deploy → migrations). The demo applies only the cds schema, so
-- replicate the migration-created objects the runtime needs here, after the base schema.
--
-- Idempotent (IF NOT EXISTS / CREATE OR REPLACE). NO GRANTs: the demo DB role owns the
-- schema, and prod's `cloud_katalog` role does not exist here. We deliberately do NOT
-- replay the historical migration chain — later migrations (e.g. 017) drop columns that
-- earlier backfills read, which conflicts with the current cds table shape.

-- migration 015: the upsert target for ProcessingStepService.upsert(), which runs
-- INSERT ... ON CONFLICT (item_id, step) DO UPDATE. Without this unique index the
-- analyzer's PUT /api/analyze/items/{id}/steps/{step} 500s ("no unique or exclusion
-- constraint matching the ON CONFLICT specification").
CREATE UNIQUE INDEX IF NOT EXISTS idx_processingsteps_item_step
  ON com_nalet_katalog_itemprocessingsteps (item_id, step);

CREATE INDEX IF NOT EXISTS idx_processingsteps_pending
  ON com_nalet_katalog_itemprocessingsteps (step)
  WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS idx_processingsteps_inflight
  ON com_nalet_katalog_itemprocessingsteps (startedat)
  WHERE status = 'in_progress';

-- migration 015: computed overall-status view (Fiori header badge / status queries).
CREATE OR REPLACE VIEW com_nalet_katalog_itemoverallstatus AS
SELECT
  i.id AS item_id,
  CASE
    WHEN COUNT(s.id) FILTER (WHERE s.status NOT IN ('skipped','not_applicable')) = 0
      THEN 'pending'
    WHEN COUNT(*) FILTER (WHERE s.status = 'failed') > 0
         AND COUNT(*) FILTER (WHERE s.status IN ('pending','in_progress')) = 0
         AND COUNT(*) FILTER (WHERE s.status = 'done') > 0
      THEN 'partial_failure'
    WHEN COUNT(*) FILTER (WHERE s.status = 'failed') > 0
         AND COUNT(*) FILTER (WHERE s.status IN ('pending','in_progress')) = 0
      THEN 'failed'
    WHEN COUNT(*) FILTER (WHERE s.status = 'in_progress') > 0
      THEN 'processing'
    WHEN COUNT(*) FILTER (WHERE s.status = 'pending') > 0
      THEN 'queued'
    WHEN COUNT(*) FILTER (WHERE s.status = 'done') > 0
      THEN 'complete'
    ELSE 'not_applicable'
  END AS overallstatus,
  COUNT(*) FILTER (WHERE s.status = 'done')           AS donecount,
  COUNT(*) FILTER (WHERE s.status = 'pending')        AS pendingcount,
  COUNT(*) FILTER (WHERE s.status = 'failed')         AS failedcount,
  COUNT(*) FILTER (WHERE s.status = 'in_progress')    AS inprogresscount,
  COUNT(*) FILTER (WHERE s.status = 'not_applicable') AS notapplicablecount,
  COUNT(s.id)                                         AS totalsteps,
  MAX(s.finishedat)                                   AS laststepfinishedat
FROM com_nalet_katalog_items i
LEFT JOIN com_nalet_katalog_itemprocessingsteps s ON s.item_id = i.id
GROUP BY i.id;

-- migration 015: CAP service projections (OData reads these; missing -> 500).
CREATE OR REPLACE VIEW KatalogService_ItemProcessingSteps AS
SELECT
  s.ID, s.createdAt, s.createdBy, s.modifiedAt, s.modifiedBy,
  s.item_ID, s.step, s.status, s.startedAt, s.finishedAt, s.attempts, s.error, s.details,
  CASE s.status
    WHEN 'failed'      THEN 1
    WHEN 'in_progress' THEN 2
    WHEN 'pending'     THEN 2
    WHEN 'done'        THEN 3
    ELSE 0
  END AS statusCriticality
FROM com_nalet_katalog_itemprocessingsteps AS s;

CREATE OR REPLACE VIEW KatalogService_ItemOverallStatus AS
SELECT * FROM com_nalet_katalog_itemoverallstatus;

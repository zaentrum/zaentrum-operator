-- catalog schema 007: per-item processing-step audit trail + rollup view.
--
-- Renamed from CAP physical table:
--   com_nalet_katalog_itemprocessingsteps -> katalog_itemprocessingsteps
--
-- One row per (item, step) recording every processing pass applied to files
-- the operator already owns: scan, metadata enrich, intro/credits detection,
-- chapter extraction, fingerprint, blackframe/silence detection, subtitle
-- parse, transcode, package. Status is one of pending | in_progress | done |
-- failed | skipped | not_applicable.
--
-- katalog_itemoverallstatus is the derived rollup the read/admin path treats
-- as a pre-existing relation (it is read-only and computed by GROUP BY), so it
-- is created here as a view over the step rows. Renamed from the CAP rollup
-- view com_nalet_katalog_itemoverallstatus -> katalog_itemoverallstatus.

CREATE TABLE IF NOT EXISTS katalog_itemprocessingsteps (
  id         VARCHAR(36) NOT NULL,
  createdat  TIMESTAMP,
  createdby  VARCHAR(255),
  modifiedat TIMESTAMP,
  modifiedby VARCHAR(255),
  item_id    VARCHAR(36) NOT NULL,
  step       VARCHAR(20) NOT NULL,
  status     VARCHAR(20) NOT NULL DEFAULT 'pending',
  startedat  TIMESTAMP,
  finishedat TIMESTAMP,
  attempts   INTEGER     NOT NULL DEFAULT 0,
  error      VARCHAR(500),
  details    TEXT,
  PRIMARY KEY (id)
);

-- One row per (item, step); upserts go through this unique index.
CREATE UNIQUE INDEX IF NOT EXISTS idx_processingsteps_item_step
  ON katalog_itemprocessingsteps (item_id, step);

-- Work-queue claim: "give me items where this step is pending".
CREATE INDEX IF NOT EXISTS idx_processingsteps_pending
  ON katalog_itemprocessingsteps (step)
  WHERE status = 'pending';

-- Stuck-job recovery: filter the in-flight rows by start time.
CREATE INDEX IF NOT EXISTS idx_processingsteps_inflight
  ON katalog_itemprocessingsteps (startedat)
  WHERE status = 'in_progress';

-- Derived overall-status rollup per item. CREATE OR REPLACE so re-running is a
-- no-op; the per-item GROUP BY is on a single indexed FK column.
CREATE OR REPLACE VIEW katalog_itemoverallstatus AS
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
FROM katalog_items i
LEFT JOIN katalog_itemprocessingsteps s ON s.item_id = i.id
GROUP BY i.id;

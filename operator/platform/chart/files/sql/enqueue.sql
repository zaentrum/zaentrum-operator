-- Post-scan enqueue (demo self-drive). The scan creates Items + PlaybackAssets
-- but NOT ItemProcessingSteps, and nothing auto-enqueues analysis for newly
-- scanned items (by design: production enqueues via the operator "Package" action
-- or the 015_processing_steps migration backfill). The HTTP-poll workers only
-- claim items that already have a `pending` step, so without this the analyzer/
-- transcoder/packager sit idle.
--
-- This reproduces migration 015's backfill for the freshly scanned movie/episode
-- items: seed the analyzer passes (chapter/blackframe/silence/subtitle) + transcode as
-- `pending`. Analyzer passes read the source + write results to the catalog DB. transcode
-- writes to the SHARED packages volume (parent-mount layout), so the packager can read it;
-- the transcode→package chain-promotion (AnalyzerController, on transcode reaching a
-- terminal state) then upserts package=pending automatically — matching the real flow.
--
-- Idempotent via NOT EXISTS (the table has only a PK on id; the (item_id,step) unique
-- index is added by schema-extra.sql), so re-running on every deploy is safe.
INSERT INTO com_nalet_katalog_itemprocessingsteps (id, createdat, item_id, step, status, attempts)
SELECT gen_random_uuid()::varchar, now(), i.id, s.step, 'pending', 0
FROM com_nalet_katalog_items i
CROSS JOIN (VALUES ('chapter'), ('blackframe'), ('silence'), ('subtitle'), ('transcode')) AS s(step)
WHERE i.type IN ('movie', 'episode')
  AND NOT EXISTS (
    SELECT 1 FROM com_nalet_katalog_itemprocessingsteps x
    WHERE x.item_id = i.id AND x.step = s.step
  );


CREATE TABLE IF NOT EXISTS com_nalet_katalog_Items (
  ID VARCHAR(36) NOT NULL,
  createdAt TIMESTAMP,
  createdBy VARCHAR(255),
  modifiedAt TIMESTAMP,
  modifiedBy VARCHAR(255),
  type VARCHAR(20) NOT NULL,
  title VARCHAR(255) NOT NULL,
  sortTitle VARCHAR(255),
  year INTEGER,
  description TEXT,
  rating DECIMAL(3, 1),
  durationMs BIGINT,
  parent_ID VARCHAR(36),
  seasonNumber INTEGER,
  episodeNumber INTEGER,
  tagline VARCHAR(500),
  PRIMARY KEY(ID)
);

CREATE TABLE IF NOT EXISTS com_nalet_katalog_ItemExternalIds (
  ID VARCHAR(36) NOT NULL,
  item_ID VARCHAR(36) NOT NULL,
  source VARCHAR(30) NOT NULL,
  externalId VARCHAR(120) NOT NULL,
  PRIMARY KEY(ID)
);

CREATE TABLE IF NOT EXISTS com_nalet_katalog_ItemArtwork (
  ID VARCHAR(36) NOT NULL,
  item_ID VARCHAR(36) NOT NULL,
  kind VARCHAR(20) NOT NULL,
  url VARCHAR(2048) NOT NULL,
  PRIMARY KEY(ID)
);

CREATE TABLE IF NOT EXISTS com_nalet_katalog_ItemArtworkData (
  ID VARCHAR(36) NOT NULL,
  item_ID VARCHAR(36) NOT NULL,
  kind VARCHAR(20) NOT NULL,
  contentType VARCHAR(80) NOT NULL,
  bytes BYTEA,
  fetchedAt TIMESTAMP,
  PRIMARY KEY(ID)
);

CREATE TABLE IF NOT EXISTS com_nalet_katalog_PlaybackAssets (
  ID VARCHAR(36) NOT NULL,
  item_ID VARCHAR(36) NOT NULL,
  path VARCHAR(2048) NOT NULL,
  codec VARCHAR(40),
  resolution VARCHAR(40),
  bitrateKbps INTEGER,
  sizeBytes BIGINT,
  hash VARCHAR(160),
  isPrimary BOOLEAN DEFAULT FALSE,
  kind VARCHAR(20) DEFAULT 'primary',
  audioCodec VARCHAR(40),
  audioLanguage VARCHAR(10),
  audioChannels INTEGER,
  audioBitrateKbps INTEGER,
  audioTrackCount INTEGER,
  subtitleTrackCount INTEGER,
  durationMs BIGINT,
  PRIMARY KEY(ID)
);

CREATE TABLE IF NOT EXISTS com_nalet_katalog_SubtitleAssets (
  ID VARCHAR(36) NOT NULL,
  item_ID VARCHAR(36) NOT NULL,
  path VARCHAR(2048) NOT NULL,
  format VARCHAR(10),
  lang VARCHAR(10),
  label VARCHAR(120),
  isDefault BOOLEAN DEFAULT FALSE,
  PRIMARY KEY(ID)
);

CREATE TABLE IF NOT EXISTS com_nalet_katalog_MediaSegments (
  ID VARCHAR(36) NOT NULL,
  createdAt TIMESTAMP,
  createdBy VARCHAR(255),
  modifiedAt TIMESTAMP,
  modifiedBy VARCHAR(255),
  item_ID VARCHAR(36) NOT NULL,
  kind VARCHAR(20) NOT NULL,
  startMs BIGINT NOT NULL,
  endMs BIGINT NOT NULL,
  source VARCHAR(30) NOT NULL,
  confidence DECIMAL(3, 2),
  label VARCHAR(120),
  PRIMARY KEY(ID)
);

CREATE TABLE IF NOT EXISTS com_nalet_katalog_ItemTrailerLinks (
  ID VARCHAR(36) NOT NULL,
  createdAt TIMESTAMP,
  createdBy VARCHAR(255),
  modifiedAt TIMESTAMP,
  modifiedBy VARCHAR(255),
  item_ID VARCHAR(36) NOT NULL,
  source VARCHAR(20) NOT NULL,
  site VARCHAR(40),
  externalId VARCHAR(120),
  url VARCHAR(2048) NOT NULL,
  title VARCHAR(255),
  durationSec INTEGER,
  publishedAt TIMESTAMP,
  downloadedAt TIMESTAMP,
  localPath VARCHAR(2048),
  PRIMARY KEY(ID)
);

CREATE TABLE IF NOT EXISTS com_nalet_katalog_ItemDiagnostics (
  ID VARCHAR(36) NOT NULL,
  item_ID VARCHAR(36) NOT NULL,
  generatedAt TIMESTAMP,
  sourcePath VARCHAR(2048),
  sourceSize BIGINT,
  sourceMtime TIMESTAMP,
  ffprobeData TEXT,
  folderListing TEXT,
  notes VARCHAR(1024),
  PRIMARY KEY(ID)
);

CREATE TABLE IF NOT EXISTS com_nalet_katalog_ItemProcessingSteps (
  ID VARCHAR(36) NOT NULL,
  createdAt TIMESTAMP,
  createdBy VARCHAR(255),
  modifiedAt TIMESTAMP,
  modifiedBy VARCHAR(255),
  item_ID VARCHAR(36) NOT NULL,
  step VARCHAR(20) NOT NULL,
  status VARCHAR(20) NOT NULL DEFAULT 'pending',
  startedAt TIMESTAMP,
  finishedAt TIMESTAMP,
  attempts INTEGER DEFAULT 0,
  error VARCHAR(500),
  details TEXT,
  PRIMARY KEY(ID)
);

CREATE TABLE IF NOT EXISTS com_nalet_katalog_ItemGenres (
  ID VARCHAR(36) NOT NULL,
  item_ID VARCHAR(36) NOT NULL,
  genre_ID VARCHAR(36) NOT NULL,
  PRIMARY KEY(ID)
);

CREATE TABLE IF NOT EXISTS com_nalet_katalog_Genres (
  ID VARCHAR(36) NOT NULL,
  name VARCHAR(80) NOT NULL,
  PRIMARY KEY(ID)
);

CREATE TABLE IF NOT EXISTS com_nalet_katalog_ItemPeople (
  ID VARCHAR(36) NOT NULL,
  item_ID VARCHAR(36) NOT NULL,
  person_ID VARCHAR(36) NOT NULL,
  role VARCHAR(40) NOT NULL,
  PRIMARY KEY(ID)
);

CREATE TABLE IF NOT EXISTS com_nalet_katalog_People (
  ID VARCHAR(36) NOT NULL,
  name VARCHAR(255) NOT NULL,
  PRIMARY KEY(ID)
);

CREATE TABLE IF NOT EXISTS com_nalet_katalog_ItemTags (
  ID VARCHAR(36) NOT NULL,
  item_ID VARCHAR(36) NOT NULL,
  tag VARCHAR(120) NOT NULL,
  PRIMARY KEY(ID)
);

CREATE TABLE IF NOT EXISTS com_nalet_katalog_ScanJobs (
  ID VARCHAR(36) NOT NULL,
  source VARCHAR(20) NOT NULL,
  status VARCHAR(20) NOT NULL DEFAULT 'queued',
  startedAt TIMESTAMP,
  finishedAt TIMESTAMP,
  errorMessage TEXT,
  filesSeen INTEGER DEFAULT 0,
  itemsInserted INTEGER DEFAULT 0,
  itemsUpdated INTEGER DEFAULT 0,
  PRIMARY KEY(ID)
);

CREATE TABLE IF NOT EXISTS com_nalet_katalog_ItemChapters (
  ID VARCHAR(36) NOT NULL,
  createdAt TIMESTAMP,
  createdBy VARCHAR(255),
  modifiedAt TIMESTAMP,
  modifiedBy VARCHAR(255),
  item_ID VARCHAR(36) NOT NULL,
  startMs BIGINT NOT NULL,
  endMs BIGINT NOT NULL,
  title VARCHAR(120),
  ordinal INTEGER,
  PRIMARY KEY(ID)
);

CREATE TABLE IF NOT EXISTS com_nalet_katalog_EnrichmentStatusCodes (
  code VARCHAR(20) NOT NULL,
  name VARCHAR(40),
  PRIMARY KEY(code)
);

CREATE TABLE IF NOT EXISTS com_nalet_katalog_Settings (
  ID VARCHAR(36) NOT NULL,
  createdAt TIMESTAMP,
  createdBy VARCHAR(255),
  modifiedAt TIMESTAMP,
  modifiedBy VARCHAR(255),
  key VARCHAR(120) NOT NULL,
  valueText VARCHAR(2000) NOT NULL DEFAULT '',
  valueType VARCHAR(20) NOT NULL DEFAULT 'string',
  description TEXT,
  PRIMARY KEY(ID)
);

CREATE TABLE IF NOT EXISTS com_nalet_katalog_DownloadJobs (
  ID VARCHAR(36) NOT NULL,
  createdAt TIMESTAMP,
  createdBy VARCHAR(255),
  modifiedAt TIMESTAMP,
  modifiedBy VARCHAR(255),
  adapter VARCHAR(40) NOT NULL,
  clientJobId VARCHAR(255) NOT NULL,
  title VARCHAR(500),
  wantedItemId VARCHAR(80),
  state VARCHAR(20) NOT NULL DEFAULT 'queued',
  progressPct DECIMAL(5, 2) DEFAULT 0,
  downloadedBytes BIGINT DEFAULT 0,
  sizeBytes BIGINT,
  speedBps BIGINT,
  etaSec INTEGER,
  files TEXT,
  errorMessage TEXT,
  startedAt TIMESTAMP,
  completedAt TIMESTAMP,
  lastEventAt TIMESTAMP,
  PRIMARY KEY(ID)
);

CREATE TABLE IF NOT EXISTS com_nalet_katalog_EnrichmentJobs (
  ID VARCHAR(36) NOT NULL,
  status VARCHAR(20) NOT NULL DEFAULT 'queued',
  startedAt TIMESTAMP,
  finishedAt TIMESTAMP,
  errorMessage TEXT,
  itemsConsidered INTEGER DEFAULT 0,
  itemsEnriched INTEGER DEFAULT 0,
  itemsFailed INTEGER DEFAULT 0,
  PRIMARY KEY(ID)
);

CREATE OR REPLACE VIEW KatalogService_Items AS SELECT
  Items_0.ID,
  Items_0.createdAt,
  Items_0.createdBy,
  Items_0.modifiedAt,
  Items_0.modifiedBy,
  Items_0.type,
  Items_0.title,
  Items_0.sortTitle,
  Items_0.year,
  Items_0.description,
  Items_0.rating,
  Items_0.durationMs,
  Items_0.parent_ID,
  Items_0.seasonNumber,
  Items_0.episodeNumber,
  Items_0.tagline,
  '/katalog-api/api/artwork/' || Items_0.ID || '/poster' AS posterUrl,
  '/katalog-api/api/artwork/' || Items_0.ID || '/backdrop' AS backdropUrl,
  Items_0.durationMs / 60000 AS runtimeMin,
  CAST(Items_0.year AS VARCHAR(255)) AS yearText
FROM com_nalet_katalog_Items AS Items_0;

CREATE OR REPLACE VIEW KatalogService_ItemExternalIds AS SELECT
  ItemExternalIds_0.ID,
  ItemExternalIds_0.item_ID,
  ItemExternalIds_0.source,
  ItemExternalIds_0.externalId
FROM com_nalet_katalog_ItemExternalIds AS ItemExternalIds_0;

CREATE OR REPLACE VIEW KatalogService_ItemArtwork AS SELECT
  ItemArtwork_0.ID,
  ItemArtwork_0.item_ID,
  ItemArtwork_0.kind,
  ItemArtwork_0.url
FROM com_nalet_katalog_ItemArtwork AS ItemArtwork_0;

CREATE OR REPLACE VIEW KatalogService_ItemArtworkData AS SELECT
  ItemArtworkData_0.ID,
  ItemArtworkData_0.item_ID,
  ItemArtworkData_0.kind,
  ItemArtworkData_0.contentType,
  ItemArtworkData_0.bytes,
  ItemArtworkData_0.fetchedAt
FROM com_nalet_katalog_ItemArtworkData AS ItemArtworkData_0;

CREATE OR REPLACE VIEW KatalogService_PlaybackAssets AS SELECT
  PlaybackAssets_0.ID,
  PlaybackAssets_0.item_ID,
  PlaybackAssets_0.path,
  PlaybackAssets_0.codec,
  PlaybackAssets_0.resolution,
  PlaybackAssets_0.bitrateKbps,
  PlaybackAssets_0.sizeBytes,
  PlaybackAssets_0.hash,
  PlaybackAssets_0.isPrimary,
  PlaybackAssets_0.kind,
  PlaybackAssets_0.audioCodec,
  PlaybackAssets_0.audioLanguage,
  PlaybackAssets_0.audioChannels,
  PlaybackAssets_0.audioBitrateKbps,
  PlaybackAssets_0.audioTrackCount,
  PlaybackAssets_0.subtitleTrackCount,
  PlaybackAssets_0.durationMs,
  PlaybackAssets_0.sizeBytes / 1048576 AS sizeMB
FROM com_nalet_katalog_PlaybackAssets AS PlaybackAssets_0;

CREATE OR REPLACE VIEW KatalogService_SubtitleAssets AS SELECT
  SubtitleAssets_0.ID,
  SubtitleAssets_0.item_ID,
  SubtitleAssets_0.path,
  SubtitleAssets_0.format,
  SubtitleAssets_0.lang,
  SubtitleAssets_0.label,
  SubtitleAssets_0.isDefault
FROM com_nalet_katalog_SubtitleAssets AS SubtitleAssets_0;

CREATE OR REPLACE VIEW KatalogService_MediaSegments AS SELECT
  MediaSegments_0.ID,
  MediaSegments_0.createdAt,
  MediaSegments_0.createdBy,
  MediaSegments_0.modifiedAt,
  MediaSegments_0.modifiedBy,
  MediaSegments_0.item_ID,
  MediaSegments_0.kind,
  MediaSegments_0.startMs,
  MediaSegments_0.endMs,
  MediaSegments_0.source,
  MediaSegments_0.confidence,
  MediaSegments_0.label
FROM com_nalet_katalog_MediaSegments AS MediaSegments_0;

CREATE OR REPLACE VIEW KatalogService_ItemTrailerLinks AS SELECT
  ItemTrailerLinks_0.ID,
  ItemTrailerLinks_0.createdAt,
  ItemTrailerLinks_0.createdBy,
  ItemTrailerLinks_0.modifiedAt,
  ItemTrailerLinks_0.modifiedBy,
  ItemTrailerLinks_0.item_ID,
  ItemTrailerLinks_0.source,
  ItemTrailerLinks_0.site,
  ItemTrailerLinks_0.externalId,
  ItemTrailerLinks_0.url,
  ItemTrailerLinks_0.title,
  ItemTrailerLinks_0.durationSec,
  ItemTrailerLinks_0.publishedAt,
  ItemTrailerLinks_0.downloadedAt,
  ItemTrailerLinks_0.localPath
FROM com_nalet_katalog_ItemTrailerLinks AS ItemTrailerLinks_0;

CREATE OR REPLACE VIEW KatalogService_ItemDiagnostics AS SELECT
  ItemDiagnostics_0.ID,
  ItemDiagnostics_0.item_ID,
  ItemDiagnostics_0.generatedAt,
  ItemDiagnostics_0.sourcePath,
  ItemDiagnostics_0.sourceSize,
  ItemDiagnostics_0.sourceMtime,
  ItemDiagnostics_0.ffprobeData,
  ItemDiagnostics_0.folderListing,
  ItemDiagnostics_0.notes
FROM com_nalet_katalog_ItemDiagnostics AS ItemDiagnostics_0;

CREATE OR REPLACE VIEW KatalogService_ItemProcessingSteps AS SELECT
  ItemProcessingSteps_0.ID,
  ItemProcessingSteps_0.createdAt,
  ItemProcessingSteps_0.createdBy,
  ItemProcessingSteps_0.modifiedAt,
  ItemProcessingSteps_0.modifiedBy,
  ItemProcessingSteps_0.item_ID,
  ItemProcessingSteps_0.step,
  ItemProcessingSteps_0.status,
  ItemProcessingSteps_0.startedAt,
  ItemProcessingSteps_0.finishedAt,
  ItemProcessingSteps_0.attempts,
  ItemProcessingSteps_0.error,
  ItemProcessingSteps_0.details,
  CASE ItemProcessingSteps_0.status WHEN 'failed' THEN 1 WHEN 'in_progress' THEN 2 WHEN 'pending' THEN 2 WHEN 'done' THEN 3 ELSE 0 END AS statusCriticality
FROM com_nalet_katalog_ItemProcessingSteps AS ItemProcessingSteps_0;

CREATE OR REPLACE VIEW KatalogService_ItemOverallStatus AS SELECT
  ItemOverallStatus_0.item_ID,
  ItemOverallStatus_0.overallStatus,
  ItemOverallStatus_0.doneCount,
  ItemOverallStatus_0.pendingCount,
  ItemOverallStatus_0.failedCount,
  ItemOverallStatus_0.inProgressCount,
  ItemOverallStatus_0.notApplicableCount,
  ItemOverallStatus_0.totalSteps,
  ItemOverallStatus_0.lastStepFinishedAt
FROM com_nalet_katalog_ItemOverallStatus AS ItemOverallStatus_0;

CREATE OR REPLACE VIEW KatalogService_ItemGenres AS SELECT
  ItemGenres_0.ID,
  ItemGenres_0.item_ID,
  ItemGenres_0.genre_ID
FROM com_nalet_katalog_ItemGenres AS ItemGenres_0;

CREATE OR REPLACE VIEW KatalogService_Genres AS SELECT
  Genres_0.ID,
  Genres_0.name
FROM com_nalet_katalog_Genres AS Genres_0;

CREATE OR REPLACE VIEW KatalogService_ItemPeople AS SELECT
  ItemPeople_0.ID,
  ItemPeople_0.item_ID,
  ItemPeople_0.person_ID,
  ItemPeople_0.role
FROM com_nalet_katalog_ItemPeople AS ItemPeople_0;

CREATE OR REPLACE VIEW KatalogService_People AS SELECT
  People_0.ID,
  People_0.name
FROM com_nalet_katalog_People AS People_0;

CREATE OR REPLACE VIEW KatalogService_ItemTags AS SELECT
  ItemTags_0.ID,
  ItemTags_0.item_ID,
  ItemTags_0.tag
FROM com_nalet_katalog_ItemTags AS ItemTags_0;

CREATE OR REPLACE VIEW KatalogService_ScanJobs AS SELECT
  ScanJobs_0.ID,
  ScanJobs_0.source,
  ScanJobs_0.status,
  ScanJobs_0.startedAt,
  ScanJobs_0.finishedAt,
  ScanJobs_0.errorMessage,
  ScanJobs_0.filesSeen,
  ScanJobs_0.itemsInserted,
  ScanJobs_0.itemsUpdated
FROM com_nalet_katalog_ScanJobs AS ScanJobs_0;

CREATE OR REPLACE VIEW KatalogService_ItemChapters AS SELECT
  ItemChapters_0.ID,
  ItemChapters_0.createdAt,
  ItemChapters_0.createdBy,
  ItemChapters_0.modifiedAt,
  ItemChapters_0.modifiedBy,
  ItemChapters_0.item_ID,
  ItemChapters_0.startMs,
  ItemChapters_0.endMs,
  ItemChapters_0.title,
  ItemChapters_0.ordinal
FROM com_nalet_katalog_ItemChapters AS ItemChapters_0;

CREATE OR REPLACE VIEW KatalogService_EnrichmentStatusCodes AS SELECT
  EnrichmentStatusCodes_0.code,
  EnrichmentStatusCodes_0.name
FROM com_nalet_katalog_EnrichmentStatusCodes AS EnrichmentStatusCodes_0;

CREATE OR REPLACE VIEW KatalogService_Settings AS SELECT
  Settings_0.ID,
  Settings_0.createdAt,
  Settings_0.createdBy,
  Settings_0.modifiedAt,
  Settings_0.modifiedBy,
  Settings_0.key,
  Settings_0.valueText,
  Settings_0.valueType,
  Settings_0.description
FROM com_nalet_katalog_Settings AS Settings_0;

CREATE OR REPLACE VIEW KatalogService_Movies AS SELECT
  Items_0.ID,
  Items_0.createdAt,
  Items_0.createdBy,
  Items_0.modifiedAt,
  Items_0.modifiedBy,
  Items_0.type,
  Items_0.title,
  Items_0.sortTitle,
  Items_0.year,
  Items_0.description,
  Items_0.rating,
  Items_0.durationMs,
  Items_0.parent_ID,
  Items_0.seasonNumber,
  Items_0.episodeNumber,
  Items_0.tagline,
  '/katalog-api/api/artwork/' || Items_0.ID || '/poster' AS posterUrl,
  '/katalog-api/api/artwork/' || Items_0.ID || '/backdrop' AS backdropUrl,
  Items_0.durationMs / 60000 AS runtimeMin,
  CAST(Items_0.year AS VARCHAR(255)) AS yearText,
  CASE WHEN EXISTS (SELECT
      1 AS dummy
    FROM com_nalet_katalog_PlaybackAssets AS _assets_exists_1
    WHERE _assets_exists_1.item_ID = Items_0.ID AND (_assets_exists_1.codec LIKE 'hev1%' OR _assets_exists_1.codec LIKE 'hvc1%')) THEN TRUE ELSE FALSE END AS isPackaged
FROM com_nalet_katalog_Items AS Items_0
WHERE Items_0.type = 'movie';

CREATE OR REPLACE VIEW KatalogService_Series AS SELECT
  Items_0.ID,
  Items_0.createdAt,
  Items_0.createdBy,
  Items_0.modifiedAt,
  Items_0.modifiedBy,
  Items_0.type,
  Items_0.title,
  Items_0.sortTitle,
  Items_0.year,
  Items_0.description,
  Items_0.rating,
  Items_0.durationMs,
  Items_0.parent_ID,
  Items_0.seasonNumber,
  Items_0.episodeNumber,
  Items_0.tagline,
  '/katalog-api/api/artwork/' || Items_0.ID || '/poster' AS posterUrl,
  '/katalog-api/api/artwork/' || Items_0.ID || '/backdrop' AS backdropUrl,
  Items_0.durationMs / 60000 AS runtimeMin,
  CAST(Items_0.year AS VARCHAR(255)) AS yearText,
  CASE WHEN EXISTS (SELECT
      1 AS dummy
    FROM com_nalet_katalog_Items AS _children_exists_1
    WHERE _children_exists_1.parent_ID = Items_0.ID AND _children_exists_1.type = 'episode') AND NOT EXISTS (SELECT
      1 AS dummy
    FROM com_nalet_katalog_Items AS _children_exists_2
    WHERE _children_exists_2.parent_ID = Items_0.ID AND (_children_exists_2.type = 'episode' AND NOT EXISTS (SELECT
        1 AS dummy
      FROM com_nalet_katalog_PlaybackAssets AS _assets_exists_3
      WHERE _assets_exists_3.item_ID = _children_exists_2.ID AND (_assets_exists_3.codec LIKE 'hev1%' OR _assets_exists_3.codec LIKE 'hvc1%')))) THEN TRUE ELSE FALSE END AS isPackaged
FROM com_nalet_katalog_Items AS Items_0
WHERE Items_0.type = 'series';

CREATE OR REPLACE VIEW KatalogService_Episodes AS SELECT
  Items_0.ID,
  Items_0.createdAt,
  Items_0.createdBy,
  Items_0.modifiedAt,
  Items_0.modifiedBy,
  Items_0.type,
  Items_0.title,
  Items_0.sortTitle,
  Items_0.year,
  Items_0.description,
  Items_0.rating,
  Items_0.durationMs,
  Items_0.parent_ID,
  Items_0.seasonNumber,
  Items_0.episodeNumber,
  Items_0.tagline,
  '/katalog-api/api/artwork/' || Items_0.ID || '/poster' AS posterUrl,
  '/katalog-api/api/artwork/' || Items_0.ID || '/backdrop' AS backdropUrl,
  Items_0.durationMs / 60000 AS runtimeMin,
  CAST(Items_0.year AS VARCHAR(255)) AS yearText,
  CASE WHEN EXISTS (SELECT
      1 AS dummy
    FROM com_nalet_katalog_MediaSegments AS _segments_exists_1
    WHERE _segments_exists_1.item_ID = Items_0.ID AND _segments_exists_1.kind = 'intro') THEN TRUE ELSE FALSE END AS hasIntro,
  CASE WHEN EXISTS (SELECT
      1 AS dummy
    FROM com_nalet_katalog_MediaSegments AS _segments_exists_2
    WHERE _segments_exists_2.item_ID = Items_0.ID AND _segments_exists_2.kind = 'credits') THEN TRUE ELSE FALSE END AS hasCredits,
  CASE WHEN EXISTS (SELECT
      1 AS dummy
    FROM com_nalet_katalog_MediaSegments AS _segments_exists_3
    WHERE _segments_exists_3.item_ID = Items_0.ID AND _segments_exists_3.kind = 'recap') THEN TRUE ELSE FALSE END AS hasRecap,
  CASE WHEN EXISTS (SELECT
      1 AS dummy
    FROM com_nalet_katalog_PlaybackAssets AS _assets_exists_4
    WHERE _assets_exists_4.item_ID = Items_0.ID AND (_assets_exists_4.codec LIKE 'hev1%' OR _assets_exists_4.codec LIKE 'hvc1%')) THEN TRUE ELSE FALSE END AS isPackaged
FROM com_nalet_katalog_Items AS Items_0
WHERE Items_0.type = 'episode';

CREATE OR REPLACE VIEW KatalogService_Albums AS SELECT
  Items_0.ID,
  Items_0.createdAt,
  Items_0.createdBy,
  Items_0.modifiedAt,
  Items_0.modifiedBy,
  Items_0.type,
  Items_0.title,
  Items_0.sortTitle,
  Items_0.year,
  Items_0.description,
  Items_0.rating,
  Items_0.durationMs,
  Items_0.parent_ID,
  Items_0.seasonNumber,
  Items_0.episodeNumber,
  Items_0.tagline,
  '/katalog-api/api/artwork/' || Items_0.ID || '/poster' AS posterUrl,
  '/katalog-api/api/artwork/' || Items_0.ID || '/backdrop' AS backdropUrl,
  Items_0.durationMs / 60000 AS runtimeMin,
  CAST(Items_0.year AS VARCHAR(255)) AS yearText
FROM com_nalet_katalog_Items AS Items_0
WHERE Items_0.type = 'album';

CREATE OR REPLACE VIEW KatalogService_DownloadJobs AS SELECT
  DownloadJobs_0.ID,
  DownloadJobs_0.createdAt,
  DownloadJobs_0.createdBy,
  DownloadJobs_0.modifiedAt,
  DownloadJobs_0.modifiedBy,
  DownloadJobs_0.adapter,
  DownloadJobs_0.clientJobId,
  DownloadJobs_0.title,
  DownloadJobs_0.wantedItemId,
  DownloadJobs_0.state,
  DownloadJobs_0.progressPct,
  DownloadJobs_0.downloadedBytes,
  DownloadJobs_0.sizeBytes,
  DownloadJobs_0.speedBps,
  DownloadJobs_0.etaSec,
  DownloadJobs_0.files,
  DownloadJobs_0.errorMessage,
  DownloadJobs_0.startedAt,
  DownloadJobs_0.completedAt,
  DownloadJobs_0.lastEventAt,
  CASE DownloadJobs_0.state WHEN 'failed' THEN 1 WHEN 'downloading' THEN 2 WHEN 'queued' THEN 2 WHEN 'completed' THEN 3 ELSE 0 END AS stateCriticality
FROM com_nalet_katalog_DownloadJobs AS DownloadJobs_0;

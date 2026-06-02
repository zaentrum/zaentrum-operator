-- catalog schema 009: global key/value settings.
--
-- Renamed from CAP physical table:
--   com_nalet_katalog_settings -> katalog_settings
--
-- One row per logical knob. Consumers (the packager language whitelist /
-- single-language fallback, the validate small-file threshold) grep their key
-- out of this table at runtime; there is no compile-time coupling. valuetext
-- carries the canonical representation (scalar or comma-separated list);
-- valuetype is informational ('string' | 'list_csv' | 'bool' | 'int' |
-- 'float'). Column shape matches the CAP-aligned schema (camelCase folded to
-- lowercase: valuetext / valuetype, plus the managed columns).

CREATE TABLE IF NOT EXISTS katalog_settings (
  id          VARCHAR(36)   NOT NULL,
  createdat   TIMESTAMP     NOT NULL DEFAULT now(),
  createdby   VARCHAR(255),
  modifiedat  TIMESTAMP     NOT NULL DEFAULT now(),
  modifiedby  VARCHAR(255),
  key         VARCHAR(120)  NOT NULL UNIQUE,
  valuetext   VARCHAR(2000) NOT NULL DEFAULT '',
  valuetype   VARCHAR(20)   NOT NULL DEFAULT 'string',
  description TEXT,
  PRIMARY KEY (id)
);

-- Default rows so the packager + validate keep working on a fresh install.
-- ON CONFLICT (key) DO UPDATE refreshes the description only; operator-edited
-- valuetext is preserved across re-runs.
INSERT INTO katalog_settings (id, key, valuetext, valuetype, description)
VALUES
  (gen_random_uuid()::varchar,
   'packager.language_whitelist',
   'en,de,zh',
   'list_csv',
   'ISO 639-1/2 codes (comma-separated) the packager keeps for audio AND subtitle tracks. Empty = keep everything. Matched case-insensitively against the source track''s language tag.'),

  (gen_random_uuid()::varchar,
   'packager.keep_original_if_single',
   'true',
   'bool',
   'When the source has exactly one audio language and zero matches in the whitelist, keep that one anyway. Catches the foreign-only case where the whitelist would otherwise drop every track.'),

  (gen_random_uuid()::varchar,
   'validate.small_file_threshold_mb',
   '5',
   'int',
   'Files-facet rows whose size is below this many MB AND whose kind is not ''primary'' are reported as stray_small_file by Validate.')
ON CONFLICT (key) DO UPDATE
  SET description = EXCLUDED.description,
      modifiedat  = now();

-- Independent one-time checkpoint; never rewrite ongoing import progress.
ALTER TABLE post_import_states ADD COLUMN history_enabled BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE post_import_states ADD COLUMN history_json TEXT NOT NULL DEFAULT '';
ALTER TABLE post_import_states ADD COLUMN history_next_eligible_at TIMESTAMP;

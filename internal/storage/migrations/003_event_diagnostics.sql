ALTER TABLE inbox_events ADD COLUMN content_type TEXT;
ALTER TABLE inbox_events ADD COLUMN diagnostic_reason TEXT;
ALTER TABLE inbox_events ADD COLUMN route_id TEXT;

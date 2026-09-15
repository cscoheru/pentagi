-- Langfuse ClickHouse TTL retention (intel-platform fork hardening)
-- Set: 30 days for analytical data, 14 days for raw event log + blob references.
-- ClickHouse 24 requires DateTime (not DateTime64) in TTL expressions,
-- so we wrap each time column with toDateTime() to truncate precision.

ALTER TABLE observations          MODIFY TTL toDateTime(start_time) + INTERVAL 30 DAY;
ALTER TABLE traces               MODIFY TTL toDateTime(timestamp)  + INTERVAL 30 DAY;
ALTER TABLE scores               MODIFY TTL toDateTime(timestamp)  + INTERVAL 30 DAY;

ALTER TABLE event_log             MODIFY TTL toDateTime(created_at) + INTERVAL 14 DAY;
ALTER TABLE blob_storage_file_log MODIFY TTL toDateTime(event_ts)   + INTERVAL 14 DAY;

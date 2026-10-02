-- +goose Up
-- +goose StatementBegin
-- Task output-path contract (D3): the path the framework writes the task result to.
-- Keeping it structured instead of free text in `result` is what lets the runtime
-- persist and verify the deliverable without trusting the model to report where it
-- saved the file.
-- Nullable on purpose: rows created before this migration never declared a contract
-- path, and NULL is the honest encoding of "not declared".
ALTER TABLE tasks ADD COLUMN output_path TEXT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE tasks DROP COLUMN output_path;
-- +goose StatementEnd

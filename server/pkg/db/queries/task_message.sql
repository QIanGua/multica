-- name: CreateTaskMessage :one
INSERT INTO task_message (id, task_id, seq, type, tool, content, input, output)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: CreateTaskMessages :many
-- Batch variant of CreateTaskMessage: persists a whole daemon-reported batch in
-- ONE statement (and therefore one round trip and one commit) instead of one
-- INSERT per message. The rows arrive as a single jsonb document expanded by
-- jsonb_to_recordset rather than as parallel arrays, because `input` is itself
-- jsonb — parallel arrays would need a jsonb[] parameter, and a per-column
-- array cannot express "this row's input is NULL" from Go without a nullable
-- element type. A jsonb document also keeps the statement free of the ~64k
-- bind-parameter ceiling a multi-row VALUES list would hit.
--
-- Elements must carry: id (uuid), seq (int), type (text), and optionally tool /
-- content / output (text) and input (jsonb); an omitted or JSON-null field
-- becomes SQL NULL, matching the pgtype.Text{Valid:false} semantics of the
-- single-row query. Callers MUST have run the Postgres text sanitizer first:
-- jsonb rejects \u0000 outright, so an unsanitized NUL fails the whole batch
-- (GH #7098) instead of just one row.
--
-- Atomicity is a deliberate side effect, not just a speedup: the per-message
-- loop this replaces could persist part of a batch and then fail, leaving a
-- permanent hole in the transcript because the daemon does not retry this
-- endpoint. One statement makes the batch all-or-nothing.
INSERT INTO task_message (id, task_id, seq, type, tool, content, input, output)
SELECT m.id, sqlc.arg('task_id')::uuid, m.seq, m.type, m.tool, m.content, m.input, m.output
FROM jsonb_to_recordset(sqlc.arg('messages')::jsonb)
    AS m(id uuid, seq integer, type text, tool text, content text, input jsonb, output text)
RETURNING *;

-- name: ListTaskMessages :many
SELECT * FROM task_message
WHERE task_id = $1
ORDER BY seq ASC;

-- name: ListTaskMessagesSince :many
SELECT * FROM task_message
WHERE task_id = $1 AND seq > $2
ORDER BY seq ASC;

-- name: DeleteTaskMessages :exec
DELETE FROM task_message
WHERE task_id = $1;

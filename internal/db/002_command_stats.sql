-- Summaries retain a score at last_used, avoiding an exponentially growing
-- epoch-based weight. Queries decay it to the current time. 2592000000 ms = 30 days.
CREATE TABLE command_stats (
    scope_path TEXT NOT NULL,
    cwd TEXT NOT NULL,
    command TEXT NOT NULL,
    executions INTEGER NOT NULL,
    last_used INTEGER NOT NULL,
    last_id INTEGER NOT NULL,
    weight REAL NOT NULL,
    PRIMARY KEY (scope_path, cwd, command)
) WITHOUT ROWID;
CREATE INDEX idx_command_stats_cwd ON command_stats(cwd, command);
CREATE TABLE dirty_commands (
    scope_path TEXT NOT NULL,
    cwd TEXT NOT NULL,
    command TEXT NOT NULL,
    PRIMARY KEY (scope_path, cwd, command)
) WITHOUT ROWID;
CREATE INDEX idx_history_command ON history(scope_path, cwd, command, started_at DESC, id DESC);

CREATE TRIGGER history_stats_insert AFTER INSERT ON history BEGIN
    INSERT INTO command_stats VALUES (new.scope_path, new.cwd, new.command, 1, new.started_at, new.id, 1.0)
    ON CONFLICT(scope_path, cwd, command) DO UPDATE SET
        executions = executions + 1,
        weight = weight * pow(2.0, (CAST(last_used AS REAL) - max(last_used, new.started_at)) / 2592000000.0)
               + pow(2.0, (CAST(new.started_at AS REAL) - max(last_used, new.started_at)) / 2592000000.0),
        last_id = CASE WHEN (new.started_at, new.id) > (last_used, last_id) THEN new.id ELSE last_id END,
        last_used = max(last_used, new.started_at);
END;
-- Rebuild each affected command once after a bulk deletion, not once per event.
CREATE TRIGGER history_stats_delete AFTER DELETE ON history BEGIN
    INSERT OR IGNORE INTO dirty_commands VALUES (old.scope_path, old.cwd, old.command);
END;
CREATE TRIGGER history_stats_update AFTER UPDATE ON history BEGIN
    INSERT OR IGNORE INTO dirty_commands VALUES (old.scope_path, old.cwd, old.command);
    INSERT OR IGNORE INTO dirty_commands VALUES (new.scope_path, new.cwd, new.command);
END;
INSERT INTO dirty_commands SELECT DISTINCT scope_path, cwd, command FROM history;
PRAGMA user_version = 2;

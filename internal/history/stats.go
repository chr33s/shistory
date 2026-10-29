package history

import "database/sql"

// RefreshStats repairs summaries invalidated by deletes or updates. The caller
// owns the transaction, so counts, ranking and representative events share one
// snapshot. Inserts update clean summaries directly via a database trigger.
func RefreshStats(tx *sql.Tx) error {
	var dirty bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM dirty_commands)").Scan(&dirty); err != nil {
		return err
	}
	if !dirty {
		return nil
	}
	_, err := tx.Exec(`
 DELETE FROM command_stats WHERE (scope_path,cwd,command) IN (SELECT scope_path,cwd,command FROM dirty_commands);
 INSERT INTO command_stats(scope_path,cwd,command,executions,last_used,last_id,weight)
 SELECT scope_path,cwd,command,count(*),max(started_at),max(CASE WHEN position=1 THEN id END),
        sum(pow(2.0,(CAST(started_at AS REAL)-newest)/2592000000.0))
 FROM (
   SELECT h.scope_path,h.cwd,h.command,h.id,h.started_at,
          max(h.started_at) OVER command_group AS newest,
          row_number() OVER (command_group ORDER BY h.started_at DESC,h.id DESC) AS position
   FROM dirty_commands d JOIN history h
     ON h.scope_path=d.scope_path AND h.cwd=d.cwd AND h.command=d.command
   WINDOW command_group AS (PARTITION BY h.scope_path,h.cwd,h.command)
 ) GROUP BY scope_path,cwd,command;
 DELETE FROM dirty_commands;
 `)
	return err
}

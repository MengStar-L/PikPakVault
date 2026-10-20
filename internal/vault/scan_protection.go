package vault

import "database/sql"

// Transfers own stable local IDs, including when a file has moved away from its
// original monitored folder. Protect the current ancestors too: a file upload
// can restore those directories without explicitly listing them in its job.
func scanProtectedNodes(q rowsQuerier, account string) (map[string]bool, error) {
	rows, err := q.Query(resourceJobsCTE+`, protected(id) AS (
		SELECT node_id FROM resource_jobs
		UNION SELECT n.parent_id FROM nodes n JOIN protected p ON n.id=p.id
		WHERE n.parent_id<>'' AND n.id<>'root'
	) SELECT id FROM protected`, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func scanUnknown(tx *sql.Tx, account string, protected map[string]bool) error {
	ids := make([]string, 0, len(protected))
	for id := range protected {
		ids = append(ids, id)
	}
	_, err := tx.Exec(`UPDATE bindings SET state='unknown' WHERE account_id=?
		AND node_id NOT IN (SELECT value FROM json_each(?))`, account, jsonText(ids))
	return err
}

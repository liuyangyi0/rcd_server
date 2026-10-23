package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"rcd_manager/internal/calculation"
	"rcd_manager/internal/domain"
)

// Explicit deployment mappings; never infer arbitrary source identity from an
// address. Agent/gateway identities, service names and paths are unchanged.
var legacyMachineSources = []struct{ old, id, endpoint string }{
	{"rcd93-30", "rcd-30", "opc.tcp://192.168.10.30:4841"},
	{"real-lower", "rcd-46", "opc.tcp://192.168.10.46:4841"},
	{"rcd92-58", "rcd-58", "opc.tcp://192.168.10.58:4841"},
}

type SourceIDMigration struct {
	Sources             map[string]string `json:"sources"`
	Groups              map[string]string `json:"groups"`
	CalculationVersions int               `json:"calculation_versions"`
	DraftChanged        bool              `json:"draft_changed"`
}

// NormalizeLegacySourceIDs requires a stopped manager and consistent backup.
// Only current references move. Immutable versions/audits retain original IDs.
// There is no legacy alias, fallback lookup or schema change.
func (r *Repository) NormalizeLegacySourceIDs(ctx context.Context) (SourceIDMigration, error) {
	result := SourceIDMigration{Sources: map[string]string{}, Groups: map[string]string{}}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "PRAGMA defer_foreign_keys=ON"); err != nil {
		return result, err
	}
	var pending int
	if err = tx.QueryRowContext(ctx, "SELECT (SELECT COUNT(*) FROM command_audit WHERE state IN ('Queued','Waiting'))+(SELECT COUNT(*) FROM process_control_operations WHERE state NOT IN ('succeeded','failed'))").Scan(&pending); err != nil {
		return result, err
	}
	if pending != 0 {
		return result, fmt.Errorf("unfinished command/process operation: %w", domain.ErrCommandPending)
	}
	now := encodeTime(r.clock().UTC())
	mapping := map[string]string{}
	for _, v := range legacyMachineSources {
		var endpoint string
		e := tx.QueryRowContext(ctx, "SELECT endpoint FROM sources WHERE id=?", v.old).Scan(&endpoint)
		if errors.Is(e, sql.ErrNoRows) {
			e = tx.QueryRowContext(ctx, "SELECT endpoint FROM sources WHERE id=?", v.id).Scan(&endpoint)
			if errors.Is(e, sql.ErrNoRows) {
				continue
			}
			if e != nil {
				return result, e
			}
			if endpoint != v.endpoint {
				return result, fmt.Errorf("canonical source %s endpoint differs", v.id)
			}
		} else {
			if e != nil {
				return result, e
			}
			if endpoint != v.endpoint {
				return result, fmt.Errorf("source %s endpoint differs from approved mapping", v.old)
			}
			var count int
			if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM sources WHERE id=?", v.id).Scan(&count); e != nil {
				return result, e
			}
			if count != 0 {
				return result, fmt.Errorf("source ID collision: %s", v.id)
			}
			result.Sources[v.old] = v.id
		}
		mapping[v.old] = v.id
		if v.id == "rcd-46" {
			var count int
			if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM sources WHERE id='rcd92-46'").Scan(&count); e != nil {
				return result, e
			}
			if count != 0 {
				return result, fmt.Errorf("rcd92-46 is a configured source, not an imported typo")
			}
			mapping["rcd92-46"] = v.id
		}
	}
	for old, id := range result.Sources {
		for _, table := range []string{"catalog_nodes", "source_topology", "process_bindings", "source_station_version_state", "station_version_cache", "lower_log_ingest_cursor", "command_trace_cursor", "software_version_cache", "fleet_binding"} {
			if _, err = tx.ExecContext(ctx, "UPDATE "+table+" SET source_id=? WHERE source_id=?", id, old); err != nil {
				return result, fmt.Errorf("migrate %s: %w", table, err)
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE sources SET id=?,revision=revision+1,updated_at=? WHERE id=?", id, now, old); err != nil {
			return result, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE groups SET default_source_id=?,revision=revision+1,updated_at=? WHERE default_source_id=?", id, now, old); err != nil {
			return result, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE software_version_cache SET snapshot=json_set(snapshot,'$.source_id',?) WHERE source_id=?", id, id); err != nil {
			return result, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE alarms SET source_id=?,revision=revision+1,updated_at=? WHERE source_id=? AND state='Open'", id, now, old); err != nil {
			return result, err
		}
	}
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM groups WHERE id='real-hardware'").Scan(&count); err != nil {
		return result, err
	}
	if count != 0 {
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM groups WHERE id='serial-acquisition'").Scan(&count); err != nil {
			return result, err
		}
		if count != 0 {
			return result, fmt.Errorf("group ID collision: serial-acquisition")
		}
		result.Groups["real-hardware"] = "serial-acquisition"
		if err = remapGroup(ctx, tx, now); err != nil {
			return result, err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE groups SET name='机组数据同步',revision=revision+1,updated_at=? WHERE id='unit-sync-46' AND name<>'机组数据同步'", now); err != nil {
		return result, err
	}
	if err = remapRedundancy(ctx, tx, mapping, result.Groups); err != nil {
		return result, err
	}
	if result.CalculationVersions, result.DraftChanged, err = remapStoredCalculations(ctx, tx, mapping, result.Groups, now); err != nil {
		return result, err
	}
	if len(result.Sources) != 0 || len(result.Groups) != 0 || result.DraftChanged || result.CalculationVersions != 0 {
		raw, e := json.Marshal(result)
		if e != nil {
			return result, e
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO management_audit(kind,actor,target,revision,details,occurred_at) VALUES('source.id_migration','offline-maintenance','machine-sources',1,?,?)", string(raw), now); err != nil {
			return result, err
		}
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

func remapGroup(ctx context.Context, tx *sql.Tx, now string) error {
	for _, query := range []string{
		"UPDATE sources SET group_id='serial-acquisition',revision=revision+1,updated_at=? WHERE group_id='real-hardware'",
		"UPDATE groups SET id='serial-acquisition',name='串口采集组',revision=revision+1,updated_at=? WHERE id='real-hardware'",
		"UPDATE alarms SET kind='group/serial-acquisition/data_difference',revision=revision+1,updated_at=? WHERE kind='group/real-hardware/data_difference' AND state='Open'",
	} {
		if _, err := tx.ExecContext(ctx, query, now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE catalog_nodes SET group_id='serial-acquisition' WHERE group_id='real-hardware'"); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT id,group_bindings FROM sources WHERE group_bindings<>'[]'")
	if err != nil {
		return err
	}
	type item struct{ id, raw string }
	var items []item
	for rows.Next() {
		var v item
		if err = rows.Scan(&v.id, &v.raw); err != nil {
			rows.Close()
			return err
		}
		items = append(items, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range items {
		var bindings []domain.GroupBinding
		if err = json.Unmarshal([]byte(v.raw), &bindings); err != nil {
			return err
		}
		changed := false
		for i := range bindings {
			if bindings[i].GroupID == "real-hardware" {
				bindings[i].GroupID = "serial-acquisition"
				changed = true
			}
		}
		if changed {
			raw, e := json.Marshal(bindings)
			if e != nil {
				return e
			}
			if _, err = tx.ExecContext(ctx, "UPDATE sources SET group_bindings=?,revision=revision+1,updated_at=? WHERE id=?", string(raw), now, v.id); err != nil {
				return err
			}
		}
	}
	return nil
}

func remapRedundancy(ctx context.Context, tx *sql.Tx, mapping, groups map[string]string) error {
	rows, err := tx.QueryContext(ctx, "SELECT policy_json FROM group_redundancy")
	if err != nil {
		return err
	}
	var policies []domain.RedundancyPolicy
	for rows.Next() {
		var raw string
		var p domain.RedundancyPolicy
		if err = rows.Scan(&raw); err == nil {
			err = json.Unmarshal([]byte(raw), &p)
		}
		if err != nil {
			rows.Close()
			return err
		}
		policies = append(policies, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range policies {
		oldGroup := p.GroupID
		changed := false
		if id, ok := groups[p.GroupID]; ok {
			p.GroupID = id
			changed = true
		}
		remap := func(id *string) {
			if next, ok := mapping[*id]; ok {
				*id, changed = next, true
			}
		}
		for i := range p.Priority {
			remap(&p.Priority[i])
		}
		remap(&p.LeftSourceID)
		remap(&p.RightSourceID)
		if !changed {
			continue
		}
		if err = p.Validate(); err != nil {
			return err
		}
		p.Revision++
		raw, e := json.Marshal(p)
		if e != nil {
			return e
		}
		if _, err = tx.ExecContext(ctx, "UPDATE group_redundancy SET group_id=?,revision=?,policy_json=? WHERE group_id=?", p.GroupID, p.Revision, string(raw), oldGroup); err != nil {
			return err
		}
	}
	return nil
}

func remapStoredCalculations(ctx context.Context, tx *sql.Tx, sources, groups map[string]string, now string) (int, bool, error) {
	var content string
	if err := tx.QueryRowContext(ctx, "SELECT content FROM calculation_draft WHERE id=1").Scan(&content); err != nil {
		return 0, false, err
	}
	updated, draftChanged, err := calculation.RemapRoutingIDs(content, sources, groups)
	if err != nil {
		return 0, false, fmt.Errorf("draft must be parseable before migration: %w", err)
	}
	if draftChanged {
		cfg, e := calculation.ParseConfig(updated)
		if e != nil {
			return 0, false, e
		}
		problems, _ := json.Marshal([]calculation.Problem{{Field: "state_inputs", Message: "来源及分组已迁移，请重新校验草稿"}})
		if _, err = tx.ExecContext(ctx, "UPDATE calculation_draft SET revision=revision+1,content=?,content_hash=?,valid=0,problems=?,updated_at=? WHERE id=1", updated, cfg.Hash, string(problems), now); err != nil {
			return 0, false, err
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT id,content,state,rule_count FROM calculation_versions WHERE state IN ('active','pending_restart') ORDER BY revision")
	if err != nil {
		return 0, false, err
	}
	type entry struct {
		id             int64
		content, state string
		count          int
	}
	var entries []entry
	for rows.Next() {
		var v entry
		if err = rows.Scan(&v.id, &v.content, &v.state, &v.count); err != nil {
			rows.Close()
			return 0, false, err
		}
		entries = append(entries, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, false, err
	}
	count := 0
	for _, v := range entries {
		updated, changed, e := calculation.RemapRoutingIDs(v.content, sources, groups)
		if e != nil {
			return 0, false, e
		}
		if !changed {
			continue
		}
		cfg, e := calculation.ParseConfig(updated)
		if e != nil {
			return 0, false, e
		}
		if _, err = tx.ExecContext(ctx, "UPDATE calculation_versions SET state='archived' WHERE id=?", v.id); err != nil {
			return 0, false, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO calculation_versions(revision,content,content_hash,rule_count,state,actor,created_at,activated_at) SELECT COALESCE(MAX(revision),0)+1,?,?,?,?,'source-id-migration',?,? FROM calculation_versions", updated, cfg.Hash, v.count, v.state, now, now); err != nil {
			return 0, false, err
		}
		count++
	}
	return count, draftChanged, nil
}

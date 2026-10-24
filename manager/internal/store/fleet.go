package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"rcd_manager/internal/domain"
	"time"
)

func (r *Repository) SaveFleetSnapshot(ctx context.Context, platform string, snapshot domain.FleetSnapshot) (time.Time, error) {
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return time.Time{}, err
	}
	at := r.clock().UTC()
	_, err = r.db.ExecContext(ctx, `INSERT INTO fleet_snapshot(platform,snapshot,received_at) VALUES(?,?,?) ON CONFLICT(platform) DO UPDATE SET snapshot=excluded.snapshot,received_at=excluded.received_at`, platform, string(raw), encodeTime(at))
	return at, err
}
func (r *Repository) LoadFleetSnapshot(ctx context.Context, platform string) (domain.FleetSnapshot, time.Time, error) {
	var snapshot domain.FleetSnapshot
	var raw, stamp string
	err := r.db.QueryRowContext(ctx, `SELECT snapshot,received_at FROM fleet_snapshot WHERE platform=?`, platform).Scan(&raw, &stamp)
	if errors.Is(err, sql.ErrNoRows) {
		return snapshot, time.Time{}, domain.ErrNotFound
	}
	if err != nil {
		return snapshot, time.Time{}, err
	}
	if err = json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return snapshot, time.Time{}, err
	}
	at, err := decodeTime(stamp)
	return snapshot, at, err
}
func (r *Repository) FleetBinding(ctx context.Context, sourceID string) (domain.FleetSourceStatus, string, error) {
	value := domain.FleetSourceStatus{SourceID: sourceID}
	var platform, raw, stamp string
	err := r.db.QueryRowContext(ctx, `SELECT platform,device_id,machine,applied_at FROM fleet_binding WHERE source_id=?`, sourceID).Scan(&platform, &value.DeviceID, &raw, &stamp)
	if errors.Is(err, sql.ErrNoRows) {
		return value, platform, domain.ErrNotFound
	}
	if err != nil {
		return value, platform, err
	}
	err = json.Unmarshal([]byte(raw), &value.Machine)
	if err != nil {
		return value, platform, err
	}
	value.AppliedAt, err = decodeTime(stamp)
	return value, platform, err
}

// Changes only the address/label and process binding. Credentials, logical
// groups, calculation and station configuration remain untouched. Caller has
// verified both physical identities and serialized route/command admission.
func (r *Repository) ApplyFleetMachine(ctx context.Context, platform string, current domain.Source, binding domain.ProcessBinding, machine domain.FleetMachine) (domain.Source, error) {
	if err := machine.Validate(); err != nil {
		return current, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return current, err
	}
	defer tx.Rollback()
	var priorPlatform, priorRaw string
	err = tx.QueryRowContext(ctx, `SELECT platform,machine FROM fleet_binding WHERE source_id=?`, current.ID).Scan(&priorPlatform, &priorRaw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return current, err
	}
	if err == nil {
		var previous domain.FleetMachine
		if json.Unmarshal([]byte(priorRaw), &previous) != nil || previous.ID != machine.ID || previous.AgentInstanceID != machine.AgentInstanceID || previous.GatewayInstanceID != machine.GatewayInstanceID || previous.ManagerInstanceID != machine.ManagerInstanceID || priorPlatform != platform {
			return current, fmt.Errorf("台账稳定身份变化，须人工核对，不自动替换来源")
		}
		if machine.Revision < previous.Revision {
			return current, fmt.Errorf("台账 revision 回退，保留最后有效绑定")
		}
		if machine.Revision == previous.Revision && !machine.SameBinding(previous) {
			return current, fmt.Errorf("同一台账 revision 含不同绑定，保留最后有效绑定")
		}
	}
	var pending int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM process_control_operations WHERE source_id=? AND state NOT IN ('succeeded','failed')`, current.ID).Scan(&pending); err != nil {
		return current, err
	}
	if pending > 0 {
		return current, processBusy()
	}
	var br uint64
	var aid string
	err = tx.QueryRowContext(ctx, `SELECT revision,instance_id FROM process_bindings WHERE source_id=?`, current.ID).Scan(&br, &aid)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return current, err
	}
	if br != binding.Revision || (aid != "" && aid != machine.AgentInstanceID) {
		return current, domain.ErrRevisionConflict
	}
	at := r.clock().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE sources SET name=?,endpoint=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, machine.Name, machine.OPCEndpoint, encodeTime(at), current.ID, current.Revision)
	if err != nil {
		return current, err
	}
	if err = requireMutationTx(ctx, tx, result, "sources", current.ID, current.Revision); err != nil {
		return current, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO process_bindings(source_id,agent_url,instance_id,revision,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(source_id) DO UPDATE SET agent_url=excluded.agent_url,instance_id=excluded.instance_id,revision=excluded.revision,updated_at=excluded.updated_at`, current.ID, machine.AgentURL, machine.AgentInstanceID, br+1, encodeTime(at))
	if err != nil {
		return current, err
	}
	raw, _ := json.Marshal(machine)
	_, err = tx.ExecContext(ctx, `INSERT INTO fleet_binding(source_id,platform,device_id,machine,applied_at) VALUES(?,?,?,?,?) ON CONFLICT(source_id) DO UPDATE SET machine=excluded.machine,applied_at=excluded.applied_at`, current.ID, platform, machine.ID, string(raw), encodeTime(at))
	if err != nil {
		return current, err
	}
	if err = tx.Commit(); err != nil {
		return current, err
	}
	current.Name, current.Endpoint, current.Revision, current.UpdatedAt = machine.Name, machine.OPCEndpoint, current.Revision+1, at
	return current, nil
}

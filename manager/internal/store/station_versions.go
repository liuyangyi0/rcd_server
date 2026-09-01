package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"rcd_manager/internal/domain"
)

func (r *Repository) SaveSourceStationVersions(ctx context.Context, snapshot domain.SourceStationVersions) error {
	if strings.TrimSpace(snapshot.SourceID) == "" || strings.TrimSpace(snapshot.TopologyFingerprint) == "" {
		return &domain.FieldError{Field: "station_versions", Message: "source and topology fingerprint are required"}
	}
	if !domain.ValidOperationRevision(snapshot.Revision) || !domain.ValidStationRevision(snapshot.StationRevision) {
		return &domain.FieldError{Field: "station_versions.revision", Message: "invalid lower revision"}
	}
	if snapshot.ObservedAt.IsZero() {
		snapshot.ObservedAt = r.clock().UTC()
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin station version cache update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO source_station_version_state
		(source_id, topology_fingerprint, lower_revision, station_revision, software_version, build_commit, observed_at)
		VALUES (?,?,?,?,?,?,?) ON CONFLICT(source_id) DO UPDATE SET
		topology_fingerprint=excluded.topology_fingerprint, lower_revision=excluded.lower_revision,
		station_revision=excluded.station_revision, software_version=excluded.software_version,
		build_commit=excluded.build_commit, observed_at=excluded.observed_at`, snapshot.SourceID,
		snapshot.TopologyFingerprint, snapshot.Revision, snapshot.StationRevision, snapshot.SoftwareVersion,
		snapshot.BuildCommit, encodeTime(snapshot.ObservedAt)); err != nil {
		return fmt.Errorf("save station version source state: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM station_version_cache WHERE source_id=?`, snapshot.SourceID); err != nil {
		return fmt.Errorf("replace station version cache: %w", err)
	}
	seen := make(map[string]struct{})
	for _, port := range snapshot.Ports {
		if strings.TrimSpace(port.PortID) == "" {
			return &domain.FieldError{Field: "station_versions.port_id", Message: "must not be empty"}
		}
		for _, device := range port.Devices {
			key := port.PortID + "\x00" + device.DeviceID
			if _, duplicate := seen[key]; duplicate {
				return &domain.FieldError{Field: "station_versions.device_id", Message: "duplicate physical station"}
			}
			seen[key] = struct{}{}
			if !domain.ValidModuleType(device.ConfiguredModuleType) ||
				(device.DetectedModuleType != "" && !domain.ValidModuleType(device.DetectedModuleType)) ||
				!domain.ValidVersionPayload(device.VersionPayload) {
				return &domain.FieldError{Field: "station_versions.device", Message: "invalid module type or version payload"}
			}
			var attempted, succeeded any
			if !device.LastVersionAttemptAt.IsZero() {
				attempted = encodeTime(device.LastVersionAttemptAt)
			}
			if !device.LastVersionSuccessAt.IsZero() {
				succeeded = encodeTime(device.LastVersionSuccessAt)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO station_version_cache
				(source_id,topology_fingerprint,port_id,port_name,port_enabled,port_applied_enabled,port_open,
				control_pending,pending_commands,device_id,device_name,read_enabled,applied_read_enabled,
				communication_ok,configured_module_type,detected_module_type,type_mismatch,
				communication_module_version,communication_module_version_raw,plc_version,plc_version_raw,
				version_payload,version_state,last_version_attempt_at,last_version_success_at,last_version_error,
				consecutive_failures,stale) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				snapshot.SourceID, snapshot.TopologyFingerprint, port.PortID, port.PortName, port.Enabled,
				port.AppliedEnabled, port.Open, port.ControlPending, port.PendingCommands, device.DeviceID,
				device.DeviceName, device.ReadEnabled, device.AppliedReadEnabled, device.CommunicationOK,
				device.ConfiguredModuleType, device.DetectedModuleType, device.TypeMismatch,
				device.CommunicationModuleVersion, device.CommunicationModuleVersionRaw, device.PLCVersion,
				device.PLCVersionRaw, device.VersionPayload, device.VersionState, attempted, succeeded,
				device.LastVersionError, device.ConsecutiveFailures, device.Stale); err != nil {
				return fmt.Errorf("save station version %s/%s: %w", port.PortID, device.DeviceID, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit station version cache: %w", err)
	}
	return nil
}

func (r *Repository) LoadSourceStationVersions(ctx context.Context, sourceID string) (domain.SourceStationVersions, error) {
	var snapshot domain.SourceStationVersions
	var observed string
	err := r.db.QueryRowContext(ctx, `SELECT source_id,topology_fingerprint,lower_revision,station_revision,
		software_version,build_commit,observed_at FROM source_station_version_state WHERE source_id=?`, sourceID).
		Scan(&snapshot.SourceID, &snapshot.TopologyFingerprint, &snapshot.Revision, &snapshot.StationRevision,
			&snapshot.SoftwareVersion, &snapshot.BuildCommit, &observed)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SourceStationVersions{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.SourceStationVersions{}, fmt.Errorf("load station version source state: %w", err)
	}
	snapshot.ObservedAt, err = decodeTime(observed)
	if err != nil {
		return domain.SourceStationVersions{}, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT port_id,port_name,port_enabled,port_applied_enabled,port_open,
		control_pending,pending_commands,device_id,device_name,read_enabled,applied_read_enabled,communication_ok,
		configured_module_type,detected_module_type,type_mismatch,communication_module_version,
		communication_module_version_raw,plc_version,plc_version_raw,version_payload,version_state,
		last_version_attempt_at,last_version_success_at,last_version_error,consecutive_failures,stale
		FROM station_version_cache WHERE source_id=? AND topology_fingerprint=? ORDER BY port_id, device_id`,
		sourceID, snapshot.TopologyFingerprint)
	if err != nil {
		return domain.SourceStationVersions{}, fmt.Errorf("load station version cache: %w", err)
	}
	defer rows.Close()
	ports := make(map[string]int)
	for rows.Next() {
		var port domain.StationVersionPort
		var device domain.StationVersion
		var attempted, succeeded sql.NullString
		var communicationRaw, plcRaw sql.NullInt64
		if err := rows.Scan(&port.PortID, &port.PortName, &port.Enabled, &port.AppliedEnabled, &port.Open,
			&port.ControlPending, &port.PendingCommands, &device.DeviceID, &device.DeviceName,
			&device.ReadEnabled, &device.AppliedReadEnabled, &device.CommunicationOK,
			&device.ConfiguredModuleType, &device.DetectedModuleType, &device.TypeMismatch,
			&device.CommunicationModuleVersion, &communicationRaw, &device.PLCVersion,
			&plcRaw, &device.VersionPayload, &device.VersionState, &attempted, &succeeded,
			&device.LastVersionError, &device.ConsecutiveFailures, &device.Stale); err != nil {
			return domain.SourceStationVersions{}, err
		}
		if communicationRaw.Valid {
			value := uint16(communicationRaw.Int64)
			device.CommunicationModuleVersionRaw = &value
		}
		if plcRaw.Valid {
			value := uint16(plcRaw.Int64)
			device.PLCVersionRaw = &value
		}
		if attempted.Valid {
			device.LastVersionAttemptAt, err = decodeTime(attempted.String)
			if err != nil {
				return domain.SourceStationVersions{}, err
			}
		}
		if succeeded.Valid {
			device.LastVersionSuccessAt, err = decodeTime(succeeded.String)
			if err != nil {
				return domain.SourceStationVersions{}, err
			}
		}
		index, ok := ports[port.PortID]
		if !ok {
			index = len(snapshot.Ports)
			ports[port.PortID] = index
			port.Devices = []domain.StationVersion{}
			snapshot.Ports = append(snapshot.Ports, port)
		}
		snapshot.Ports[index].Devices = append(snapshot.Ports[index].Devices, device)
	}
	if err := rows.Err(); err != nil {
		return domain.SourceStationVersions{}, err
	}
	snapshot.Cached = true
	return snapshot, nil
}

func (r *Repository) ClearSourceStationVersions(ctx context.Context, sourceID string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin clear station version cache: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM station_version_cache WHERE source_id=?`, sourceID); err != nil {
		return fmt.Errorf("clear station version cache: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM source_station_version_state WHERE source_id=?`, sourceID); err != nil {
		return fmt.Errorf("clear station version source state: %w", err)
	}
	return tx.Commit()
}

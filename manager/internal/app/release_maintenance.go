package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"rcd_manager/internal/domain"
)

// This fence only concerns this manager's command admission. It does not stop
// lower acquisition, alter Session routes, or invent a second pending index.
// The separate durable file intentionally survives binary-only recovery.
type managerReleaseState struct {
	SchemaVersion int       `json:"schema_version"`
	InstanceID    string    `json:"instance_id"`
	Revision      uint64    `json:"revision"`
	DeploymentID  string    `json:"deployment_id"`
	Active        bool      `json:"active"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type managerReleaseSnapshot struct {
	managerReleaseState
	Version           string `json:"version"`
	PendingCommands   int    `json:"pending_commands"`
	ActiveDispatches  int64  `json:"active_dispatches"`
	PendingOperations int    `json:"pending_operations"`
	Drained           bool   `json:"drained"`
}

type managerReleaseRequest struct {
	Action           string `json:"action"`
	InstanceID       string `json:"instance_id"`
	DeploymentID     string `json:"deployment_id"`
	ExpectedRevision uint64 `json:"expected_revision"`
}

func (r *Runtime) loadReleaseState(instance string) error {
	r.release = managerReleaseState{SchemaVersion: 1, InstanceID: instance, Revision: 1}
	if r.options.DataDir == "" {
		return nil
	}
	path := filepath.Join(r.options.DataDir, "release-maintenance.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved managerReleaseState
	if err = json.Unmarshal(data, &saved); err != nil {
		return err
	}
	if saved.SchemaVersion != 1 || saved.InstanceID != instance || saved.Revision == 0 ||
		(saved.Active && !domain.ValidOperationRevision(saved.DeploymentID)) {
		return errors.New("invalid manager release fence; refusing to discard maintenance")
	}
	r.release = saved
	return nil
}

func (r *Runtime) releaseSnapshotLocked() managerReleaseSnapshot {
	v := managerReleaseSnapshot{managerReleaseState: r.release, Version: r.options.Version,
		PendingCommands: r.routes.TotalPending(), ActiveDispatches: r.releaseDispatches.Load(), PendingOperations: r.maintenance.activeCount()}
	v.Drained = v.Active && v.PendingCommands == 0 && v.ActiveDispatches == 0 && v.PendingOperations == 0
	return v
}

func (r *Runtime) releaseSnapshot() managerReleaseSnapshot {
	r.configMu.RLock()
	defer r.configMu.RUnlock()
	return r.releaseSnapshotLocked()
}

func (r *Runtime) changeRelease(req managerReleaseRequest) (managerReleaseSnapshot, error) {
	r.configMu.Lock()
	defer r.configMu.Unlock()
	if req.InstanceID != r.release.InstanceID || req.ExpectedRevision != r.release.Revision ||
		!domain.ValidOperationRevision(req.DeploymentID) || (req.Action != "enter" && req.Action != "complete") {
		return r.releaseSnapshotLocked(), domain.ErrRevisionConflict
	}
	if r.closing.Load() {
		return r.releaseSnapshotLocked(), domain.ErrShuttingDown
	}
	if r.release.Active && r.release.DeploymentID != req.DeploymentID {
		return r.releaseSnapshotLocked(), domain.ErrCommandPending
	}
	if req.Action == "complete" {
		if r.release.DeploymentID != req.DeploymentID || (r.release.Active && !r.releaseSnapshotLocked().Drained) {
			return r.releaseSnapshotLocked(), domain.ErrCommandPending
		}
		if !r.release.Active {
			return r.releaseSnapshotLocked(), nil
		}
	} else if r.release.Active {
		return r.releaseSnapshotLocked(), nil
	} else if r.release.DeploymentID == req.DeploymentID {
		return r.releaseSnapshotLocked(), domain.ErrRevisionConflict
	}
	next := r.release
	next.Active = req.Action == "enter"
	next.DeploymentID = req.DeploymentID
	next.Revision++
	if next.Revision == 0 {
		return r.releaseSnapshotLocked(), errors.New("release revision exhausted")
	}
	next.UpdatedAt = time.Now().UTC()
	// Fail closed even when rename succeeded but directory sync failed.
	if next.Active {
		r.release.Active = true
	}
	if err := r.saveReleaseState(next); err != nil {
		return r.releaseSnapshotLocked(), err
	}
	r.release = next
	return r.releaseSnapshotLocked(), nil
}

func (r *Runtime) saveReleaseState(v managerReleaseState) error {
	if r.options.DataDir == "" {
		return errors.New("manager release requires a persistent data directory")
	}
	file, err := os.CreateTemp(r.options.DataDir, ".release-maintenance-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err = file.Chmod(0600); err == nil {
		err = json.NewEncoder(file).Encode(v)
	}
	if err == nil {
		err = file.Sync()
	}
	if err == nil {
		err = file.Close()
	}
	if err == nil {
		err = os.Rename(file.Name(), filepath.Join(r.options.DataDir, "release-maintenance.json"))
	}
	if err != nil {
		return fmt.Errorf("persist manager release revision %s: %w", strconv.FormatUint(v.Revision, 10), err)
	}
	dir, err := os.Open(r.options.DataDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

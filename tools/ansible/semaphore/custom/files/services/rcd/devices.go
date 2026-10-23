// SPDX-License-Identifier: MIT
package rcd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var hostAliasPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

type DeviceBinding struct {
	OPCEndpoint       string `json:"opc_endpoint,omitempty"`
	AgentURL          string `json:"agent_url,omitempty"`
	AgentInstanceID   string `json:"agent_instance_id,omitempty"`
	GatewayInstanceID string `json:"gateway_instance_id,omitempty"`
	ManagerInstanceID string `json:"manager_instance_id,omitempty"`
	ControlTemplateID int    `json:"control_template_id,omitempty"`
	ReleaseTemplateID int    `json:"release_template_id,omitempty"`
	Name              string `json:"name"`
	Address           string `json:"address"`
	HostAlias         string `json:"host_alias"`
	InventoryID       int    `json:"inventory_id"`
	TemplateID        int    `json:"template_id"`
	WebURL            string `json:"web_url"`
}

func (b DeviceBinding) Validate() error {
	if len(strings.TrimSpace(b.Name)) == 0 || len(b.Name) > 128 || net.ParseIP(b.Address) == nil || !hostAliasPattern.MatchString(b.HostAlias) || b.TemplateID < 1 || b.InventoryID < 1 {
		return ErrInvalid
	}
	if b.WebURL != "" {
		u, err := url.Parse(b.WebURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Hostname() != b.Address || u.RawQuery != "" || u.Fragment != "" {
			return ErrInvalid
		}
	}
	return nil
}

type Device struct {
	ID                string `json:"id"`
	ActiveOperationID string `json:"active_operation_id"`
	DeviceBinding
	Revision      int                    `json:"revision"`
	CreatedBy     string                 `json:"created_by"`
	LastTaskID    int                    `json:"last_task_id"`
	PendingTaskID int                    `json:"pending_task_id"`
	LastAttemptAt string                 `json:"last_attempt_at"`
	LastSuccessAt string                 `json:"last_success_at"`
	LastError     string                 `json:"last_error"`
	Components    []ComponentObservation `json:"components"`
	HostStatus    *HostStatus            `json:"host_status,omitempty"`
	State         string                 `json:"state"`
}

func (d *Device) SetFreshness(now time.Time) {
	d.State = "never_checked"
	if d.LastError != "" {
		d.State = "check_failed"
		return
	}
	at, err := time.Parse(time.RFC3339Nano, d.LastSuccessAt)
	if err != nil {
		return
	}
	d.State = "checked"
	if now.Sub(at) > 180*time.Second || now.Before(at.Add(-5*time.Second)) {
		d.State = "stale"
	}
}
func (d Device) ETag() string { return fmt.Sprintf(`"%d"`, d.Revision) }

const deviceColumns = "id,binding,revision,created_by,last_task_id,last_attempt_at,last_success_at,last_error,components,host_status"

func scanDevice(row scanner) (d Device, err error) {
	var raw, components, host string
	err = row.Scan(&d.ID, &raw, &d.Revision, &d.CreatedBy, &d.LastTaskID, &d.LastAttemptAt, &d.LastSuccessAt, &d.LastError, &components, &host)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return
	}
	if err = json.Unmarshal([]byte(raw), &d.DeviceBinding); err != nil {
		return
	}
	err = json.Unmarshal([]byte(components), &d.Components)
	if host != "" && host != "null" {
		var status HostStatus
		if json.Unmarshal([]byte(host), &status) == nil {
			d.HostStatus = &status
		}
	}
	d.SetFreshness(time.Now())
	return
}
func (r *Repository) Device(ctx context.Context, project int, id string) (Device, error) {
	d, err := scanDevice(r.DB.QueryRowContext(ctx, r.Bind("select "+deviceColumns+" from rcd_device where project_id=? and id=?"), project, id))
	if err != nil {
		return d, err
	}
	err = r.DB.QueryRowContext(ctx, r.Bind("select operation_id from rcd_operation_lock where project_id=? and device_id=?"), project, id).Scan(&d.ActiveOperationID)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return d, err
}

func (r *Repository) BindReleaseTemplate(ctx context.Context, project int, id string, revision, template int) (Device, error) {
	d, err := r.Device(ctx, project, id)
	if err != nil {
		return d, err
	}
	if d.ActiveOperationID != "" || d.Revision != revision || template < 1 {
		return d, ErrConflict
	}
	d.ReleaseTemplateID = template
	raw, _ := json.Marshal(d.DeviceBinding)
	res, err := r.DB.ExecContext(ctx, r.Bind("update rcd_device set binding=?,revision=revision+1 where project_id=? and id=? and revision=? and not exists (select 1 from rcd_operation_lock where project_id=? and device_id=?)"), string(raw), project, id, revision, project, id)
	if err != nil {
		return d, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return d, err
	}
	if n != 1 {
		return d, ErrConflict
	}
	return r.Device(ctx, project, id)
}
func (r *Repository) DeviceForTemplate(ctx context.Context, project, template int) (Device, error) {
	return scanDevice(r.DB.QueryRowContext(ctx, r.Bind("select "+deviceColumns+" from rcd_device where project_id=? and template_id=?"), project, template))
}

func (r *Repository) updateDeviceBinding(ctx context.Context, project int, d Device, revision int) (Device, error) {
	raw, err := json.Marshal(d.DeviceBinding)
	if err != nil {
		return d, err
	}
	res, err := r.DB.ExecContext(ctx, r.Bind("update rcd_device set binding=?,revision=revision+1 where project_id=? and id=? and revision=? and not exists (select 1 from rcd_operation_lock where project_id=? and device_id=?)"), string(raw), project, d.ID, revision, project, d.ID)
	if err != nil {
		return d, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return d, err
	}
	if n != 1 {
		return d, ErrConflict
	}
	return r.Device(ctx, project, d.ID)
}
func (r *Repository) Devices(ctx context.Context, project int) ([]Device, error) {
	rows, err := r.DB.QueryContext(ctx, r.Bind("select "+deviceColumns+" from rcd_device where project_id=? order by id"), project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Device{}
	for rows.Next() {
		d, e := scanDevice(rows)
		if e != nil {
			return nil, e
		}
		result = append(result, d)
	}
	return result, rows.Err()
}
func (r *Repository) AddDevice(ctx context.Context, project int, b DeviceBinding, actor string) (Device, error) {
	// Physical identity and control templates have separate verified bind APIs.
	if b.ControlTemplateID != 0 || b.AgentURL != "" || b.OPCEndpoint != "" || b.AgentInstanceID != "" || b.GatewayInstanceID != "" || b.ManagerInstanceID != "" {
		return Device{}, ErrInvalid
	}
	if err := b.Validate(); err != nil {
		return Device{}, err
	}
	raw, _ := json.Marshal(b)
	d := Device{ID: randomID(), DeviceBinding: b, Revision: 1, CreatedBy: actor, Components: []ComponentObservation{}}
	_, err := r.DB.ExecContext(ctx, r.Bind("insert into rcd_device(project_id,id,template_id,inventory_id,host_alias,binding,revision,created_by,last_task_id,last_attempt_at,last_success_at,last_error,components) values(?,?,?,?,?,?,1,?,0,'','','','[]')"), project, d.ID, b.TemplateID, b.InventoryID, b.HostAlias, string(raw), actor)
	if err != nil {
		var count int
		check := r.DB.QueryRowContext(ctx, r.Bind("select count(*) from rcd_device where project_id=? and (template_id=? or (inventory_id=? and host_alias=?))"), project, b.TemplateID, b.InventoryID, b.HostAlias).Scan(&count)
		if check == nil && count > 0 {
			return d, ErrConflict
		}
		return d, err
	}
	return r.Device(ctx, project, d.ID)
}

// AcceptObservation is only called from the native task finalizer, never from a
// browser report-upload endpoint. A failed check retains the last good snapshot.
// Native task IDs order observations so a delayed old result cannot go backwards.
func (r *Repository) AcceptObservation(ctx context.Context, project int, id string, task int, attempt time.Time, components []ComponentObservation, checkError string, host ...*HostStatus) error {
	if task < 1 || attempt.IsZero() || len(checkError) > 4096 {
		return ErrInvalid
	}
	if checkError == "" && (len(components) < 3 || len(components) > 36) {
		return ErrInvalid
	}
	if checkError == "" {
		seen := map[string]bool{}
		for _, c := range components {
			if !IsComponent(c.Component) || seen[c.Component] {
				return ErrInvalid
			}
			seen[c.Component] = true
			if IsApplicationComponent(c.Component) {
				if _, e := r.GetApplication(ctx, project, c.Component); e != nil {
					return ErrInvalid
				}
			}
		}
		if !seen["manager"] || !seen["lower"] || !seen["agent"] {
			return ErrInvalid
		}
	}
	raw, err := json.Marshal(components)
	if err != nil {
		return err
	}
	stamp := attempt.UTC().Format(time.RFC3339Nano)
	query := "update rcd_device set last_task_id=?,last_attempt_at=?,last_error=?"
	args := []any{task, stamp, checkError}
	if checkError == "" {
		query += ",last_success_at=?,components=?"
		args = append(args, stamp, string(raw))
		if len(host) > 1 {
			return ErrInvalid
		}
		if len(host) == 1 && host[0] != nil {
			value := *host[0]
			if !value.Available {
				previous, e := r.Device(ctx, project, id)
				if e != nil {
					return e
				}
				if previous.HostStatus != nil && previous.HostStatus.Available {
					value = *previous.HostStatus
					value.Error = host[0].Error
					value.LastAttemptAt = host[0].ObservedAt
				}
			}
			hostRaw, e := json.Marshal(value)
			if e != nil || len(hostRaw) > 128<<10 {
				return ErrInvalid
			}
			query += ",host_status=?"
			args = append(args, string(hostRaw))
		}
	}
	query += " where project_id=? and id=? and last_task_id<?"
	args = append(args, project, id, task)
	result, err := r.DB.ExecContext(ctx, r.Bind(query), args...)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		if _, err = r.Device(ctx, project, id); err != nil {
			return err
		}
	}
	return nil
}

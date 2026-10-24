// SPDX-License-Identifier: MIT
package rcd

import (
	"context"
	"errors"
	"fmt"
)

// Service control reserves the native device slot, without inventing a package.
func (r *Repository) planServiceControl(ctx context.Context, project int, req OperationRequest) (Operation, error) {
	o := Operation{Request: req}
	if !req.CommandsPaused || req.PackageID != "" || req.PackageRevision != 0 || len(req.Targets) != 1 ||
		!(((req.Component == "manager" || IsApplicationComponent(req.Component)) && (req.Action == "start" || req.Action == "stop" || req.Action == "restart")) || (req.Component == "agent" && req.Action == "recover")) {
		return o, ErrInvalid
	}
	if IsApplicationComponent(req.Component) {
		a, err := r.GetApplication(ctx, project, req.Component)
		if err != nil {
			return o, err
		}
		o.Application = &a
	}
	t := req.Targets[0]
	d, err := r.Device(ctx, project, t.DeviceID)
	if err != nil {
		return o, err
	}
	if _, err = r.ActiveHostSetup(ctx, project, d.Address); err == nil {
		return o, ErrConflict
	} else if !errors.Is(err, ErrNotFound) {
		return o, err
	}
	if d.Revision != t.Revision || d.ActiveOperationID != "" {
		return o, ErrConflict
	}
	if d.ControlTemplateID < 1 || d.State != "checked" {
		return o, fmt.Errorf("%w: 需要近期软件检查和受控服务模板", ErrInvalid)
	}
	var observed *ComponentObservation
	for i := range d.Components {
		if d.Components[i].Component == req.Component {
			observed = &d.Components[i]
			break
		}
	}
	if observed == nil {
		return o, ErrInvalid
	}
	if req.Component == "agent" && observed.State != "stopped" {
		return o, fmt.Errorf("%w: 仅可恢复已确认停止的 Agent；状态不明不能强制重启", ErrInvalid)
	}
	if observed.State != "running" && observed.State != "stopped" {
		return o, fmt.Errorf("%w: 程序状态无法确认", ErrInvalid)
	}
	if (req.Action == "start" && observed.State != "stopped") || ((req.Action == "stop" || req.Action == "restart") && observed.State != "running") {
		return o, fmt.Errorf("%w: 启停动作与当前进程状态不符，请刷新", ErrInvalid)
	}
	hash := observed.DiskSHA256
	if observed.State == "running" {
		hash = observed.RunningSHA256
		if observed.DiskSHA256 != hash {
			return o, fmt.Errorf("%w: 磁盘与运行映像不同，请使用软件发布流程", ErrInvalid)
		}
	}
	if !digestPattern.MatchString(hash) {
		return o, fmt.Errorf("%w: 缺少可信程序摘要", ErrInvalid)
	}
	o.TemplateID, o.InventoryID = d.ControlTemplateID, d.InventoryID
	o.Targets = []ReleaseTarget{{DeviceID: d.ID, Name: d.Name, Address: d.Address, HostAlias: d.HostAlias, State: "pending", ExpectedBinarySHA256: hash}}
	return o, nil
}

func (r *Repository) BindControlTemplate(ctx context.Context, project int, id string, revision, template int) (Device, error) {
	d, err := r.Device(ctx, project, id)
	if err != nil {
		return d, err
	}
	if d.Revision != revision || d.ActiveOperationID != "" || template < 1 {
		return d, ErrConflict
	}
	d.ControlTemplateID = template
	return r.updateDeviceBinding(ctx, project, d, revision)
}

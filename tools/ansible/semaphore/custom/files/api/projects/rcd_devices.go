// SPDX-License-Identifier: MIT
package projects

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gorilla/mux"
	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/services/rcd"
	"github.com/semaphoreui/semaphore/services/tasks"
)

type rcdDevicesController struct {
	store db.Store
	repo  *rcd.Repository
}

func registerRCDDevices(router *mux.Router, store db.Store, repo *rcd.Repository) {
	c := &rcdDevicesController{store: store, repo: repo}
	router.HandleFunc("/fleet", c.fleet).Methods("GET")
	router.Handle("/devices/{device_id}/machine-binding", GetMustCanMiddleware(db.CanManageProjectResources)(http.HandlerFunc(c.bindMachine))).Methods("PUT")
	router.HandleFunc("/devices", c.list).Methods("GET")
	router.Handle("/devices", GetMustCanMiddleware(db.CanManageProjectResources)(http.HandlerFunc(c.add))).Methods("POST")
	router.HandleFunc("/devices/{device_id}", c.get).Methods("GET")
	router.Handle("/devices/{device_id}/inspections", GetMustCanMiddleware(db.CanRunProjectTasks)(http.HandlerFunc(c.inspect))).Methods("POST")
	router.Handle("/devices/{device_id}/release-binding", GetMustCanMiddleware(db.CanManageProjectResources)(http.HandlerFunc(c.bindRelease))).Methods("PUT")
	router.Handle("/devices/{device_id}/control-binding", GetMustCanMiddleware(db.CanManageProjectResources)(http.HandlerFunc(c.bindControl))).Methods("PUT")
}

func (c *rcdDevicesController) bindControl(w http.ResponseWriter, r *http.Request) {
	d, err := c.repo.Device(r.Context(), rcdProject(r), mux.Vars(r)["device_id"])
	if err != nil {
		deviceError(w, err)
		return
	}
	var input struct {
		Revision   int `json:"revision"`
		TemplateID int `json:"control_template_id"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1025))
	if err != nil || len(raw) > 1024 || rcd.StrictJSON(raw, &input) != nil {
		deviceError(w, rcd.ErrInvalid)
		return
	}
	if r.Header.Get("If-Match") != d.ETag() || input.Revision != d.Revision {
		deviceError(w, rcd.ErrConflict)
		return
	}
	if err = tasks.ValidateRCDControlTemplate(c.store, rcdProject(r), input.TemplateID, d.InventoryID); err != nil {
		helpers.WriteJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}
	d, err = c.repo.BindControlTemplate(r.Context(), rcdProject(r), d.ID, input.Revision, input.TemplateID)
	if err != nil {
		deviceError(w, err)
		return
	}
	w.Header().Set("ETag", d.ETag())
	helpers.WriteJSON(w, 200, d)
}

func (c *rcdDevicesController) bindRelease(w http.ResponseWriter, r *http.Request) {
	d, err := c.repo.Device(r.Context(), rcdProject(r), mux.Vars(r)["device_id"])
	if err != nil {
		deviceError(w, err)
		return
	}
	var input struct {
		Revision   int `json:"revision"`
		TemplateID int `json:"release_template_id"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1025))
	if err != nil || len(raw) > 1024 || rcd.StrictJSON(raw, &input) != nil {
		deviceError(w, rcd.ErrInvalid)
		return
	}
	if r.Header.Get("If-Match") != d.ETag() || input.Revision != d.Revision {
		deviceError(w, rcd.ErrConflict)
		return
	}
	if err = tasks.ValidateRCDReleaseTemplate(c.store, rcdProject(r), input.TemplateID, d.InventoryID); err != nil {
		helpers.WriteJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}
	d, err = c.repo.BindReleaseTemplate(r.Context(), rcdProject(r), d.ID, input.Revision, input.TemplateID)
	if err != nil {
		deviceError(w, err)
		return
	}
	w.Header().Set("ETag", d.ETag())
	helpers.WriteJSON(w, 200, d)
}
func deviceError(w http.ResponseWriter, err error) {
	// Use a generic resource label instead of the package-specific error string.
	status := 500
	message := "设备检查存储失败，请查看平台日志"
	switch {
	case errors.Is(err, rcd.ErrInvalid):
		status = 422
		message = "设备或版本条件不合法"
	case errors.Is(err, rcd.ErrConflict):
		status = 409
		message = "设备绑定已变更，请刷新后重试"
	case errors.Is(err, rcd.ErrNotFound):
		status = 404
		message = "设备不存在"
	}
	helpers.WriteJSON(w, status, map[string]string{"error": message})
}
func (c *rcdDevicesController) reconcile(project int, d rcd.Device) int {
	latest, err := c.store.GetTemplateTasks(project, d.TemplateID, db.RetrieveQueryParams{Count: 1})
	if err == nil && len(latest) > 0 {
		if !latest[0].Status.IsFinished() {
			return latest[0].ID
		}
		tasks.CollectRCDInspection(c.store, latest[0].Task)
	}
	return 0
}
func (c *rcdDevicesController) list(w http.ResponseWriter, r *http.Request) {
	page, size := 1, 20
	var err error
	if s := r.URL.Query().Get("page"); s != "" {
		page, err = strconv.Atoi(s)
		if err != nil {
			deviceError(w, rcd.ErrInvalid)
			return
		}
	}
	if s := r.URL.Query().Get("page_size"); s != "" {
		size, err = strconv.Atoi(s)
		if err != nil {
			deviceError(w, rcd.ErrInvalid)
			return
		}
	}
	q, state := strings.ToLower(r.URL.Query().Get("q")), r.URL.Query().Get("state")
	if page < 1 || page > 1000000 || (size != 20 && size != 50 && size != 100) || len(q) > 128 || (state != "" && state != "checked" && state != "stale" && state != "check_failed" && state != "never_checked") {
		deviceError(w, rcd.ErrInvalid)
		return
	}
	devices, err := c.repo.Devices(r.Context(), rcdProject(r))
	if err != nil {
		deviceError(w, err)
		return
	}
	items := []rcd.Device{}
	for _, d := range devices {
		pending := c.reconcile(rcdProject(r), d)
		d, err = c.repo.Device(r.Context(), rcdProject(r), d.ID)
		if err != nil {
			deviceError(w, err)
			return
		}
		d.PendingTaskID = pending
		if (state == "" || state == d.State) && (q == "" || strings.Contains(strings.ToLower(d.Name+" "+d.Address), q)) {
			items = append(items, d)
		}
	}
	total := len(items)
	start := (page - 1) * size
	if start > total {
		start = total
	}
	end := start + size
	if end > total {
		end = total
	}
	helpers.WriteJSON(w, 200, map[string]any{"items": items[start:end], "total": total, "page": page, "page_size": size, "total_pages": (total + size - 1) / size})
}
func (c *rcdDevicesController) add(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	var b rcd.DeviceBinding
	if err != nil || len(raw) > 4096 || rcd.StrictJSON(raw, &b) != nil || b.Validate() != nil {
		deviceError(w, rcd.ErrInvalid)
		return
	}
	if err = tasks.ValidateRCDInspectionTemplate(c.store, rcdProject(r), b); err != nil {
		helpers.WriteJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}
	if b.ReleaseTemplateID > 0 {
		if err = tasks.ValidateRCDReleaseTemplate(c.store, rcdProject(r), b.ReleaseTemplateID, b.InventoryID); err != nil {
			helpers.WriteJSON(w, 422, map[string]string{"error": err.Error()})
			return
		}
	}
	d, err := c.repo.AddDevice(r.Context(), rcdProject(r), b, rcdActor(r))
	if err != nil {
		deviceError(w, err)
		return
	}
	// Use the native persistent scheduler, not a new polling goroutine/queue.
	if _, err = c.store.CreateSchedule(db.Schedule{ProjectID: rcdProject(r), TemplateID: b.TemplateID,
		Name: "RCD 自动状态检查 · " + b.Name, CronFormat: "* * * * *", Active: true,
		TaskParams: &db.TaskParams{Environment: "{}", Message: "RCD 自动只读检查"}}); err != nil {
		helpers.WriteJSON(w, 500, map[string]any{"error": "设备已绑定，但自动检查计划创建失败，请在高级管理补建每分钟检查", "device": d})
		return
	}
	refreshSchedulePool(r)
	w.Header().Set("ETag", d.ETag())
	helpers.WriteJSON(w, 201, d)
}
func (c *rcdDevicesController) get(w http.ResponseWriter, r *http.Request) {
	d, err := c.repo.Device(r.Context(), rcdProject(r), mux.Vars(r)["device_id"])
	if err != nil {
		deviceError(w, err)
		return
	}
	pending := c.reconcile(rcdProject(r), d)
	d, err = c.repo.Device(r.Context(), rcdProject(r), d.ID)
	if err != nil {
		deviceError(w, err)
		return
	}
	d.PendingTaskID = pending
	w.Header().Set("ETag", d.ETag())
	helpers.WriteJSON(w, 200, d)
}
func (c *rcdDevicesController) inspect(w http.ResponseWriter, r *http.Request) {
	d, err := c.repo.Device(r.Context(), rcdProject(r), mux.Vars(r)["device_id"])
	if err != nil {
		deviceError(w, err)
		return
	}
	if r.Header.Get("If-Match") == "" {
		helpers.WriteJSON(w, 428, map[string]string{"error": "缺少 If-Match 版本条件"})
		return
	}
	if r.Header.Get("If-Match") != d.ETag() {
		deviceError(w, rcd.ErrConflict)
		return
	}
	var input struct {
		Revision int `json:"revision"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1025))
	if err != nil || len(raw) > 1024 || rcd.StrictJSON(raw, &input) != nil {
		deviceError(w, rcd.ErrInvalid)
		return
	}
	if input.Revision != d.Revision {
		deviceError(w, rcd.ErrConflict)
		return
	}
	user := helpers.UserFromContext(r)
	task, err := taskPool(r).AddTask(db.Task{TemplateID: d.TemplateID, Message: fmt.Sprintf("RCD 只读状态检查 · %s", d.Name)}, &user.ID, user.Username, rcdProject(r), false)
	if err != nil {
		helpers.WriteJSON(w, 409, map[string]string{"error": "检查未入队：" + err.Error()})
		return
	}
	helpers.WriteJSON(w, 202, map[string]any{"task_id": task.ID, "device_id": d.ID, "status": task.Status})
}

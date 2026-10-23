// SPDX-License-Identifier: MIT
package projects

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/services/rcd"
	"github.com/semaphoreui/semaphore/services/tasks"
)

type rcdOperationsController struct {
	store db.Store
	repo  *rcd.Repository
}

var rcdOperationCreation sync.Mutex

// Operation errors must not claim that no execution happened: a native task
// may already be durable when its association or the final response is lost.
func operationError(w http.ResponseWriter, err error) {
	operationErrorWithID(w, err, "")
}

func operationErrorWithID(w http.ResponseWriter, err error, id string) {
	status := http.StatusInternalServerError
	message := "操作状态暂时无法确认，请刷新原作业或查看原任务日志；不要重复提交"
	switch {
	case errors.Is(err, rcd.ErrNotFound):
		status, message = http.StatusNotFound, "目标设备、软件包或操作记录不存在，请刷新后核对原作业"
	case errors.Is(err, rcd.ErrConflict):
		status, message = http.StatusConflict, "操作条件或记录已变更，请刷新原作业与设备状态；不要更换编号重复提交"
	case errors.Is(err, rcd.ErrInvalid):
		status = http.StatusUnprocessableEntity
		message = "操作参数不合法，请检查组件、目标、版本及确认条件"
		if detail := strings.TrimPrefix(err.Error(), rcd.ErrInvalid.Error()+": "); detail != err.Error() {
			message = detail
		}
	default:
		log.Printf("RCD operation state: %v", err)
	}
	body := map[string]string{"error": message}
	if id != "" {
		body["operation_id"] = id
	}
	helpers.WriteJSON(w, status, body)
}

func (c *rcdOperationsController) releaseBusyReason(ctx context.Context, project int, device rcd.Device) (string, error) {
	if device.ActiveOperationID != "" {
		return "该机器存在未结束的发布操作，请先补查原事务", nil
	}
	if _, err := c.repo.ActiveHostSetup(ctx, project, device.Address); err == nil {
		return "该机器正在接入或首次安装，请等待原作业确认", nil
	} else if !errors.Is(err, rcd.ErrNotFound) {
		return "", err
	}
	return "", nil
}

func registerRCDOperations(router *mux.Router, store db.Store, repo *rcd.Repository) {
	c := &rcdOperationsController{store, repo}
	router.HandleFunc("/operations", c.list).Methods("GET")
	router.HandleFunc("/operations/{operation_id}", c.get).Methods("GET")
	router.Handle("/operations/validate", GetMustCanMiddleware(db.CanRunProjectTasks)(http.HandlerFunc(c.validate))).Methods("POST")
	router.Handle("/operations", GetMustCanMiddleware(db.CanRunProjectTasks)(http.HandlerFunc(c.create))).Methods("POST")
	router.Handle("/operations/{operation_id}/cancel", GetMustCanMiddleware(db.CanRunProjectTasks)(http.HandlerFunc(c.cancel))).Methods("POST")
	router.Handle("/operations/{operation_id}/reconcile", GetMustCanMiddleware(db.CanRunProjectTasks)(http.HandlerFunc(c.readback))).Methods("POST")
	router.HandleFunc("/packages/{package_id}/compatibility", c.compatibility).Methods("GET")
	router.HandleFunc("/devices/{device_id}/components/{component}/history", c.history).Methods("GET")
}
func (c *rcdOperationsController) compatibility(w http.ResponseWriter, r *http.Request) {
	p, err := c.repo.Get(r.Context(), rcdProject(r), mux.Vars(r)["package_id"])
	if err != nil {
		operationError(w, err)
		return
	}
	devices, err := c.repo.Devices(r.Context(), rcdProject(r))
	if err != nil {
		operationError(w, err)
		return
	}
	reasons := map[string]string{}
	for _, d := range devices {
		d, err = c.repo.Device(r.Context(), rcdProject(r), d.ID)
		if err != nil {
			operationError(w, err)
			return
		}
		reason := rcd.CompatibilityReason(d, p.Manifest)
		if !p.Enabled {
			reason = "软件包已停用"
		}
		busy, e := c.releaseBusyReason(r.Context(), rcdProject(r), d)
		if e != nil {
			operationError(w, e)
			return
		}
		if busy != "" {
			reason = busy
		}
		reasons[d.ID] = reason
	}
	helpers.WriteJSON(w, 200, map[string]any{"reasons": reasons, "package_revision": p.Revision})
}
func (c *rcdOperationsController) readback(w http.ResponseWriter, r *http.Request) {
	o, err := c.repo.Operation(r.Context(), rcdProject(r), mux.Vars(r)["operation_id"])
	if err != nil {
		operationError(w, err)
		return
	}
	if r.Header.Get("If-Match") != o.ETag() || o.KnownTerminal() || o.TaskID < 1 {
		operationError(w, rcd.ErrConflict)
		return
	}
	user := helpers.UserFromContext(r)
	ids := []int{}
	failed := []string{}
	for _, target := range o.Targets {
		d, e := c.repo.Device(r.Context(), rcdProject(r), target.DeviceID)
		if e != nil {
			failed = append(failed, target.DeviceID)
			continue
		}
		t, e := taskPool(r).AddTask(db.Task{TemplateID: d.TemplateID, Message: "RCD 补查原发布事务（只读，不重新安装）"}, &user.ID, user.Username, rcdProject(r), false)
		if e != nil {
			failed = append(failed, d.ID)
		} else {
			ids = append(ids, t.ID)
		}
	}
	helpers.WriteJSON(w, 202, map[string]any{"task_ids": ids, "failed_device_ids": failed, "message": "补查复用只读检查任务，不重启或重复安装"})
}
func operationInput(w http.ResponseWriter, r *http.Request) (rcd.OperationRequest, bool) {
	var v rcd.OperationRequest
	raw, err := io.ReadAll(io.LimitReader(r.Body, 16385))
	if err != nil || len(raw) > 16384 || rcd.StrictJSON(raw, &v) != nil {
		operationError(w, rcd.ErrInvalid)
		return v, false
	}
	expected := v.PackageRevision
	if v.Kind == "service_control" {
		if len(v.Targets) != 1 {
			operationError(w, rcd.ErrInvalid)
			return v, false
		}
		expected = v.Targets[0].Revision
	}
	if r.Header.Get("If-Match") != strconv.Quote(strconv.Itoa(expected)) {
		operationError(w, rcd.ErrConflict)
		return v, false
	}
	return v, true
}
func (c *rcdOperationsController) validate(w http.ResponseWriter, r *http.Request) {
	req, ok := operationInput(w, r)
	if !ok {
		return
	}
	o, err := c.repo.PlanOperation(r.Context(), rcdProject(r), req)
	if err == nil {
		err = tasks.ValidateRCDOperationTemplate(c.store, rcdProject(r), o)
	}
	if err != nil {
		operationError(w, err)
		return
	}
	helpers.WriteJSON(w, 200, o)
}
func (c *rcdOperationsController) create(w http.ResponseWriter, r *http.Request) {
	req, ok := operationInput(w, r)
	if !ok {
		return
	}
	user := helpers.UserFromContext(r)
	rcdOperationCreation.Lock()
	defer rcdOperationCreation.Unlock()
	o, nonce, created, err := c.repo.CreateOperation(r.Context(), rcdProject(r), r.Header.Get("Idempotency-Key"), req, user.Username, user.ID)
	if err != nil {
		operationErrorWithID(w, err, o.ID)
		return
	}
	if created {
		secret, _ := json.Marshal(map[string]string{"rcd_operation_id": o.ID, "rcd_nonce": nonce})
		_, err = taskPool(r).AddTask(db.Task{TemplateID: o.TemplateID, Secret: string(secret)}, &user.ID, user.Username, rcdProject(r), false)
		if err != nil {
			// An AddTask error may follow a durable native task creation. Do not infer
			// that no effect happened or free the device for a second operation.
			var rejected *tasks.RCDTaskAdmissionError
			saveCtx, cancelSave := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			saveErr := c.repo.RecordSubmissionFailure(saveCtx, rcdProject(r), o.ID, err.Error(), errors.As(err, &rejected))
			cancelSave()
			if saveErr != nil {
				operationErrorWithID(w, saveErr, o.ID)
				return
			}
		}
	}
	operationID := o.ID
	o, e := c.repo.Operation(r.Context(), rcdProject(r), operationID)
	if e != nil {
		operationErrorWithID(w, e, operationID)
		return
	}
	w.Header().Set("ETag", o.ETag())
	helpers.WriteJSON(w, 202, o)
}
func (c *rcdOperationsController) reconcile(r *http.Request, o rcd.Operation) rcd.Operation {
	if o.TaskID > 0 {
		if t, e := c.store.GetTask(rcdProject(r), o.TaskID); e == nil {
			tasks.CollectRCDOperation(c.store, t)
		}
	}
	fresh, e := c.repo.Operation(r.Context(), rcdProject(r), o.ID)
	if e == nil {
		return fresh
	}
	return o
}
func (c *rcdOperationsController) get(w http.ResponseWriter, r *http.Request) {
	o, err := c.repo.Operation(r.Context(), rcdProject(r), mux.Vars(r)["operation_id"])
	if err != nil {
		operationError(w, err)
		return
	}
	o = c.reconcile(r, o)
	w.Header().Set("ETag", o.ETag())
	helpers.WriteJSON(w, 200, o)
}
func operationPagination(r *http.Request) (int, int, bool) {
	page, size := 1, 20
	var err error
	if raw := r.URL.Query().Get("page"); raw != "" {
		page, err = strconv.Atoi(raw)
		if err != nil {
			return 0, 0, false
		}
	}
	if raw := r.URL.Query().Get("page_size"); raw != "" {
		size, err = strconv.Atoi(raw)
		if err != nil {
			return 0, 0, false
		}
	}
	return page, size, page > 0 && page <= 1000000 && (size == 20 || size == 50 || size == 100)
}
func (c *rcdOperationsController) list(w http.ResponseWriter, r *http.Request) {
	page, size, ok := operationPagination(r)
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	state := r.URL.Query().Get("state")
	kind := r.URL.Query().Get("kind")
	if !ok || len(q) > 128 || !validOperationState(state) || (kind != "" && kind != "release" && kind != "service_control") {
		operationError(w, rcd.ErrInvalid)
		return
	}
	items, err := c.repo.Operations(r.Context(), rcdProject(r))
	if err != nil {
		operationError(w, err)
		return
	}
	selected := []rcd.Operation{}
	for _, o := range items {
		o = c.reconcile(r, o)
		if (kind == "release" && o.ServiceControl()) || (kind == "service_control" && !o.ServiceControl()) {
			continue
		}
		if (state == "" || o.State == state) && operationMatchesSearch(o, q) {
			selected = append(selected, o)
		}
	}
	total := len(selected)
	start := (page - 1) * size
	if start > total {
		start = total
	}
	end := start + size
	if end > total {
		end = total
	}
	helpers.WriteJSON(w, 200, map[string]any{"items": selected[start:end], "total": total, "page": page, "page_size": size, "total_pages": (total + size - 1) / size})
}

func validOperationState(state string) bool {
	switch state {
	case "", "created", "queued", "running", "succeeded", "recovered", "partial_failed", "failed_before_install", "failed_before_execution", "unknown", "needs_maintenance", "recovery_failed", "cancelled":
		return true
	}
	return false
}

func operationMatchesSearch(o rcd.Operation, q string) bool {
	if q == "" {
		return true
	}
	fields := []string{o.Package.Version, o.Package.ReleaseID, o.Component(), o.Request.Action, o.Actor, o.ID, strconv.Itoa(o.TaskID)}
	for _, target := range o.Targets {
		fields = append(fields, target.Name, target.Address, target.HostAlias, target.DeviceID)
	}
	return strings.Contains(strings.ToLower(strings.Join(fields, " ")), q)
}
func (c *rcdOperationsController) cancel(w http.ResponseWriter, r *http.Request) {
	o, err := c.repo.Operation(r.Context(), rcdProject(r), mux.Vars(r)["operation_id"])
	if err != nil {
		operationError(w, err)
		return
	}
	if r.Header.Get("If-Match") != o.ETag() || o.State != "queued" {
		operationError(w, rcd.ErrConflict)
		return
	}
	o.State = "cancelled"
	o.Error = "在执行准入之前取消，未执行本次操作"
	for i := range o.Targets {
		o.Targets[i].State = "not_started"
	}
	if err = c.repo.UpdateOperation(r.Context(), rcdProject(r), o, o.Revision); err != nil {
		operationError(w, err)
		return
	}
	o.Revision++
	w.Header().Set("ETag", o.ETag())
	helpers.WriteJSON(w, 200, o)
}
func (c *rcdOperationsController) history(w http.ResponseWriter, r *http.Request) {
	device := mux.Vars(r)["device_id"]
	component := mux.Vars(r)["component"]
	if !rcd.IsComponent(component) {
		operationError(w, rcd.ErrInvalid)
		return
	}
	d, err := c.repo.Device(r.Context(), rcdProject(r), device)
	if err != nil {
		operationError(w, err)
		return
	}
	page, size, ok := operationPagination(r)
	if !ok {
		operationError(w, rcd.ErrInvalid)
		return
	}
	items, err := c.repo.Installations(r.Context(), rcdProject(r), device, component)
	if err != nil {
		operationError(w, err)
		return
	}
	busy, err := c.releaseBusyReason(r.Context(), rcdProject(r), d)
	if err != nil {
		operationError(w, err)
		return
	}
	result := []map[string]any{}
	for _, installed := range items {
		p, e := c.repo.Get(r.Context(), rcdProject(r), installed.PackageID)
		reason := ""
		if e != nil {
			reason = "软件包不存在"
		} else if !p.Enabled {
			reason = "软件包已停用"
		} else {
			reason = rcd.CompatibilityReason(d, p.Manifest)
		}
		if busy != "" {
			reason = busy
		}
		result = append(result, map[string]any{"package": p, "operation_id": installed.OperationID, "installed_at": installed.InstalledAt, "restore_disabled_reason": reason, "installation_kind": installed.Kind})
	}
	total := len(result)
	start := (page - 1) * size
	if start > total {
		start = total
	}
	end := start + size
	if end > total {
		end = total
	}
	packages, e := rcd.NewPackages(c.repo, os.Getenv("SEMAPHORE_RCD_PACKAGES_PATH"))
	if e != nil {
		operationError(w, e)
		return
	}
	for _, row := range result[start:end] {
		if row["restore_disabled_reason"] == "" {
			p := row["package"].(rcd.Package)
			f, e := packages.OpenArchive(rcdProject(r), p)
			if e != nil {
				row["restore_disabled_reason"] = "软件包完整性校验失败，禁止恢复"
			} else {
				f.Close()
			}
		}
	}
	helpers.WriteJSON(w, 200, map[string]any{"items": result[start:end], "total": total, "page": page, "page_size": size, "total_pages": (total + size - 1) / size, "checked_at": d.LastSuccessAt})
}

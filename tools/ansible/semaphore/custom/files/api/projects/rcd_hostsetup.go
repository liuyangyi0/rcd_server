// SPDX-License-Identifier: MIT
package projects

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/gorilla/mux"
	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/services/rcd"
	"github.com/semaphoreui/semaphore/services/tasks"
)

type rcdHostSetupController struct {
	store db.Store
	repo  *rcd.Repository
	queue hostSetupTaskQueue
}

type hostSetupTaskQueue interface {
	AddTask(db.Task, *int, string, int, bool) (db.Task, error)
}

var setupProvision sync.Mutex

func registerRCDHostSetups(router *mux.Router, store db.Store, repo *rcd.Repository) {
	c := &rcdHostSetupController{store: store, repo: repo}
	manage := GetMustCanMiddleware(db.CanManageProjectResources)
	run := GetMustCanMiddleware(db.CanRunProjectTasks)
	router.Handle("/host-setups/options", manage(http.HandlerFunc(c.options))).Methods("GET")
	router.Handle("/host-setups", manage(http.HandlerFunc(c.list))).Methods("GET")
	router.Handle("/host-setups", manage(run(http.HandlerFunc(c.create)))).Methods("POST")
	router.Handle("/host-setups/{setup_id}", manage(http.HandlerFunc(c.get))).Methods("GET")
	router.Handle("/host-setups/{setup_id}/host-key", manage(run(http.HandlerFunc(c.confirmKeys)))).Methods("POST")
	router.Handle("/host-setups/{setup_id}/install", manage(run(http.HandlerFunc(c.install)))).Methods("POST")
	router.Handle("/host-setups/{setup_id}/reconcile", manage(run(http.HandlerFunc(c.requestReconcile)))).Methods("POST")
	router.Handle("/host-setups/{setup_id}/cancel", manage(http.HandlerFunc(c.cancel))).Methods("POST")
	router.Handle("/devices/{device_id}/first-installs", manage(run(http.HandlerFunc(c.firstInstall)))).Methods("POST")
}
func setupError(w http.ResponseWriter, e error) {
	status := 500
	message := "主机接入失败，请查看原任务及平台日志"
	switch {
	case errors.Is(e, rcd.ErrInvalid):
		status = 422
		message = "主机、凭据、软件包或修订条件不合法"
	case errors.Is(e, rcd.ErrConflict):
		status = 409
		message = "接入状态已变化或目标已被占用；请查询原操作，不能自动重投"
	case errors.Is(e, rcd.ErrNotFound):
		status = 404
		message = "主机接入记录不存在"
	}
	helpers.WriteJSON(w, status, map[string]string{"error": message})
}

type setupKeyOption struct {
	ID   int              `json:"id"`
	Name string           `json:"name"`
	Type db.AccessKeyType `json:"type"`
}

func (c *rcdHostSetupController) options(w http.ResponseWriter, r *http.Request) {
	project := rcdProject(r)
	keys, e := c.store.GetAccessKeys(project, db.GetAccessKeyOptions{Owner: db.AccessKeyShared}, db.RetrieveQueryParams{})
	if e != nil {
		setupError(w, e)
		return
	}
	ssh := []setupKeyOption{}
	become := []setupKeyOption{}
	for _, k := range keys {
		if k.Type == db.AccessKeySSH || k.Type == db.AccessKeyLoginPassword {
			ssh = append(ssh, setupKeyOption{k.ID, k.Name, k.Type})
		}
		if k.Type == db.AccessKeyLoginPassword {
			become = append(become, setupKeyOption{k.ID, k.Name, k.Type})
		}
	}
	_, e = tasks.HostSetupSource(c.store, project)
	message := ""
	if e != nil {
		message = e.Error()
	}
	helpers.WriteJSON(w, 200, map[string]any{"ssh_keys": ssh, "become_keys": become, "available": e == nil, "error": message, "platform": "linux", "initial_state": "installed_stopped", "requires_existing_service_account": true})
}
func setupInput(r *http.Request, v any) error {
	raw, e := io.ReadAll(io.LimitReader(r.Body, 8193))
	if e != nil || len(raw) > 8192 || rcd.StrictJSON(raw, v) != nil {
		return rcd.ErrInvalid
	}
	return nil
}
func (c *rcdHostSetupController) validateKeys(project int, req rcd.HostSetupRequest) error {
	key, e := c.store.GetAccessKey(project, req.SSHKeyID)
	if e != nil {
		return e
	}
	if key.Owner != db.AccessKeyShared || (key.Type != db.AccessKeySSH && key.Type != db.AccessKeyLoginPassword) {
		return rcd.ErrInvalid
	}
	if req.BecomeKeyID > 0 {
		key, e = c.store.GetAccessKey(project, req.BecomeKeyID)
		if e != nil {
			return e
		}
		if key.Owner != db.AccessKeyShared || key.Type != db.AccessKeyLoginPassword {
			return rcd.ErrInvalid
		}
	}
	return nil
}
func (c *rcdHostSetupController) create(w http.ResponseWriter, r *http.Request) {
	var req rcd.HostSetupRequest
	if setupInput(r, &req) != nil || req.Validate() != nil {
		setupError(w, rcd.ErrInvalid)
		return
	}
	req.Address = net.ParseIP(req.Address).String()
	project := rcdProject(r)
	old, findErr := c.repo.HostSetupByRequest(r.Context(), project, r.Header.Get("Idempotency-Key"))
	if findErr == nil {
		if old.Request != req || old.ExistingDevice {
			setupError(w, rcd.ErrConflict)
			return
		}
		c.write(w, old, 200)
		return
	}
	if !errors.Is(findErr, rcd.ErrNotFound) {
		setupError(w, findErr)
		return
	}
	if e := c.validateKeys(project, req); e != nil {
		setupError(w, e)
		return
	}
	if _, e := tasks.HostSetupSource(c.store, project); e != nil {
		helpers.WriteJSON(w, 422, map[string]string{"error": e.Error()})
		return
	}
	// Existing registered machines must not be adopted through blank-host
	// first installation. Their current packages/configuration are untouched.
	devices, e := c.repo.Devices(r.Context(), project)
	if e != nil {
		setupError(w, e)
		return
	}
	for _, d := range devices {
		if net.ParseIP(req.Address).Equal(net.ParseIP(d.Address)) {
			helpers.WriteJSON(w, 409, map[string]string{"error": "该地址已登记，请使用现有设备的软件升级，不要重复首装"})
			return
		}
	}
	user := helpers.UserFromContext(r)
	setupProvision.Lock()
	defer setupProvision.Unlock()
	s, created, e := c.repo.CreateHostSetup(r.Context(), project, req, r.Header.Get("Idempotency-Key"), rcdActor(r), user.ID)
	if e != nil {
		setupError(w, e)
		return
	}
	if !created {
		c.write(w, s, 200)
		return
	}
	s, e = tasks.EnsureRCDHostSetupResources(c.store, c.repo, project, s)
	if e != nil {
		s.SetResult("failed", "原生资源登记未完成；未发送 SSH 凭据或安装软件，请管理员核查原记录")
		s, _ = c.repo.UpdateHostSetup(context.Background(), project, s, s.Revision)
		c.write(w, s, 202)
		return
	}
	c.enqueue(w, r, s)
}
func (c *rcdHostSetupController) enqueue(w http.ResponseWriter, r *http.Request, s rcd.HostSetup) {
	secret, _ := json.Marshal(map[string]string{"rcd_host_setup_id": s.ID, "rcd_nonce": s.Nonce})
	user := helpers.UserFromContext(r)
	queue := c.queue
	if queue == nil {
		queue = taskPool(r)
	}
	task, e := queue.AddTask(db.Task{TemplateID: s.TemplateID, Secret: string(secret), Environment: "{}", Message: "RCD 主机接入"}, &user.ID, user.Username, rcdProject(r), false)
	if e != nil {
		current, readErr := c.repo.HostSetup(r.Context(), rcdProject(r), s.ID)
		if readErr == nil {
			s = current
		}
		var admission *tasks.RCDTaskAdmissionError
		notQueued := errors.As(e, &admission) && s.TaskID == 0
		s.QueryNotQueued = s.Phase == "reconcile" && notQueued
		if notQueued && s.Phase != "reconcile" {
			s.SetResult("failed", "未进入原生队列："+e.Error())
		} else {
			s.SetResult("unknown", "入队结果不明确，请查询原任务；不会自动重投")
		}
		s, _ = c.repo.UpdateHostSetup(context.Background(), rcdProject(r), s, s.Revision)
		c.write(w, s, 202)
		return
	}
	s, e = c.repo.HostSetup(r.Context(), rcdProject(r), s.ID)
	if e != nil {
		setupError(w, e)
		return
	}
	if s.TaskID != task.ID {
		s.SetResult("unknown", "任务关联需要补查；不会重新下发")
		s, _ = c.repo.UpdateHostSetup(context.Background(), rcdProject(r), s, s.Revision)
	}
	c.write(w, s, 202)
}
func (c *rcdHostSetupController) write(w http.ResponseWriter, s rcd.HostSetup, status int) {
	w.Header().Set("ETag", s.ETag())
	helpers.WriteJSON(w, status, s)
}
func (c *rcdHostSetupController) reconcile(r *http.Request, s rcd.HostSetup) (rcd.HostSetup, error) {
	var err error
	s, err = tasks.RecoverRCDHostSetupTask(c.store, c.repo, rcdProject(r), s)
	if err != nil {
		return s, err
	}
	if s.TaskID > 0 {
		task, e := c.store.GetTask(rcdProject(r), s.TaskID)
		if e != nil {
			return s, e
		}
		tasks.CollectRCDHostSetup(c.store, task)
		var err error
		s, err = c.repo.HostSetup(r.Context(), rcdProject(r), s.ID)
		if err != nil {
			return s, err
		}
	}
	if (s.State == "ready_for_install" || s.State == "installed_stopped") && !s.ExistingDevice {
		setupProvision.Lock()
		defer setupProvision.Unlock()
		current, e := c.repo.HostSetup(r.Context(), rcdProject(r), s.ID)
		if e != nil {
			return s, e
		}
		s = current
		if s.DeviceID == "" {
			b := rcd.DeviceBinding{Name: s.Request.Name, Address: s.Request.Address, HostAlias: s.HostAlias(), InventoryID: s.InventoryID, TemplateID: s.InspectionTemplateID, ReleaseTemplateID: s.ReleaseTemplateID}
			if e = tasks.ValidateRCDInspectionTemplate(c.store, rcdProject(r), b); e != nil {
				return s, e
			}
			d, e := c.repo.DeviceForTemplate(r.Context(), rcdProject(r), b.TemplateID)
			if errors.Is(e, rcd.ErrNotFound) {
				d, e = c.repo.AddDevice(r.Context(), rcdProject(r), b, s.Actor)
			}
			if e != nil {
				return s, e
			}
			s.DeviceID = d.ID
			s, e = c.repo.UpdateHostSetup(r.Context(), rcdProject(r), s, s.Revision)
			if e != nil {
				return s, e
			}
		}
		if e = c.ensureSchedule(r, s); e != nil {
			return s, e
		}
		d, e := c.repo.Device(r.Context(), rcdProject(r), s.DeviceID)
		if e != nil {
			return s, e
		}
		if d.ControlTemplateID == 0 {
			if e = tasks.ValidateRCDControlTemplate(c.store, rcdProject(r), s.ControlTemplateID, s.InventoryID); e != nil {
				return s, e
			}
			if _, e = c.repo.BindControlTemplate(r.Context(), rcdProject(r), d.ID, d.Revision, s.ControlTemplateID); e != nil {
				return s, e
			}
		} else if d.ControlTemplateID != s.ControlTemplateID {
			return s, rcd.ErrConflict
		}
	}
	return s, nil
}

// requestReconcile submits one explicit READ-ONLY receipt query. It is not an
// installation retry and cannot clear an unknown operation on absent evidence.
func (c *rcdHostSetupController) requestReconcile(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Revision  int  `json:"revision"`
		Confirmed bool `json:"confirmed"`
	}
	if setupInput(r, &input) != nil || !input.Confirmed {
		setupError(w, rcd.ErrInvalid)
		return
	}
	setupProvision.Lock()
	defer setupProvision.Unlock()
	project := rcdProject(r)
	s, err := c.repo.HostSetup(r.Context(), project, mux.Vars(r)["setup_id"])
	if err != nil {
		setupError(w, err)
		return
	}
	if s.Phase == "reconcile" && s.ReconcileRevision == input.Revision && r.Header.Get("If-Match") == `"`+strconv.Itoa(input.Revision)+`"` {
		c.write(w, s, 200)
		return
	}
	if !setupCAS(r, s, input.Revision) || s.State != "unknown" || s.Install == nil || !s.HostKeysConfirmed || !s.Request.CanInstall() {
		setupError(w, rcd.ErrConflict)
		return
	}
	s, err = tasks.RecoverRCDHostSetupTask(c.store, c.repo, project, s)
	if err != nil {
		setupError(w, err)
		return
	}
	provedQueryRejection := s.TaskID == 0 && s.Phase == "reconcile" && s.QueryNotQueued
	if (s.TaskID < 1 && !provedQueryRejection) || s.InstallationTaskID() < 1 {
		helpers.WriteJSON(w, 409, map[string]string{"error": "尚未找到唯一原生安装任务；保留未知操作，不能重新安装或盲目补查"})
		return
	}
	if !provedQueryRejection {
		current, err := c.store.GetTask(project, s.TaskID)
		if err != nil {
			setupError(w, err)
			return
		}
		if !current.Status.IsFinished() || current.TemplateID != s.TemplateID {
			helpers.WriteJSON(w, 409, map[string]string{"error": "原任务尚未结束或身份无法确认；先查询原任务，不并发补查"})
			return
		}
	}
	original, err := c.store.GetTask(project, s.InstallationTaskID())
	if err != nil {
		setupError(w, err)
		return
	}
	if original.TemplateID != s.TemplateID || !original.Status.IsFinished() {
		setupError(w, rcd.ErrConflict)
		return
	}
	s.OriginalInstallTaskID = s.InstallationTaskID()
	s.ReconcileRevision = input.Revision
	s.StartPhase("reconcile", "reconcile_queued")
	user := helpers.UserFromContext(r)
	s.UserID = user.ID
	s.Actor = user.Username
	s, err = c.repo.UpdateHostSetup(r.Context(), project, s, s.Revision)
	if err != nil {
		setupError(w, err)
		return
	}
	c.enqueue(w, r, s)
}
func (c *rcdHostSetupController) get(w http.ResponseWriter, r *http.Request) {
	s, e := c.repo.HostSetup(r.Context(), rcdProject(r), mux.Vars(r)["setup_id"])
	if e == nil {
		s, e = c.reconcile(r, s)
	}
	if e != nil {
		setupError(w, e)
		return
	}
	c.write(w, s, 200)
}
func (c *rcdHostSetupController) list(w http.ResponseWriter, r *http.Request) {
	page, size, valid := operationPagination(r)
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	state := r.URL.Query().Get("state")
	if !valid || len(q) > 128 || !validHostSetupState(state) {
		setupError(w, rcd.ErrInvalid)
		return
	}
	items, e := c.repo.HostSetups(r.Context(), rcdProject(r))
	if e != nil {
		setupError(w, e)
		return
	}
	selected := []rcd.HostSetup{}
	for _, item := range items {
		component := ""
		if item.Install != nil {
			component = item.Install.Component
		}
		text := strings.ToLower(strings.Join([]string{item.Request.Name, item.Request.Address, item.ID, item.Actor, component}, " "))
		if (state == "" || item.State == state) && (q == "" || strings.Contains(text, q)) {
			selected = append(selected, item)
		}
	}
	// Existing maintenance callers intentionally read every active record. Only
	// an explicitly paginated request may truncate this backwards-compatible API.
	if r.URL.Query().Has("page") || r.URL.Query().Has("page_size") {
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
		return
	}
	helpers.WriteJSON(w, 200, map[string]any{"items": selected, "total": len(selected)})
}

func validHostSetupState(state string) bool {
	switch state {
	case "", "keyscan_queued", "awaiting_host_key", "ssh_probe_queued", "ready_for_install", "installation_queued", "reconcile_queued", "running", "installed_stopped", "failed", "host_key_conflict", "cancelled", "unknown":
		return true
	}
	return false
}
func setupCAS(r *http.Request, s rcd.HostSetup, revision int) bool {
	return r.Header.Get("If-Match") == s.ETag() && revision == s.Revision
}
func (c *rcdHostSetupController) confirmKeys(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Revision     int      `json:"revision"`
		Fingerprints []string `json:"fingerprints"`
		Confirmed    bool     `json:"confirmed"`
	}
	if setupInput(r, &req) != nil || !req.Confirmed {
		setupError(w, rcd.ErrInvalid)
		return
	}
	setupProvision.Lock()
	defer setupProvision.Unlock()
	s, e := c.repo.HostSetup(r.Context(), rcdProject(r), mux.Vars(r)["setup_id"])
	if e != nil {
		setupError(w, e)
		return
	}
	if !setupCAS(r, s, req.Revision) || s.State != "awaiting_host_key" {
		setupError(w, rcd.ErrConflict)
		return
	}
	// Verify the exact fingerprint set before writing native trust records.
	candidate, e := c.repo.ConfirmHostSetupKeys(r.Context(), rcdProject(r), s, req.Fingerprints)
	if e != nil {
		setupError(w, e)
		return
	}
	s = candidate
	if e = rcd.ConfirmHostKeys(s); e != nil {
		s.SetResult("host_key_conflict", "SSH 主机指纹信任未通过；原信任不覆盖，请核查目标和已有 known_hosts")
		s, _ = c.repo.UpdateHostSetup(r.Context(), rcdProject(r), s, s.Revision)
		c.write(w, s, 409)
		return
	}
	user := helpers.UserFromContext(r)
	s.UserID = user.ID
	s.Actor = rcdActor(r)
	s, e = c.repo.UpdateHostSetup(r.Context(), rcdProject(r), s, s.Revision)
	if e != nil {
		setupError(w, e)
		return
	}
	c.enqueue(w, r, s)
}
func (c *rcdHostSetupController) install(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Revision        int    `json:"revision"`
		PackageID       string `json:"package_id"`
		PackageRevision int    `json:"package_revision"`
		Confirmed       bool   `json:"confirmed"`
	}
	if setupInput(r, &req) != nil || !req.Confirmed {
		setupError(w, rcd.ErrInvalid)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 || strings.ContainsAny(key, "\r\n\x00") {
		setupError(w, rcd.ErrInvalid)
		return
	}
	setupProvision.Lock()
	defer setupProvision.Unlock()
	project := rcdProject(r)
	s, e := c.repo.HostSetup(r.Context(), project, mux.Vars(r)["setup_id"])
	if e != nil {
		setupError(w, e)
		return
	}
	if s.Install != nil && s.Install.RequestKey == key {
		if s.Install.PackageID != req.PackageID || s.Install.PackageRevision != req.PackageRevision {
			setupError(w, rcd.ErrConflict)
			return
		}
		c.write(w, s, 200)
		return
	}
	if !setupCAS(r, s, req.Revision) || s.State != "ready_for_install" || s.Install != nil || !s.HostKeysConfirmed {
		setupError(w, rcd.ErrConflict)
		return
	}
	if !s.Request.CanInstall() {
		helpers.WriteJSON(w, 422, map[string]string{"error": "首装固定系统目录需要 root SSH 或已选择的加密 sudo 凭据；当前绑定仅可登记/检查，不会启动安装。请先核实并补齐凭据"})
		return
	}
	p, e := c.repo.Get(r.Context(), project, req.PackageID)
	if e != nil {
		setupError(w, e)
		return
	}
	target := p.Manifest.Target
	if target == "linux/x86_64" {
		target = "linux/amd64"
	}
	if target == "linux/aarch64" {
		target = "linux/arm64"
	}
	if !p.Enabled || p.Revision != req.PackageRevision || target != s.Platform || rcd.ValidateFirstInstallPackage(p.Manifest) != nil {
		helpers.WriteJSON(w, 422, map[string]string{"error": "软件包已停用、版本变化、架构不兼容或首装适配器版本/组件依赖尚未满足"})
		return
	}
	install := &rcd.HostSetupInstall{PackageID: p.ID, PackageRevision: p.Revision, RequestKey: key, Component: p.Manifest.Component, ArchiveSHA256: p.ArchiveSHA256, BinarySHA256: p.Manifest.BinarySHA256}
	if !rcd.IsBuiltinComponent(p.Manifest.Component) {
		app, e := c.repo.GetApplication(r.Context(), project, p.Manifest.Component)
		if e != nil {
			setupError(w, e)
			return
		}
		install.Application = &app
	}
	s.Install = install
	s.StartPhase("install", "installation_queued")
	user := helpers.UserFromContext(r)
	s.UserID = user.ID
	s.Actor = user.Username
	s, e = c.repo.UpdateHostSetup(r.Context(), project, s, s.Revision)
	if e != nil {
		setupError(w, e)
		return
	}
	c.enqueue(w, r, s)
}

func (c *rcdHostSetupController) cancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Revision int `json:"revision"`
	}
	if setupInput(r, &req) != nil {
		setupError(w, rcd.ErrInvalid)
		return
	}
	setupProvision.Lock()
	defer setupProvision.Unlock()
	s, e := c.repo.HostSetup(r.Context(), rcdProject(r), mux.Vars(r)["setup_id"])
	if e != nil {
		setupError(w, e)
		return
	}
	if !setupCAS(r, s, req.Revision) || s.Install != nil || (s.State != "awaiting_host_key" && s.State != "ready_for_install") {
		helpers.WriteJSON(w, 409, map[string]string{"error": "仅可取消等待指纹或尚未选择安装包的接入；执行中或结果未知必须核查原任务"})
		return
	}
	s.SetResult("cancelled", "")
	s, e = c.repo.UpdateHostSetup(r.Context(), rcdProject(r), s, s.Revision)
	if e != nil {
		setupError(w, e)
		return
	}
	c.write(w, s, 200)
}

func (c *rcdHostSetupController) ensureSchedule(r *http.Request, s rcd.HostSetup) error {
	existing, e := c.store.GetTemplateSchedules(rcdProject(r), s.InspectionTemplateID, false)
	if e != nil {
		return e
	}
	if len(existing) > 0 {
		for _, schedule := range existing {
			if schedule.Active && schedule.CronFormat == "* * * * *" {
				return nil
			}
		}
		return rcd.ErrConflict
	}
	_, e = c.store.CreateSchedule(db.Schedule{ProjectID: rcdProject(r), TemplateID: s.InspectionTemplateID, Name: "RCD 自动状态检查 · " + s.Request.Name, CronFormat: "* * * * *", Active: true, TaskParams: &db.TaskParams{Environment: "{}", Message: "RCD 自动只读检查"}})
	if e == nil {
		refreshSchedulePool(r)
	}
	return e
}

func (c *rcdHostSetupController) firstInstall(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Revision        int    `json:"revision"`
		PackageID       string `json:"package_id"`
		PackageRevision int    `json:"package_revision"`
		Confirmed       bool   `json:"confirmed"`
	}
	if setupInput(r, &input) != nil || !input.Confirmed {
		setupError(w, rcd.ErrInvalid)
		return
	}
	project := rcdProject(r)
	key := r.Header.Get("Idempotency-Key")
	old, e := c.repo.HostSetupByRequest(r.Context(), project, key)
	if e == nil {
		if !old.ExistingDevice || old.DeviceID != mux.Vars(r)["device_id"] || old.Install == nil || old.Install.PackageID != input.PackageID || old.Install.PackageRevision != input.PackageRevision {
			setupError(w, rcd.ErrConflict)
			return
		}
		c.write(w, old, 200)
		return
	}
	if !errors.Is(e, rcd.ErrNotFound) {
		setupError(w, e)
		return
	}
	d, e := c.repo.Device(r.Context(), project, mux.Vars(r)["device_id"])
	if e != nil {
		setupError(w, e)
		return
	}
	if r.Header.Get("If-Match") != d.ETag() || input.Revision != d.Revision || d.ActiveOperationID != "" || d.State != "checked" {
		helpers.WriteJSON(w, 409, map[string]string{"error": "设备修订已变化、状态检查过期或有未完成发布；请先检查状态并查询原操作"})
		return
	}
	p, e := c.repo.Get(r.Context(), project, input.PackageID)
	if e != nil {
		setupError(w, e)
		return
	}
	if !p.Enabled || p.Revision != input.PackageRevision || rcd.ValidateFirstInstallPackage(p.Manifest) != nil {
		setupError(w, rcd.ErrInvalid)
		return
	}
	missing := false
	architecture := ""
	for _, component := range d.Components {
		if component.Component == p.Manifest.Component && component.State == "not_installed" {
			missing = true
		}
		for _, build := range []*rcd.BuildObservation{component.RunningBuild, component.DiskBuild} {
			if build != nil {
				candidate := rcd.Architecture(build.Target)
				if architecture != "" && architecture != candidate {
					setupError(w, rcd.ErrInvalid)
					return
				}
				architecture = candidate
			}
		}
	}
	if !missing || architecture == "" || architecture != rcd.Architecture(p.Manifest.Target) {
		helpers.WriteJSON(w, 422, map[string]string{"error": "仅允许对新鲜检查明确未安装的组件执行首装，且目标架构必须有可信证据；已有程序应使用升级"})
		return
	}
	req, e := tasks.RCDExistingHostSetupRequest(c.store, project, d)
	if e != nil {
		helpers.WriteJSON(w, 422, map[string]string{"error": e.Error()})
		return
	}
	if !req.CanInstall() {
		helpers.WriteJSON(w, 422, map[string]string{"error": "首装需要 root SSH 或库存已绑定的加密 sudo 凭据；当前设备仍可只读检查，请先由管理员补齐凭据"})
		return
	}
	install := rcd.HostSetupInstall{PackageID: p.ID, PackageRevision: p.Revision, RequestKey: key, Component: p.Manifest.Component, ArchiveSHA256: p.ArchiveSHA256, BinarySHA256: p.Manifest.BinarySHA256}
	if !rcd.IsBuiltinComponent(p.Manifest.Component) {
		app, e := c.repo.GetApplication(r.Context(), project, p.Manifest.Component)
		if e != nil {
			setupError(w, e)
			return
		}
		install.Application = &app
	}
	setupProvision.Lock()
	defer setupProvision.Unlock()
	user := helpers.UserFromContext(r)
	s, created, e := c.repo.CreateExistingFirstInstall(r.Context(), project, d, req, install, key, user.Username, user.ID, "linux/"+architecture)
	if e != nil {
		setupError(w, e)
		return
	}
	if !created {
		c.write(w, s, 200)
		return
	}
	s, e = tasks.EnsureRCDHostSetupResources(c.store, c.repo, project, s)
	if e != nil {
		s.SetResult("failed", "首装原生资源登记失败；未执行安装，请核查原操作")
		s, _ = c.repo.UpdateHostSetup(context.Background(), project, s, s.Revision)
		c.write(w, s, 202)
		return
	}
	c.enqueue(w, r, s)
}

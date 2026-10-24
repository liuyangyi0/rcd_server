// SPDX-License-Identifier: MIT
package projects

import (
	"errors"
	"io"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/services/rcd"
)

func registerRCDApplications(router *mux.Router, repo *rcd.Repository) {
	c := &rcdApplicationsController{repo: repo}
	router.HandleFunc("/applications", c.list).Methods("GET")
	router.Handle("/applications", GetMustCanMiddleware(db.CanManageProjectResources)(http.HandlerFunc(c.add))).Methods("POST")
}

type rcdApplicationsController struct{ repo *rcd.Repository }

func applicationError(w http.ResponseWriter, err error) {
	status, message := 500, "软件种类保存失败，请检查平台日志；未安装或发布任何程序"
	switch {
	case errors.Is(err, rcd.ErrInvalid):
		status, message = 422, err.Error()
	case errors.Is(err, rcd.ErrConflict):
		status, message = 409, "该软件标识已注册为不同定义；为避免覆盖安装目标，请使用新标识"
	case errors.Is(err, rcd.ErrNotFound):
		status, message = 404, "软件种类不存在"
	}
	helpers.WriteJSON(w, status, map[string]string{"error": message})
}

func (c *rcdApplicationsController) list(w http.ResponseWriter, r *http.Request) {
	items, err := c.repo.ListApplications(r.Context(), rcdProject(r))
	if err != nil {
		applicationError(w, err)
		return
	}
	helpers.WriteJSON(w, 200, map[string]any{"items": items})
}

func (c *rcdApplicationsController) add(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8193))
	var req rcd.ApplicationRequest
	if err != nil || len(raw) > 8192 {
		applicationError(w, rcd.ErrInvalid)
		return
	}
	if err = rcd.StrictJSON(raw, &req); err != nil {
		applicationError(w, err)
		return
	}
	a, created, err := c.repo.RegisterApplication(r.Context(), rcdProject(r), req, rcdActor(r))
	if err != nil {
		applicationError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	w.Header().Set("ETag", `"1"`)
	helpers.WriteJSON(w, status, a)
}

// SPDX-License-Identifier: MIT
package projects

import (
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/services/rcd"
)

// RegisterRCDRoutes is inside the existing authenticated API router. Project
// membership, CSRF and native permissions are applied, including downloads.
func RegisterRCDRoutes(parent *mux.Router, store db.Store) {
	root := os.Getenv("SEMAPHORE_RCD_PACKAGES_PATH")
	if root == "" {
		return
	} // Opt-in until the complete daily workflow is accepted.
	provider, ok := store.(interface{ RCDRepository() *rcd.Repository })
	if !ok {
		panic("RCD packages require the native SQL store")
	}
	packages, err := rcd.NewPackages(provider.RCDRepository(), root)
	if err != nil {
		panic(err)
	}
	c := &rcdPackagesController{packages: packages}
	r := parent.PathPrefix("/project/{project_id}/rcd").Subrouter()
	r.Use(ProjectMiddleware)
	r.HandleFunc("/packages", c.list).Methods("GET")
	r.Handle("/packages", GetMustCanMiddleware(db.CanManageProjectResources)(http.HandlerFunc(c.upload))).Methods("POST")
	r.HandleFunc("/packages/{package_id}", c.get).Methods("GET")
	r.HandleFunc("/packages/{package_id}/download", c.download).Methods("GET", "HEAD")
	r.Handle("/packages/{package_id}/enabled", GetMustCanMiddleware(db.CanManageProjectResources)(http.HandlerFunc(c.setEnabled))).Methods("PUT")

	registerRCDDevices(r, store, provider.RCDRepository())
	registerRCDOperations(r, store, provider.RCDRepository())
	registerRCDApplications(r, provider.RCDRepository())
	registerRCDHostSetups(r, store, provider.RCDRepository())
}

type rcdPackagesController struct{ packages *rcd.Packages }

func rcdProject(r *http.Request) int  { return helpers.GetFromContext(r, "project").(db.Project).ID }
func rcdActor(r *http.Request) string { return helpers.GetFromContext(r, "user").(*db.User).Username }
func rcdError(w http.ResponseWriter, err error) {
	status, message := http.StatusInternalServerError, "软件包存储失败，请检查平台日志；未触发发布"
	var tooLarge *http.MaxBytesError
	switch {
	case errors.Is(err, rcd.ErrNotFound):
		status, message = 404, "软件包不存在"
	case errors.Is(err, rcd.ErrBinaryIdentityConflict):
		status, message = 409, "同一程序文件已登记为另一构建身份，不能只改版本或发布编号重打包；请复用已有软件包，或重新构建后上传"
	case errors.Is(err, rcd.ErrConflict):
		status, message = 409, "发布编号内容冲突或记录已被修改，请刷新后重试"
	case errors.Is(err, rcd.ErrTooLarge), errors.As(err, &tooLarge):
		status, message = 413, "软件包超过允许大小"
	case errors.Is(err, rcd.ErrInvalid):
		status, message = 422, err.Error()
	default:
		log.Printf("RCD packages: %v", err)
	}
	helpers.WriteJSON(w, status, map[string]string{"error": message})
}

func (c *rcdPackagesController) list(w http.ResponseWriter, r *http.Request) {
	filter := rcd.Filter{Page: 1, PageSize: 20, Component: r.URL.Query().Get("component"), Search: r.URL.Query().Get("q")}
	for key, ptr := range map[string]*int{"page": &filter.Page, "page_size": &filter.PageSize} {
		if value := r.URL.Query().Get(key); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil {
				rcdError(w, rcd.ErrInvalid)
				return
			}
			*ptr = n
		}
	}
	if value := r.URL.Query().Get("enabled"); value != "" {
		if value != "true" && value != "false" {
			rcdError(w, rcd.ErrInvalid)
			return
		}
		enabled := value == "true"
		filter.Enabled = &enabled
	}
	page, err := c.packages.Repository.List(r.Context(), rcdProject(r), filter)
	if err != nil {
		rcdError(w, err)
		return
	}
	helpers.WriteJSON(w, 200, page)
}

func (c *rcdPackagesController) upload(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/gzip" && mediaType != "application/octet-stream") {
		helpers.WriteJSON(w, 415, map[string]string{"error": "请直接上传 .rcd.tar.gz 文件"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, rcd.MaxArchiveBytes)
	p, created, err := c.packages.Upload(r.Context(), rcdProject(r), rcdActor(r), r.Body)
	if err != nil {
		rcdError(w, err)
		return
	}
	w.Header().Set("ETag", p.ETag())
	status := 200
	if created {
		status = 201
	}
	helpers.WriteJSON(w, status, p)
}

func (c *rcdPackagesController) getPackage(r *http.Request) (rcd.Package, error) {
	return c.packages.Repository.Get(r.Context(), rcdProject(r), mux.Vars(r)["package_id"])
}
func (c *rcdPackagesController) get(w http.ResponseWriter, r *http.Request) {
	p, err := c.getPackage(r)
	if err != nil {
		rcdError(w, err)
		return
	}
	w.Header().Set("ETag", p.ETag())
	helpers.WriteJSON(w, 200, p)
}
func (c *rcdPackagesController) download(w http.ResponseWriter, r *http.Request) {
	p, err := c.getPackage(r)
	if err != nil {
		rcdError(w, err)
		return
	}
	f, err := c.packages.OpenArchive(rcdProject(r), p)
	if err != nil {
		rcdError(w, err)
		return
	}
	defer f.Close()
	name := p.Manifest.Component + "-" + p.Manifest.ReleaseID + ".rcd.tar.gz"
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("ETag", `"`+p.ArchiveSHA256+`"`)
	http.ServeContent(w, r, name, time.Time{}, f)
}
func (c *rcdPackagesController) setEnabled(w http.ResponseWriter, r *http.Request) {
	p, err := c.getPackage(r)
	if err != nil {
		rcdError(w, err)
		return
	}
	if r.Header.Get("If-Match") == "" {
		helpers.WriteJSON(w, 428, map[string]string{"error": "缺少 If-Match 版本条件"})
		return
	}
	if r.Header.Get("If-Match") != p.ETag() {
		rcdError(w, rcd.ErrConflict)
		return
	}
	var input struct {
		Enabled  *bool `json:"enabled"`
		Revision int   `json:"revision"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1025))
	if err != nil || len(raw) > 1024 {
		rcdError(w, rcd.ErrInvalid)
		return
	}
	if err = rcd.StrictJSON(raw, &input); err != nil || input.Enabled == nil {
		rcdError(w, rcd.ErrInvalid)
		return
	}
	if input.Revision != p.Revision {
		rcdError(w, rcd.ErrConflict)
		return
	}
	// Body identity is not accepted: actor is always the authenticated user.
	updated, err := c.packages.Repository.SetEnabled(r.Context(), rcdProject(r), p.ID, p.Revision, *input.Enabled, strings.TrimSpace(rcdActor(r)))
	if err != nil {
		rcdError(w, err)
		return
	}
	w.Header().Set("ETag", updated.ETag())
	helpers.WriteJSON(w, 200, updated)
}

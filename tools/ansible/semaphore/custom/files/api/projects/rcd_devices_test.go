// SPDX-License-Identifier: MIT
package projects

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/semaphoreui/semaphore/db"
	dbsql "github.com/semaphoreui/semaphore/db/sql"
	"github.com/semaphoreui/semaphore/services/rcd"
)

func TestRCDDevicesAPIIsolationAndRevision(t *testing.T) {
	store := dbsql.InitConfigCreateTestStore()
	defer store.Close()
	t.Setenv("SEMAPHORE_RCD_PACKAGES_PATH", t.TempDir())
	for _, query := range []string{
		"insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'one'),(2,CURRENT_TIMESTAMP,'two')",
		"insert into user(id,created,username,name,email,password,admin) values(1,CURRENT_TIMESTAMP,'operator','operator','test@invalid','unused',0)",
		"insert into project__user(project_id,user_id,role) values(1,1,'manager'),(2,1,'guest')",
		"insert into user(id,created,username,name,email,password,admin) values(2,CURRENT_TIMESTAMP,'runner','runner','runner@invalid','unused',0)",
		"insert into project__user(project_id,user_id,role) values(1,2,'task_runner')",
	} {
		if _, err := store.Sql().Db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	d, err := store.RCDRepository().AddDevice(context.Background(), 1, rcd.DeviceBinding{Name: "46", Address: "192.168.10.46", HostAlias: "rcd46", InventoryID: 1, TemplateID: 2}, "operator")
	if err != nil {
		t.Fatal(err)
	}
	router := mux.NewRouter()
	RegisterRCDRoutes(router, store)
	actorID := 1
	request := func(method, path, body, etag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("If-Match", etag)
		req = helpers.SetContextValue(req, "store", store)
		req = helpers.SetContextValue(req, "user", &db.User{ID: actorID, Username: "operator"})
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	path := "/project/1/rcd/devices/" + d.ID
	if w := request("GET", path, "", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("GET", "/project/2/rcd/devices/"+d.ID, "", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	for _, item := range []struct {
		path, body, etag string
		status           int
	}{
		{path + "/inspections", `{"revision":1}`, "", 428},
		{path + "/inspections", `{"revision":1}`, `"2"`, 409},
		{path + "/inspections", `{"revision":2}`, `"1"`, 409},
		{path + "/inspections", `{"revision":1,"actor":"spoofed"}`, `"1"`, 422},
		{"/project/2/rcd/devices", `{}`, "", 403},
	} {
		w := request("POST", item.path, item.body, item.etag)
		if w.Code != item.status {
			t.Fatal(item, w.Code, w.Body.String())
		}
	}
	w := request("GET", "/project/1/rcd/devices?page_size=20&q=46", "", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var page struct {
		Total int          `json:"total"`
		Items []rcd.Device `json:"items"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &page); err != nil || page.Total != 1 || page.Items[0].State != "never_checked" {
		t.Fatal(page, err)
	}
	if w = request("GET", "/project/1/rcd/devices?page_size=999", "", ""); w.Code != 422 {
		t.Fatal(w.Code)
	}
	actorID = 2
	if w = request("GET", "/project/1/rcd/fleet", "", ""); w.Code != 200 {
		t.Fatal("native read permission cannot query fleet", w.Code, w.Body.String())
	}
	if w = request("PUT", path+"/machine-binding", `{}`, `"1"`); w.Code != 403 {
		t.Fatal("task runner must not change machine identities", w.Code)
	}
	if w = request("PUT", path+"/control-binding", `{}`, `"1"`); w.Code != 403 {
		t.Fatal("task runner must not change service templates", w.Code)
	}
	if w = request("POST", path+"/inspections", `{"revision":0}`, `"0"`); w.Code != 409 {
		t.Fatal("native task runner should pass the permission gate", w.Code)
	}
	if w = request("POST", "/project/1/rcd/devices", `{}`, ""); w.Code != 403 {
		t.Fatal("task runner must not manage bindings", w.Code)
	}
}

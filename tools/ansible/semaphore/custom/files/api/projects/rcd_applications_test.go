// SPDX-License-Identifier: MIT
package projects

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/semaphoreui/semaphore/db"
	dbsql "github.com/semaphoreui/semaphore/db/sql"
)

func TestRCDApplicationsAPIUsesNativeProjectPermission(t *testing.T) {
	store := dbsql.InitConfigCreateTestStore()
	defer store.Close()
	t.Setenv("SEMAPHORE_RCD_PACKAGES_PATH", t.TempDir())
	for _, query := range []string{
		"insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'one'),(2,CURRENT_TIMESTAMP,'two')",
		"insert into user(id,created,username,name,email,password,admin) values(1,CURRENT_TIMESTAMP,'operator','operator','test@invalid','unused',0)",
		"insert into project__user(project_id,user_id,role) values(1,1,'manager'),(2,1,'guest')",
	} {
		if _, err := store.Sql().Db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	router := mux.NewRouter()
	RegisterRCDRoutes(router, store)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req = helpers.SetContextValue(req, "store", store)
		req = helpers.SetContextValue(req, "user", &db.User{ID: 1, Username: "operator"})
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	valid := `{"component":"my-app","name":"我的程序","arguments":[],"graceful_shutdown_confirmed":true}`
	for _, c := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/project/1/rcd/applications", "", 200},
		{"POST", "/project/1/rcd/applications", valid, 201},
		{"POST", "/project/1/rcd/applications", valid, 200},
		{"POST", "/project/2/rcd/applications", valid, 403},
		{"POST", "/project/1/rcd/applications", `{"component":"app","name":"App","arguments":[],"graceful_shutdown_confirmed":true,"install_path":"/bin/sh"}`, 422},
	} {
		if w := request(c.method, c.path, c.body); w.Code != c.status {
			t.Fatalf("%s: want %d got %d %s", c.path, c.status, w.Code, w.Body.String())
		}
	}
}

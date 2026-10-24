// SPDX-License-Identifier: MIT
package projects

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gorilla/mux"
	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/semaphoreui/semaphore/db"
	dbsql "github.com/semaphoreui/semaphore/db/sql"
	"github.com/semaphoreui/semaphore/services/rcd"
	"github.com/semaphoreui/semaphore/services/rcd/testutil"
)

func TestRCDPackagesAPI(t *testing.T) {
	store := dbsql.InitConfigCreateTestStore()
	defer store.Close()
	root := t.TempDir()
	t.Setenv("SEMAPHORE_RCD_PACKAGES_PATH", root)
	for _, query := range []string{
		"insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'one'),(2,CURRENT_TIMESTAMP,'two'),(3,CURRENT_TIMESTAMP,'not-a-member')",
		"insert into user(id,created,username,name,email,password,admin) values(1,CURRENT_TIMESTAMP,'operator','operator','test@invalid','unused',0)",
		"insert into project__user(project_id,user_id,role) values(1,1,'manager'),(2,1,'guest')",
	} {
		if _, err := store.Sql().Db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	router := mux.NewRouter()
	RegisterRCDRoutes(router, store)
	request := func(method, path string, body []byte, etag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/gzip")
		if etag != "" {
			req.Header.Set("If-Match", etag)
		}
		req = helpers.SetContextValue(req, "store", store)
		req = helpers.SetContextValue(req, "user", &db.User{ID: 1, Username: "operator"})
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	assert := func(w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("want %d got %d: %s", status, w.Code, w.Body.String())
		}
	}
	path := "/project/1/rcd/packages"
	blob := testutil.Package(t, "manager", "r1", "test")
	w := request("POST", path, blob, "")
	assert(w, 201)
	var p rcd.Package
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.UploadedBy != "operator" {
		t.Fatal("actor must come from authenticated context")
	}
	assert(request("POST", path, blob, ""), 200)
	assert(request("POST", path, testutil.Package(t, "manager", "r1", "different content"), ""), 409)
	assert(request("POST", path, []byte("invalid"), ""), 422)
	assert(request("POST", "/project/2/rcd/packages", blob, ""), 403)
	if w = request("GET", "/project/3/rcd/packages", nil, ""); w.Code == 200 {
		t.Fatal("nonmember admitted")
	}
	assert(request("GET", "/project/2/rcd/packages/"+p.ID, nil, ""), 404)
	assert(request("GET", path+"?page_size=17", nil, ""), 422)
	w = request("GET", path+"?page_size=20&component=manager", nil, "")
	assert(w, 200)
	var page rcd.Page
	json.Unmarshal(w.Body.Bytes(), &page)
	if page.Total != 1 || page.TotalPages != 1 {
		t.Fatal(page)
	}
	w = request("GET", path+"/"+p.ID+"/download", nil, "")
	assert(w, 200)
	if !bytes.Equal(w.Body.Bytes(), blob) {
		t.Fatal("download mismatch")
	}
	change := []byte(`{"enabled":false,"revision":1}`)
	assert(request("PUT", path+"/"+p.ID+"/enabled", change, ""), 428)
	assert(request("PUT", path+"/"+p.ID+"/enabled", change, `"9"`), 409)
	assert(request("PUT", path+"/"+p.ID+"/enabled", []byte(`{"enabled":false,"revision":1,"actor":"admin"}`), `"1"`), 422)
	assert(request("PUT", path+"/"+p.ID+"/enabled", change, `"1"`), 200)
	assert(request("PUT", path+"/"+p.ID+"/enabled", change, `"1"`), 409)
	// Restart the controller against the same database and files: no lost package.
	router = mux.NewRouter()
	RegisterRCDRoutes(router, store)
	assert(request("GET", path+"/"+p.ID, nil, ""), 200)
	// A corrupted disk archive must never be returned as a verified download.
	if err := os.WriteFile(filepath.Join(root, "1", p.ID, "package.rcd.tar.gz"), []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	assert(request("GET", path+"/"+p.ID+"/download", nil, ""), http.StatusInternalServerError)
	var tasks int
	if err := store.Sql().Db.QueryRow("select count(*) from task").Scan(&tasks); err != nil || tasks != 0 {
		t.Fatal("upload created a native task", tasks, err)
	}
}

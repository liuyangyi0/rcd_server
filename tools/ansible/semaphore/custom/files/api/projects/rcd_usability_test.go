// SPDX-License-Identifier: MIT
package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/semaphoreui/semaphore/db"
	dbsql "github.com/semaphoreui/semaphore/db/sql"
	"github.com/semaphoreui/semaphore/services/rcd"
)

func TestRCDOperationErrorsNeverInventUnsubmittedResult(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		err    error
		status int
		want   string
	}{
		{"ambiguous_storage", errors.New("private storage diagnostic"), 500, "不要重复提交"},
		{"changed_state", rcd.ErrConflict, 409, "不要更换编号"},
		{"missing_resource", rcd.ErrNotFound, 404, "记录不存在"},
		{"invalid_input", rcd.ErrInvalid, 422, "操作参数不合法"},
		{"validation_detail", fmt.Errorf("%w: 请先检查目标机器状态", rcd.ErrInvalid), 422, "请先检查目标机器状态"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			operationErrorWithID(response, scenario.err, "original-public-operation-id")
			var body map[string]string
			if response.Code != scenario.status || json.Unmarshal(response.Body.Bytes(), &body) != nil {
				t.Fatal(response.Code, response.Body.String())
			}
			if !strings.Contains(body["error"], scenario.want) || body["operation_id"] != "original-public-operation-id" {
				t.Fatal(body)
			}
			for _, forbidden := range []string{"未触发发布", "private storage diagnostic", "nonce", "invalid RCD package"} {
				if strings.Contains(response.Body.String(), forbidden) {
					t.Fatal("misleading or private error response", body)
				}
			}
		})
	}
	response := httptest.NewRecorder()
	operationError(response, rcd.ErrInvalid)
	if strings.Contains(response.Body.String(), "operation_id") {
		t.Fatal("validation without an operation must not invent one")
	}
}

func TestRCDOperationSearchUsesPublicTargetAndReleaseFields(t *testing.T) {
	store := dbsql.InitConfigCreateTestStore()
	defer store.Close()
	if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'one'),(2,CURRENT_TIMESTAMP,'two')"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 26; i++ {
		project := 1
		if i == 26 {
			project = 2
		}
		o := rcd.Operation{ID: fmt.Sprintf("%064x", i), Actor: "operator", Package: rcd.Manifest{Version: "1.2.3", ReleaseID: "release-October", Component: "manager"}, Request: rcd.OperationRequest{Action: "upgrade"}, Targets: []rcd.ReleaseTarget{{Name: "Testing Host", Address: "192.0.2.46", HostAlias: "rcd46", DeviceID: "public-device"}}, State: "succeeded", Revision: 1}
		raw, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = store.Sql().Db.Exec("insert into rcd_operation(project_id,id,request_key,request_hash,nonce,body,state,revision,task_id,created_at) values(?,?,?,?,?,?,?,1,0,?)", project, o.ID, fmt.Sprintf("search-%d", i), "unused", "private", string(raw), o.State, "2026-10-22T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	c := rcdOperationsController{store: store, repo: store.RCDRepository()}
	request := func(query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/?"+query, nil)
		r = helpers.SetContextValue(r, "project", db.Project{ID: 1})
		w := httptest.NewRecorder()
		c.list(w, r)
		return w
	}
	for _, search := range []string{"  TESTING HOST  ", "192.0.2.46", "rcd46", "release-OCTOBER", "public-device", "operator"} {
		w := request("q=" + url.QueryEscape(search) + "&page=2&page_size=20")
		var page struct {
			Items      []rcd.Operation `json:"items"`
			Total      int             `json:"total"`
			TotalPages int             `json:"total_pages"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Total != 25 || page.TotalPages != 2 || len(page.Items) != 5 {
			t.Fatal(search, w.Code, w.Body.String())
		}
		if page.Items[0].ID != fmt.Sprintf("%064x", 5) || page.Items[4].ID != fmt.Sprintf("%064x", 1) {
			t.Fatal("pagination/order/project isolation changed", page.Items)
		}
	}
	for _, query := range []string{"state=not-a-state", "kind=install", "q=" + strings.Repeat("x", 129), "page_size=17"} {
		if w := request(query); w.Code != 422 {
			t.Fatal(query, w.Code, w.Body.String())
		}
	}
	if w := request("q=does-not-exist"); !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestRCDHostSetupOptionalPaginationPreservesMaintenanceAllRecords(t *testing.T) {
	store := dbsql.InitConfigCreateTestStore()
	defer store.Close()
	if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'one'),(2,CURRENT_TIMESTAMP,'two')"); err != nil {
		t.Fatal(err)
	}
	repo, ctx := store.RCDRepository(), context.Background()
	ids := []string{}
	for i := 1; i <= 26; i++ {
		project := 1
		if i == 26 {
			project = 2
		}
		s, _, err := repo.CreateHostSetup(ctx, project, rcd.HostSetupRequest{Name: fmt.Sprintf("Fixture-%02d", i), Address: fmt.Sprintf("192.0.2.%d", i), SSHPort: 22, SSHUser: "operator", ServiceAccount: "operator", SSHKeyID: 1}, fmt.Sprintf("fixture-list-%d", i), "operator", 1)
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			s.SetResult("failed", "fixture was never executed")
			if _, err = repo.UpdateHostSetup(ctx, project, s, s.Revision); err != nil {
				t.Fatal(err)
			}
		}
		if project == 1 {
			ids = append(ids, s.ID)
		}
	}
	// Force equal timestamps to exercise the stable ID tiebreaker.
	if _, err := store.Sql().Db.Exec("update rcd_host_setup set created_at='2026-10-22T00:00:00Z'"); err != nil {
		t.Fatal(err)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	c := rcdHostSetupController{store: store, repo: repo}
	request := func(query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/?"+query, nil)
		r = helpers.SetContextValue(r, "project", db.Project{ID: 1})
		w := httptest.NewRecorder()
		c.list(w, r)
		return w
	}
	var full struct {
		Items []rcd.HostSetup `json:"items"`
		Total int             `json:"total"`
	}
	w := request("")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &full) != nil || full.Total != 25 || len(full.Items) != 25 || strings.Contains(w.Body.String(), "total_pages") {
		t.Fatal("legacy full-list compatibility broken", w.Code, w.Body.String())
	}
	w = request("page=2&page_size=20")
	var page struct {
		Items      []rcd.HostSetup `json:"items"`
		Total      int             `json:"total"`
		TotalPages int             `json:"total_pages"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Total != 25 || page.TotalPages != 2 || len(page.Items) != 5 || page.Items[0].ID != ids[20] {
		t.Fatal("optional pagination/order mismatch", w.Code, w.Body.String())
	}
	for _, query := range []string{"q=%20FIXTURE-01%20", "q=192.0.2.1&state=failed", "state=failed", "q=" + ids[0]} {
		w = request(query + "&page=1&page_size=20")
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Total != 1 || len(page.Items) != 1 {
			t.Fatal(query, w.Code, w.Body.String())
		}
	}
	for _, query := range []string{"state=unknown-value", "page=0", "page_size=21", "q=" + strings.Repeat("x", 129)} {
		if w = request(query); w.Code != 422 {
			t.Fatal(query, w.Code, w.Body.String())
		}
	}
	for _, secret := range []string{"request_key", "request_hash", "nonce", "admission_id"} {
		if strings.Contains(request("").Body.String(), `"`+secret+`"`) {
			t.Fatal("private field exposed", secret)
		}
	}
}

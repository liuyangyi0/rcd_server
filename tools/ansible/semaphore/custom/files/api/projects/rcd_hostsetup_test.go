// SPDX-License-Identifier: MIT
package projects

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/semaphoreui/semaphore/db"
	dbsql "github.com/semaphoreui/semaphore/db/sql"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/semaphoreui/semaphore/services/rcd"
	"github.com/semaphoreui/semaphore/services/tasks"
)

type setupOptionsStore struct{ db.Store }

func (s *setupOptionsStore) GetAccessKeys(project int, options db.GetAccessKeyOptions, params db.RetrieveQueryParams) ([]db.AccessKey, error) {
	secret := "never-return-this-secret"
	return []db.AccessKey{{ID: 1, Name: "SSH credential", Type: db.AccessKeySSH, Secret: &secret, Plain: &secret, String: secret}, {ID: 2, Name: "sudo credential", Type: db.AccessKeyLoginPassword, LoginPassword: db.LoginPassword{Login: "root", Password: secret}}, {ID: 3, Name: "unrelated", Type: db.AccessKeyNone}}, nil
}

type hostSetupNativeTaskStore struct {
	db.Store
	task db.Task
}

func (s *hostSetupNativeTaskStore) GetTask(project, id int) (db.Task, error) { return s.task, nil }
func (s *hostSetupNativeTaskStore) GetTemplateTasks(project, template int, params db.RetrieveQueryParams) ([]db.TaskWithTpl, error) {
	return nil, nil
}

type hostSetupTaskQueueFunc func(db.Task, *int, string, int, bool) (db.Task, error)

func (f hostSetupTaskQueueFunc) AddTask(task db.Task, user *int, name string, project int, alias bool) (db.Task, error) {
	return f(task, user, name, project, alias)
}

func TestHostSetupExplicitReceiptQueryOnlyRetriesProvenPreAdmissionRejection(t *testing.T) {
	for _, scenario := range []string{"pre_admission", "ambiguous", "no_original_task"} {
		t.Run(scenario, func(t *testing.T) {
			store := dbsql.InitConfigCreateTestStore()
			defer store.Close()
			if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); err != nil {
				t.Fatal(err)
			}
			repo, ctx := store.RCDRepository(), context.Background()
			s, _, err := repo.CreateHostSetup(ctx, 1, rcd.HostSetupRequest{Name: "fixture", Address: "192.0.2.10", SSHPort: 22, SSHUser: "root", SSHKeyID: 1, ServiceAccount: "operator"}, "fixture-query-rejection", "operator", 1)
			if err != nil {
				t.Fatal(err)
			}
			s.TemplateID = 7
			s.HostKeysConfirmed = true
			s.Install = &rcd.HostSetupInstall{PackageID: "original-package", BinarySHA256: "original-binary"}
			s.StartPhase("install", "installation_queued")
			s, err = repo.UpdateHostSetup(ctx, 1, s, s.Revision)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "no_original_task" {
				if err = repo.AttachHostSetupTask(ctx, 1, s.ID, 77); err != nil {
					t.Fatal(err)
				}
				s, _ = repo.HostSetup(ctx, 1, s.ID)
			}
			s.SetResult("unknown", "original reply lost")
			s, err = repo.UpdateHostSetup(ctx, 1, s, s.Revision)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			queue := hostSetupTaskQueueFunc(func(task db.Task, user *int, name string, project int, alias bool) (db.Task, error) {
				calls++
				if calls == 1 {
					if scenario == "pre_admission" {
						return db.Task{}, &tasks.RCDTaskAdmissionError{Err: errors.New("definite native admission rejection")}
					}
					return db.Task{}, errors.New("reply lost after possible native insertion")
				}
				current, err := repo.HostSetup(ctx, 1, s.ID)
				if err != nil || current.Phase != "reconcile" {
					t.Fatal(current, err)
				}
				if err = repo.AttachHostSetupTask(ctx, 1, s.ID, 78); err != nil {
					t.Fatal(err)
				}
				return db.Task{ID: 78, ProjectID: 1, TemplateID: 7}, nil
			})
			fake := &hostSetupNativeTaskStore{Store: store, task: db.Task{ID: 77, ProjectID: 1, TemplateID: 7, Status: task_logger.TaskFailStatus}}
			c := rcdHostSetupController{store: fake, repo: repo, queue: queue}
			submit := func(current rcd.HostSetup) *httptest.ResponseRecorder {
				raw, _ := json.Marshal(map[string]any{"revision": current.Revision, "confirmed": true})
				r := httptest.NewRequest("POST", "/", bytes.NewReader(raw))
				r.Header.Set("If-Match", current.ETag())
				r = helpers.SetContextValue(r, "project", db.Project{ID: 1})
				r = helpers.SetContextValue(r, "user", &db.User{ID: 1, Username: "operator"})
				r = mux.SetURLVars(r, map[string]string{"setup_id": s.ID})
				w := httptest.NewRecorder()
				c.requestReconcile(w, r)
				return w
			}
			first := submit(s)
			if scenario == "no_original_task" {
				if first.Code != 409 || calls != 0 {
					t.Fatal(first.Code, first.Body.String(), calls)
				}
				return
			}
			if first.Code != 202 {
				t.Fatal(first.Code, first.Body.String())
			}
			current, _ := repo.HostSetup(ctx, 1, s.ID)
			if current.QueryNotQueued != (scenario == "pre_admission") || current.InstallationTaskID() != 77 || current.State != "unknown" {
				t.Fatal(current)
			}
			second := submit(current)
			if scenario == "pre_admission" {
				if second.Code != 202 || calls != 2 {
					t.Fatal(second.Code, second.Body.String(), calls)
				}
				got, _ := repo.HostSetup(ctx, 1, s.ID)
				if got.TaskID != 78 || got.InstallationTaskID() != 77 || got.QueryNotQueued || got.Phase != "reconcile" {
					t.Fatal(got)
				}
			} else if second.Code != 409 || calls != 1 {
				t.Fatal("ambiguous request was resubmitted", second.Code, second.Body.String(), calls)
			}
			if _, err = repo.ActiveHostSetup(ctx, 1, s.Request.Address); err != nil {
				t.Fatal("query released unknown operation", err)
			}
		})
	}
}

func TestHostSetupReceiptQueryDoesNotRepeatRunningTaskOrUnconfirmedRequest(t *testing.T) {
	for _, scenario := range []string{"running", "unconfirmed", "stale_revision", "no_install"} {
		t.Run(scenario, func(t *testing.T) {
			store := dbsql.InitConfigCreateTestStore()
			defer store.Close()
			if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); err != nil {
				t.Fatal(err)
			}
			repo, ctx := store.RCDRepository(), context.Background()
			s, _, err := repo.CreateHostSetup(ctx, 1, rcd.HostSetupRequest{Name: "fixture", Address: "192.0.2.10", SSHPort: 22, SSHUser: "root", SSHKeyID: 1, ServiceAccount: "operator"}, "fixture-reconcile-admission", "operator", 1)
			if err != nil {
				t.Fatal(err)
			}
			s.TemplateID = 7
			s.HostKeysConfirmed = true
			s.Install = &rcd.HostSetupInstall{PackageID: "original-package", BinarySHA256: "original-binary"}
			s.StartPhase("install", "installation_queued")
			s, err = repo.UpdateHostSetup(ctx, 1, s, s.Revision)
			if err != nil {
				t.Fatal(err)
			}
			if err = repo.AttachHostSetupTask(ctx, 1, s.ID, 77); err != nil {
				t.Fatal(err)
			}
			s, _ = repo.HostSetup(ctx, 1, s.ID)
			s.SetResult("unknown", "reply lost")
			if scenario == "no_install" {
				s.Install = nil
			}
			s, err = repo.UpdateHostSetup(ctx, 1, s, s.Revision)
			if err != nil {
				t.Fatal(err)
			}
			body := map[string]any{"revision": s.Revision, "confirmed": scenario != "unconfirmed"}
			if scenario == "stale_revision" {
				body["revision"] = s.Revision - 1
			}
			raw, _ := json.Marshal(body)
			r := httptest.NewRequest("POST", "/", bytes.NewReader(raw))
			r.Header.Set("If-Match", s.ETag())
			r = helpers.SetContextValue(r, "project", db.Project{ID: 1})
			r = mux.SetURLVars(r, map[string]string{"setup_id": s.ID})
			fake := &hostSetupNativeTaskStore{Store: store, task: db.Task{ID: 77, ProjectID: 1, TemplateID: 7, Status: task_logger.TaskRunningStatus}}
			c := rcdHostSetupController{store: fake, repo: repo}
			w := httptest.NewRecorder()
			c.requestReconcile(w, r)
			if w.Code != 409 && w.Code != 422 {
				t.Fatal(w.Code, w.Body.String())
			}
			got, _ := repo.HostSetup(ctx, 1, s.ID)
			if got.TaskID != 77 || got.Revision != s.Revision || got.Phase != "install" {
				t.Fatal("query changed or requeued original task", got)
			}
			if _, err = repo.ActiveHostSetup(ctx, 1, s.Request.Address); err != nil {
				t.Fatal("query released unknown reservation", err)
			}
		})
	}
}

func TestHostSetupReceiptQueryLostResponseReturnsSameQueryWithoutResubmission(t *testing.T) {
	store := dbsql.InitConfigCreateTestStore()
	defer store.Close()
	if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); err != nil {
		t.Fatal(err)
	}
	repo, ctx := store.RCDRepository(), context.Background()
	s, _, err := repo.CreateHostSetup(ctx, 1, rcd.HostSetupRequest{Name: "fixture", Address: "192.0.2.10", SSHPort: 22, SSHUser: "root", SSHKeyID: 1, ServiceAccount: "operator"}, "fixture-reconcile-repeat", "operator", 1)
	if err != nil {
		t.Fatal(err)
	}
	s.OriginalInstallTaskID = 77
	s.ReconcileRevision = s.Revision
	requestRevision := s.Revision
	s.StartPhase("reconcile", "reconcile_queued")
	s, err = repo.UpdateHostSetup(ctx, 1, s, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.AttachHostSetupTask(ctx, 1, s.ID, 78); err != nil {
		t.Fatal(err)
	}
	s, _ = repo.HostSetup(ctx, 1, s.ID)
	raw, _ := json.Marshal(map[string]any{"revision": requestRevision, "confirmed": true})
	r := httptest.NewRequest("POST", "/", bytes.NewReader(raw))
	r.Header.Set("If-Match", `"1"`)
	r = helpers.SetContextValue(r, "project", db.Project{ID: 1})
	r = mux.SetURLVars(r, map[string]string{"setup_id": s.ID})
	c := rcdHostSetupController{repo: repo}
	w := httptest.NewRecorder()
	c.requestReconcile(w, r)
	var got rcd.HostSetup
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.TaskID != 78 || got.OriginalInstallTaskID != 77 || got.Revision != s.Revision {
		t.Fatal(w.Code, w.Body.String())
	}
}
func (s *setupOptionsStore) GetTemplates(project int, filter db.TemplateFilter, params db.RetrieveQueryParams) ([]db.Template, error) {
	return []db.Template{}, nil
}

func TestHostSetupOptionsOnlyReturnNativeCredentialReferences(t *testing.T) {
	c := rcdHostSetupController{store: &setupOptionsStore{}}
	request := httptest.NewRequest("GET", "/", nil)
	request = helpers.SetContextValue(request, "project", db.Project{ID: 1})
	response := httptest.NewRecorder()
	c.options(response, request)
	body := response.Body.String()
	if response.Code != 200 || strings.Contains(body, "never-return") || strings.Contains(body, "private_key") || strings.Contains(body, "login_password\":{") {
		t.Fatal(response.Code, body)
	}
	if !strings.Contains(body, `"id":1`) || !strings.Contains(body, `"available":false`) {
		t.Fatal(body)
	}
}

func TestHostSetupCreateRetryReturnsOriginalAfterDeviceRegistration(t *testing.T) {
	store := dbsql.InitConfigCreateTestStore()
	defer store.Close()
	if _, e := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); e != nil {
		t.Fatal(e)
	}
	repo := store.RCDRepository()
	req := rcd.HostSetupRequest{Name: "fixture", Address: "2001:db8:0:0::10", SSHPort: 22, SSHUser: "operator", SSHKeyID: 4, ServiceAccount: "operator"}
	original, _, e := repo.CreateHostSetup(context.Background(), 1, req, "fixture-original", "operator", 1)
	if e != nil {
		t.Fatal(e)
	}
	d, e := repo.AddDevice(context.Background(), 1, rcd.DeviceBinding{Name: req.Name, Address: "2001:db8::10", HostAlias: "existing", InventoryID: 4, TemplateID: 5}, "operator")
	if e != nil {
		t.Fatal(e)
	}
	original.DeviceID = d.ID
	original.SetResult("ready_for_install", "")
	original, e = repo.UpdateHostSetup(context.Background(), 1, original, original.Revision)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(req)
	request := httptest.NewRequest("POST", "/", bytes.NewReader(raw))
	request.Header.Set("Idempotency-Key", "fixture-original")
	request = helpers.SetContextValue(request, "project", db.Project{ID: 1})
	// No native resource/credential methods may be invoked on this retry.
	controller := rcdHostSetupController{repo: repo}
	response := httptest.NewRecorder()
	controller.create(response, request)
	var actual rcd.HostSetup
	if json.Unmarshal(response.Body.Bytes(), &actual) != nil || response.Code != 200 || actual.ID != original.ID {
		t.Fatal(response.Code, response.Body.String())
	}
}

func TestHostSetupCancelCannotReleaseUnknownOrInstalling(t *testing.T) {
	store := dbsql.InitConfigCreateTestStore()
	defer store.Close()
	if _, e := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); e != nil {
		t.Fatal(e)
	}
	repo := store.RCDRepository()
	s, _, e := repo.CreateHostSetup(context.Background(), 1, rcd.HostSetupRequest{Name: "fixture", Address: "192.0.2.10", SSHPort: 22, SSHUser: "operator", SSHKeyID: 1, ServiceAccount: "operator"}, "fixture-cancel", "operator", 1)
	if e != nil {
		t.Fatal(e)
	}
	controller := rcdHostSetupController{store: store, repo: repo}
	cancel := func() *httptest.ResponseRecorder {
		raw, _ := json.Marshal(map[string]int{"revision": s.Revision})
		request := httptest.NewRequest("POST", "/", bytes.NewReader(raw))
		request.Header.Set("If-Match", s.ETag())
		request = helpers.SetContextValue(request, "project", db.Project{ID: 1})
		request = mux.SetURLVars(request, map[string]string{"setup_id": s.ID})
		response := httptest.NewRecorder()
		controller.cancel(response, request)
		return response
	}
	s.SetResult("unknown", "reply missing")
	s, e = repo.UpdateHostSetup(context.Background(), 1, s, s.Revision)
	if e != nil {
		t.Fatal(e)
	}
	if response := cancel(); response.Code != 409 {
		t.Fatal(response.Code, response.Body.String())
	}
	if _, e = repo.ActiveHostSetup(context.Background(), 1, s.Request.Address); e != nil {
		t.Fatal("unknown lock lost", e)
	}
	s.SetResult("awaiting_host_key", "")
	s, e = repo.UpdateHostSetup(context.Background(), 1, s, s.Revision)
	if e != nil {
		t.Fatal(e)
	}
	if response := cancel(); response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	if _, e = repo.ActiveHostSetup(context.Background(), 1, s.Request.Address); !errors.Is(e, rcd.ErrNotFound) {
		t.Fatal("cancelled preinstall lock retained", e)
	}
}

type setupScheduleStore struct {
	db.Store
	calls int
}

func (s *setupScheduleStore) GetTemplateSchedules(project, template int, only bool) ([]db.Schedule, error) {
	s.calls++
	if s.calls == 1 {
		return nil, errors.New("temporary schedule store failure")
	}
	return []db.Schedule{{TemplateID: template, Active: true, CronFormat: "* * * * *"}}, nil
}
func TestHostSetupEnsureScheduleCanRecoverAfterRegistration(t *testing.T) {
	store := &setupScheduleStore{}
	c := rcdHostSetupController{store: store}
	request := httptest.NewRequest("GET", "/", nil)
	request = helpers.SetContextValue(request, "project", db.Project{ID: 1})
	s := rcd.HostSetup{DeviceID: "already-registered", InspectionTemplateID: 12}
	if c.ensureSchedule(request, s) == nil {
		t.Fatal("expected temporary failure")
	}
	if e := c.ensureSchedule(request, s); e != nil || store.calls != 2 {
		t.Fatal("registered device skipped schedule repair", store.calls, e)
	}
}

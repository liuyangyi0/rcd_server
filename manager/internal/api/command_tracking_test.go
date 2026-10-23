package api

import (
	"context"
	"net/http/httptest"
	"rcd_manager/internal/domain"
	"testing"
)

type commandDetailFake struct {
	*fakeService
	calls int
}

func (f *commandDetailFake) CommandDetail(_ context.Context, id string) (domain.CommandDetail, error) {
	if id == "missing" {
		return domain.CommandDetail{}, domain.ErrNotFound
	}
	return domain.CommandDetail{Command: domain.PendingCommand{ID: id}}, nil
}
func (f *commandDetailFake) CommandEvents(_ context.Context, _ string, limit, offset int) ([]domain.TrackedCommand, int, error) {
	f.calls++
	return []domain.TrackedCommand{}, 0, nil
}
func TestCommandDetailsAndEventPagination(t *testing.T) {
	f := &commandDetailFake{fakeService: &fakeService{}}
	handler := New(f, Options{})
	for path, status := range map[string]int{"/commands/id": 200, "/commands/missing": 404, "/commands/id/events?limit=1&offset=0": 200, "/commands/id/events?limit=0": 400, "/commands/id/events?offset=-1": 400, "/commands/id/events?limit=bad": 400} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "/api/v1"+path, nil))
		if response.Code != status {
			t.Fatalf("%s %d %s", path, response.Code, response.Body.String())
		}
	}
	if f.calls != 1 {
		t.Fatal("invalid pagination reached service")
	}
}

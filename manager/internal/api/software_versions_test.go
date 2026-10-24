package api

import (
	"context"
	"net/http/httptest"
	"rcd_manager/internal/domain"
	"strings"
	"testing"
)

type softwareServiceStub struct {
	fakeService
	refresh bool
}

func (s *softwareServiceStub) SoftwareVersions(_ context.Context, refresh bool) ([]domain.SoftwareVersion, error) {
	s.refresh = refresh
	return []domain.SoftwareVersion{{Name: "当前管理端", Component: "manager", State: "online"}}, nil
}
func TestSoftwareVersionRouteIsReadOnlyAndOptional(t *testing.T) {
	service := &softwareServiceStub{}
	h := New(service, Options{})
	for _, query := range []string{"", "?refresh=1"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/software-versions"+query, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "manager") || service.refresh != (query != "") {
			t.Fatalf("%d %s refresh=%v", w.Code, w.Body.String(), service.refresh)
		}
	}
	w := httptest.NewRecorder()
	New(&fakeService{}, Options{}).ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/software-versions", nil))
	if w.Code != 501 {
		t.Fatalf("unsupported: %d", w.Code)
	}
}

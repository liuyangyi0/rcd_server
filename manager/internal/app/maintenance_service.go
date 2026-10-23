package app

import (
	"sync"

	"rcd_manager/internal/domain"
)

// maintenanceService owns lower mutation leases and durable process fences.
// It never performs I/O or calls another component while holding its mutex.
// Runtime's short config transaction coordinates admission with RouteTable;
// it is not the owner of these maps. A process fence outlives its RPC lease
// until a terminal outcome has been persisted (unknown is not terminal).
type maintenanceService struct {
	mu        sync.Mutex
	next      uint64
	mutations map[string]uint64
	fences    map[string]string
	executing map[string]string
}

func processMaintenanceBusy() error {
	return &domain.OperationError{Code: "busy", Message: "该下级有待确认的进程操作，命令及运维修改暂不可用；请核对原请求"}
}

func (s *maintenanceService) checkLocked(source string, processOnly bool) error {
	if s.fences[source] != "" || s.executing[source] != "" {
		return processMaintenanceBusy()
	}
	if !processOnly && s.mutations[source] != 0 {
		return &domain.OperationError{Code: "busy", Message: "该下级有运维修改正在执行，请刷新状态后重试"}
	}
	return nil
}

func (s *maintenanceService) check(source string, processOnly bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkLocked(source, processOnly)
}

func (s *maintenanceService) beginMutation(source string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(source, false); err != nil {
		return 0, err
	}
	if s.mutations == nil {
		s.mutations = make(map[string]uint64)
	}
	s.next++
	if s.next == 0 {
		s.next++
	}
	s.mutations[source] = s.next
	return s.next, nil
}

func (s *maintenanceService) endMutation(source string, lease uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mutations[source] == lease {
		delete(s.mutations, source)
	}
}

func (s *maintenanceService) beginProcess(source, request string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(source, false); err != nil {
		return err
	}
	if s.fences == nil {
		s.fences = make(map[string]string)
	}
	if s.executing == nil {
		s.executing = make(map[string]string)
	}
	s.fences[source], s.executing[source] = request, request
	return nil
}

func (s *maintenanceService) releaseFence(source, request string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fences[source] == request {
		delete(s.fences, source)
	}
}

func (s *maintenanceService) endExecution(source, request string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.executing[source] == request {
		delete(s.executing, source)
	}
}

func (s *maintenanceService) tryReconcile(source, request string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.executing[source] != "" || s.mutations[source] != 0 {
		return false
	}
	if pending := s.fences[source]; pending != "" && pending != request {
		return false
	}
	if s.executing == nil {
		s.executing = make(map[string]string)
	}
	s.executing[source] = request
	return true
}

func (s *maintenanceService) restoreFence(source, request string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current := s.fences[source]; current != "" && current != request {
		return processMaintenanceBusy()
	}
	if s.fences == nil {
		s.fences = make(map[string]string)
	}
	s.fences[source] = request
	return nil
}

func (s *maintenanceService) pending(source string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fences[source]
}

func (s *maintenanceService) pendingSnapshot() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]string, len(s.fences))
	for source, request := range s.fences {
		result[source] = request
	}
	return result
}

func (s *maintenanceService) maintained(source string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutations[source] != 0 || s.fences[source] != "" || s.executing[source] != ""
}

func (s *maintenanceService) activeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.mutations) + len(s.fences) + len(s.executing)
}

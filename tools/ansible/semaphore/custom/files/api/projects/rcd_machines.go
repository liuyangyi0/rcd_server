// SPDX-License-Identifier: MIT
package projects

import (
	"encoding/json"
	"github.com/gorilla/mux"
	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/semaphoreui/semaphore/services/rcd"
	"io"
	"net/http"
	"time"
)

func (c *rcdDevicesController) fleet(w http.ResponseWriter, r *http.Request) {
	items, e := c.repo.Devices(r.Context(), rcdProject(r))
	if e != nil {
		deviceError(w, e)
		return
	}
	machines := []rcd.Device{}
	for _, d := range items {
		if d.AgentInstanceID != "" {
			full, err := c.repo.Device(r.Context(), rcdProject(r), d.ID)
			if err != nil {
				deviceError(w, err)
				return
			}
			machines = append(machines, full)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	helpers.WriteJSON(w, 200, map[string]any{"schema_version": 1, "machines": machines})
}

// Read-only target verification precedes binding; the HTTP request never
// starts/stops a service. Native queue remains the only execution entry.
func (c *rcdDevicesController) bindMachine(w http.ResponseWriter, r *http.Request) {
	d, e := c.repo.Device(r.Context(), rcdProject(r), mux.Vars(r)["device_id"])
	if e != nil {
		deviceError(w, e)
		return
	}
	if r.Header.Get("If-Match") != d.ETag() {
		deviceError(w, rcd.ErrConflict)
		return
	}
	var input struct {
		Revision int `json:"revision"`
		rcd.MachineBinding
	}
	raw, e := io.ReadAll(io.LimitReader(r.Body, 4097))
	if e != nil || len(raw) > 4096 || rcd.StrictJSON(raw, &input) != nil || input.Revision != d.Revision || input.Validate(d.Address) != nil {
		deviceError(w, rcd.ErrInvalid)
		return
	}
	confirmedManager := ""
	for _, o := range d.Components {
		if o.Component == "manager" && o.Compatibility != nil {
			confirmedManager = o.Compatibility.InstanceID
		}
	}
	if d.State != "checked" || confirmedManager == "" || confirmedManager != input.ManagerInstanceID {
		helpers.WriteJSON(w, 409, map[string]string{"error": "需要近期只读检查确认管理端稳定身份；绑定未更改"})
		return
	}
	client := http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	req, e := http.NewRequestWithContext(r.Context(), "GET", input.AgentURL+"/api/v1/process", nil)
	if e != nil {
		deviceError(w, rcd.ErrInvalid)
		return
	}
	req.Header.Set("X-RCD-Instance-ID", input.AgentInstanceID)
	response, e := client.Do(req)
	if e != nil {
		helpers.WriteJSON(w, 503, map[string]string{"error": "Agent 无法访问，绑定未更改"})
		return
	}
	defer response.Body.Close()
	raw, e = io.ReadAll(io.LimitReader(response.Body, 65537))
	var identity struct {
		Instance    string `json:"instance_id"`
		Gateway     string `json:"gateway_instance_id"`
		Lifecycle   bool   `json:"lifecycle_supported"`
		Pending     string `json:"pending_request_id"`
		Maintenance string `json:"maintenance_deployment_id"`
		Job         *struct {
			State string `json:"state"`
		} `json:"lifecycle"`
	}
	if e != nil || response.StatusCode != 200 || len(raw) > 65536 || json.Unmarshal(raw, &identity) != nil || identity.Instance != input.AgentInstanceID || identity.Gateway != input.GatewayInstanceID || !identity.Lifecycle {
		helpers.WriteJSON(w, 409, map[string]string{"error": "Agent/下级身份或安全启停能力不匹配，绑定未更改"})
		return
	}
	if identity.Pending != "" || identity.Maintenance != "" || (identity.Job != nil && identity.Job.State != "succeeded" && identity.Job.State != "failed") {
		helpers.WriteJSON(w, 409, map[string]string{"error": "目标正在运维或结果未知，暂不更改绑定"})
		return
	}
	d, e = c.repo.BindMachine(r.Context(), rcdProject(r), d.ID, input.Revision, input.MachineBinding)
	if e != nil {
		deviceError(w, e)
		return
	}
	w.Header().Set("ETag", d.ETag())
	helpers.WriteJSON(w, 200, d)
}

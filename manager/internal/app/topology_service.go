package app

import (
	"context"
	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"sort"
)

func (r *Runtime) GetSourceTopology(_ context.Context, sourceID string) (api.SourceTopologyView, error) {
	r.configMu.RLock()
	defer r.configMu.RUnlock()
	source, err := r.source(sourceID)
	if err != nil {
		return api.SourceTopologyView{}, err
	}
	r.mu.RLock()
	catalog := r.catalogs[sourceID]
	r.mu.RUnlock()
	view := api.SourceTopologyView{SourceID: source.ID, GroupID: source.GroupID, Revision: source.Revision, CatalogState: source.CatalogState, Ports: []api.TopologyPortView{}}
	for _, node := range catalog.Nodes {
		if node.Retired || node.Class != domain.NodeClassVariable || node.Kind == "command" {
			continue
		}
		bound := false
		for _, binding := range source.GroupBindings {
			if binding.PortID == node.PortID && binding.DeviceID == node.DeviceID {
				bound = true
				break
			}
		}
		if !bound {
			view.UnassignedPointCount++
		}
	}
	if catalog.Topology == nil {
		return view, nil
	}
	view.Supported = true
	view.SchemaVersion = catalog.Topology.SchemaVersion
	view.CalculationCount = len(catalog.Topology.Calculations)
	calculationCounts := make(map[domain.TopologyOwner]int)
	for _, calculation := range catalog.Topology.Calculations {
		if len(calculation.Owners) == 1 {
			calculationCounts[calculation.Owners[0]]++
		}
		if len(calculation.Owners) > 1 {
			view.CrossDeviceCalculationCount++
		}
	}
	for _, port := range catalog.Topology.Ports {
		portView := api.TopologyPortView{PortID: port.PortID, PortName: port.PortName, SendNodeID: port.SendNodeID, SendMethodID: port.SendMethodID, Devices: []api.TopologyDeviceView{}}
		for _, device := range port.Devices {
			deviceView := api.TopologyDeviceView{DeviceID: device.DeviceID, DeviceAddress: device.DeviceAddress, Name: device.Name, KKSPrefix: device.KKSPrefix, PointCount: len(device.NodeIDs), GroupID: source.GroupForDevice(port.PortID, device.DeviceID), CalculationCount: calculationCounts[domain.TopologyOwner{PortID: port.PortID, DeviceID: device.DeviceID}]}
			for _, binding := range source.GroupBindings {
				if binding.PortID == port.PortID && binding.DeviceID == device.DeviceID {
					deviceView.Bound = true
					break
				}
			}
			portView.PointCount += deviceView.PointCount + deviceView.CalculationCount
			portView.Devices = append(portView.Devices, deviceView)
		}
		sort.Slice(portView.Devices, func(i, j int) bool { return portView.Devices[i].DeviceAddress < portView.Devices[j].DeviceAddress })
		view.Ports = append(view.Ports, portView)
	}
	sort.Slice(view.Ports, func(i, j int) bool { return view.Ports[i].PortID < view.Ports[j].PortID })
	return view, nil
}

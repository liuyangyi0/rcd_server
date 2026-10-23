// SPDX-License-Identifier: MIT
package rcd

import (
	"fmt"
	"strconv"
	"strings"
)

func versionAtLeast(actual, minimum string) bool {
	actual = strings.Split(strings.Split(actual, "+")[0], "-")[0]
	a, b := strings.Split(actual, "."), strings.Split(minimum, ".")
	if len(a) != 3 || len(b) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		av, e := strconv.Atoi(a[i])
		bv, f := strconv.Atoi(b[i])
		if e != nil || f != nil || av < 0 || bv < 0 {
			return false
		}
		if av != bv {
			return av > bv
		}
	}
	return true
}

// Cache is an early UI guard, never permission to skip the install-time check.
func CompatibilityReason(d Device, m Manifest) string {
	if d.State != "checked" {
		return "状态检查已过期或失败，请先检查状态"
	}
	var c *ComponentObservation
	for i := range d.Components {
		if d.Components[i].Component == m.Component {
			c = &d.Components[i]
		}
	}
	if c == nil || (c.State != "running" && c.State != "stopped") {
		if c != nil && c.State == "not_installed" {
			return "尚未安装，请从设备接入中的首次安装操作安装，不可直接升级"
		}
		return "目标程序状态无法确认"
	}
	if m.Component == "communicate" && c.State != "stopped" {
		return "通信程序尚不支持可信的活动通信排空；请由维护人员受控停止后再升级，平台不会自动停止它"
	}
	build := c.RunningBuild
	if c.State == "stopped" {
		build = c.DiskBuild
	}
	if build == nil || Architecture(build.Target) == "" || Architecture(build.Target) != Architecture(m.Target) {
		return "适用架构不兼容或缺少可信版本信息"
	}
	if c.Compatibility == nil {
		return "尚未取得数据格式检查，请先检查状态"
	}
	observed := c.Compatibility
	if observed.Error != "" {
		return observed.Error
	}
	if !versionAtLeast(observed.AdapterVersion, m.Compatibility.AdapterMinVersion) {
		return "升级适配器版本过低"
	}
	if observed.StateFormats == nil || len(observed.StateFormats) != len(m.Compatibility.StateFormats) {
		return "当前数据格式检查不完整"
	}
	for name, bounds := range m.Compatibility.StateFormats {
		value, ok := observed.StateFormats[name]
		if !ok || bounds.Min == nil || bounds.Max == nil {
			return "当前数据格式检查不完整"
		}
		if value < *bounds.Min || value > *bounds.Max {
			return fmt.Sprintf("数据格式不兼容：%s 当前 %d，软件包支持 %d–%d", name, value, *bounds.Min, *bounds.Max)
		}
	}
	if m.Component == "agent" && c.State == "running" {
		for _, v := range d.Components {
			if v.Component == "lower" && v.State == "stopped" {
				bounds := m.Compatibility.StateFormats["agent_state"]
				if bounds.Max == nil || *bounds.Max < 3 {
					return "下级已停止，目标 Agent 必须支持停止维护状态格式 3"
				}
			}
		}
	}
	for component, minimum := range m.Compatibility.ComponentMinVersions {
		found := false
		for _, v := range d.Components {
			if v.Component == component {
				b := v.RunningBuild
				if v.State == "stopped" {
					b = v.DiskBuild
				}
				found = b != nil && versionAtLeast(b.Version, minimum)
			}
		}
		if !found {
			return "依赖组件 " + component + " 未满足最低版本 " + minimum
		}
	}
	return ""
}

package fleetcharging

import "time"

// Milliwatts 功率，单位毫瓦（整数，避免浮点误差）。
type Milliwatts int64

// Millijoules 能量，单位毫焦（毫瓦·秒，整数，保证能量计算精确）。
type Millijoules int64

// energyOver 计算功率 p 在时长 d 内产生的能量。d 必须为整秒。
func (p Milliwatts) energyOver(d time.Duration) Millijoules {
	return Millijoules(int64(p) * int64(d/time.Second))
}

// Slot 站点的一个离散配置时段，区间为左闭右开 [Start, End)。
type Slot struct {
	Start    time.Time  `json:"start"`
	End      time.Time  `json:"end"`
	Capacity Milliwatts `json:"capacity"`
}

// Contains 报告 [start, end) 是否完全落在该时段内。
func (s Slot) Contains(start, end time.Time) bool {
	return !start.Before(s.Start) && !end.After(s.End) && start.Before(end)
}

// Request 车辆充电请求。
type Request struct {
	// ExternalID 外部请求号，用于幂等。
	ExternalID string `json:"external_id"`
	// VehicleID 车辆标识。
	VehicleID string `json:"vehicle_id"`
	// Arrival 到达时间（左闭），必须秒对齐且在未来。
	Arrival time.Time `json:"arrival"`
	// Departure 离开时间（右开），必须秒对齐且晚于到达。
	Departure time.Time `json:"departure"`
	// MinEnergy 离开前必须获得的最低能量。
	MinEnergy Millijoules `json:"min_energy"`
	// MaxPower 每时段允许分配的最大功率。
	MaxPower Milliwatts `json:"max_power"`
}

// UpdateRequest 修改计划时的新参数。
type UpdateRequest struct {
	Arrival   time.Time   `json:"arrival"`
	Departure time.Time   `json:"departure"`
	MinEnergy Millijoules `json:"min_energy"`
	MaxPower  Milliwatts  `json:"max_power"`
}

// Allocation 计划在某个时段内的功率分配。
type Allocation struct {
	// SlotStart 所属站点时段的起点。
	SlotStart time.Time `json:"slot_start"`
	// Start/End 实际占用区间（请求区间与时段的交集，左闭右开）。
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// Power 该区间内分配的功率。
	Power Milliwatts `json:"power"`
}

// Energy 该分配产生的能量。
func (a Allocation) Energy() Millijoules {
	return a.Power.energyOver(a.End.Sub(a.Start))
}

// PlanState 计划状态。
type PlanState string

const (
	// PlanActive 计划生效中，占用容量。
	PlanActive PlanState = "active"
	// PlanCancelled 计划已取消，不再占用容量。
	PlanCancelled PlanState = "cancelled"
)

// Plan 一份充电计划。
type Plan struct {
	ID          string       `json:"id"`
	Request     Request      `json:"request"`
	Allocations []Allocation `json:"allocations"`
	// Energy 全部分配的能量之和，保证 >= Request.MinEnergy。
	Energy    Millijoules `json:"energy"`
	State     PlanState   `json:"state"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}

// SlotOccupancy 单个时段的占用情况。
type SlotOccupancy struct {
	Start     time.Time  `json:"start"`
	End       time.Time  `json:"end"`
	Capacity  Milliwatts `json:"capacity"`
	Allocated Milliwatts `json:"allocated"`
	Available Milliwatts `json:"available"`
}

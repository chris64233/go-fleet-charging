package fleetcharging

import "time"

// Interval 是左闭右开的半开时间区间 [Start, End)。
// 能量按区间与时段的真实相交长度计算，不做整点对齐假设。
type Interval struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// StationConfig 是站点在离散时段上的可用功率配置。
// Segments 必须互不重叠且按 Start 升序；相邻时段允许相接，
// 时段之间的空隙表示该段时间站点不可用（容量为 0）。
// 单位约定：PowerW 为瓦（W）。
type StationConfig struct {
	StationID string    `json:"station_id"`
	Segments  []Segment `json:"segments"`
}

// Segment 描述一个半开时段 [Start, End) 内站点可提供的恒定功率。
type Segment struct {
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	PowerW int64     `json:"power_w"`
}

// ChargeRequest 是一辆车提交（或修改后）的充电请求内容。
//   - Arrival/Departure: 在场半开区间 [Arrival, Departure)
//   - MinEnergyUWh: 离开前必须充到的最低电量，单位微瓦时（µWh，1 Wh = 1e6 µWh）
//   - MaxPowerW: 单车每时段可接受的最大功率（瓦）
//   - RequestID: 外部请求号，用于幂等
type ChargeRequest struct {
	RequestID    string    `json:"request_id"`
	StationID    string    `json:"station_id"`
	VehicleID    string    `json:"vehicle_id"`
	Arrival      time.Time `json:"arrival"`
	Departure    time.Time `json:"departure"`
	MinEnergyUWh int64     `json:"min_energy_uwh"`
	MaxPowerW    int64     `json:"max_power_w"`
}

// PlanStatus 计划生命周期状态。
type PlanStatus string

const (
	// StatusActive 计划有效且车辆尚未抵达，占用容量，允许修改或取消。
	StatusActive PlanStatus = "active"
	// StatusCancelled 计划已取消，不再占用任何容量，终态。
	StatusCancelled PlanStatus = "cancelled"
	// StatusArrived 车辆已抵达，计划锁定，不允许修改或取消，终态。
	StatusArrived PlanStatus = "arrived"
)

// Plan 是一次成功提交后落库的充电计划。
// Allocation 给出每个相交配置时段内实际分配的恒定功率；能量可行性在提交时已校验。
type Plan struct {
	ID         string           `json:"id"`
	RequestID  string           `json:"request_id"` // 提交时的外部请求号，计划的稳定外部标识
	StationID  string           `json:"station_id"`
	VehicleID  string           `json:"vehicle_id"`
	Arrival    time.Time        `json:"arrival"`
	Departure  time.Time        `json:"departure"`
	MinEnergy  int64            `json:"min_energy_uwh"`
	MaxPowerW  int64            `json:"max_power_w"`
	Allocation []SlotAllocation `json:"allocation"`
	Status     PlanStatus       `json:"status"`
	CreatedAt  time.Time        `json:"created_at"`
	UpdatedAt  time.Time        `json:"updated_at"`
	Revision   int              `json:"revision"`
}

// SlotAllocation 是计划在站点某段时间上的功率分配（半开区间，边界与配置时段对齐）。
type SlotAllocation struct {
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	PowerW int64     `json:"power_w"`
}

// SlotOccupancy 是逐时段占用查询的一行：该半开区间内站点容量、已占用、剩余。
// 当有计划在配置时段中途到达/离开时，该时段会被进一步切成多行。
type SlotOccupancy struct {
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	CapacityW  int64     `json:"capacity_w"`
	ReservedW  int64     `json:"reserved_w"`
	AvailableW int64     `json:"available_w"`
}

// 能量精确性：
//
// 内部一律以“功量” W·ns（瓦 × 纳秒时长）做整数运算，使用 128 位整数存放乘积，
// 全程无浮点、无预先取整；功率在最后一步按 ceil(剩余功量 / 时长) 取整为整数瓦，
// 因而交付能量一定 >= 请求能量，误差有界且不会少给。
//
// 1 µWh = 1e-6 Wh = 1e-6 × 3600 W·s = 3_600_000 W·ns。
const nanoWorkPerMicroWh = int64(3_600_000)

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
//   - Priority: 预约优先级，数值越大越优先受保护；站点容量临时下降时，
//     低优先级预约先被削减，高优先级预约尽量维持原安排
//   - FixedPower: 固定功率预约；容量削减时其功率分配完全不允许降低
//   - RequestID: 外部请求号，用于幂等
type ChargeRequest struct {
	RequestID    string    `json:"request_id"`
	StationID    string    `json:"station_id"`
	VehicleID    string    `json:"vehicle_id"`
	Arrival      time.Time `json:"arrival"`
	Departure    time.Time `json:"departure"`
	MinEnergyUWh int64     `json:"min_energy_uwh"`
	MaxPowerW    int64     `json:"max_power_w"`
	Priority     int32     `json:"priority"`
	FixedPower   bool      `json:"fixed_power"`
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
	Priority   int32            `json:"priority"`
	FixedPower bool             `json:"fixed_power"`
	Allocation []SlotAllocation `json:"allocation"`
	Status     PlanStatus       `json:"status"`
	CreatedAt  time.Time        `json:"created_at"`
	UpdatedAt  time.Time        `json:"updated_at"`
	Revision   int              `json:"revision"`
}

// CurtailmentEvent 描述一次站点可用功率的临时下降。
// 在半开区间 [Start, End) 内，站点容量被替换为 PowerW（瓦）；
// 区间之外容量不变。Start/End 不必与既有配置时段对齐，
// 系统按真实相交长度计算。
type CurtailmentEvent struct {
	StationID string    `json:"station_id"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	PowerW    int64     `json:"power_w"`
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

// Adjustment 是容量削减方案中一笔预约的调整结果（确认前的建议值）。
// Revision 是生成方案时该预约的版本号；确认时版本若已变化，整份方案失败。
// OldAllocation/NewAllocation 均为该预约完整在场期内的分配：
// 被挤出事件窗口的功率可能转移到其在场期内窗口之外的空闲时段。
type Adjustment struct {
	RequestID     string
	Revision      int
	Priority      int32
	FixedPower    bool
	OldAllocation []SlotAllocation
	NewAllocation []SlotAllocation
}

// planVersion 记录确认时需要复核的一笔预约的版本指纹。
type planVersion struct {
	requestID string
	revision  int
	status    PlanStatus
}

// CurtailmentPlan 是一次容量削减生成的、可确认的调整方案。
// 方案是状态的不可变快照：自身不改任何数据，凭它调用 ConfirmCurtailment 才落库。
// 方案中携带的版本指纹用于乐观并发控制——方案生成后，只要涉及的站点配置、
// 任何一笔相关预约（含被取消/被修改）或事件窗口内的预约集合发生变化，
// 确认即整体失败（KindConflict），不会出现部分修改。
type CurtailmentPlan struct {
	Event       CurtailmentEvent
	Adjustments []Adjustment

	// 以下字段为确认时的乐观锁指纹，调用方只需透传，不应解读。
	stationHash string
	versions    []planVersion
	scopeStart  time.Time
	scopeEnd    time.Time
}

// ChangedReports 返回方案中分配实际发生变化的预约外部号（固定预约与未受影响的预约除外）。
func (p *CurtailmentPlan) ChangedRequests() []string {
	out := make([]string, 0, len(p.Adjustments))
	for _, a := range p.Adjustments {
		if a.FixedPower || allocationsEqual(a.OldAllocation, a.NewAllocation) {
			continue
		}
		out = append(out, a.RequestID)
	}
	return out
}

// allocationsEqual 比较两段（已按时间排序的）分配是否完全相同。
func allocationsEqual(a, b []SlotAllocation) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// 能量精确性：
//
// 内部一律以“功量” W·ns（瓦 × 纳秒时长）做整数运算，使用 128 位整数存放乘积，
// 全程无浮点、无预先取整；功率在最后一步按 ceil(剩余功量 / 时长) 取整为整数瓦，
// 因而交付能量一定 >= 请求能量，误差有界且不会少给。
//
// 1 µWh = 1e-6 Wh = 1e-6 × 3600 W·s = 3_600_000 W·ns。
const nanoWorkPerMicroWh = int64(3_600_000)

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
//   - Priority: 容量紧张时的保留优先级，数值越大越优先；降容调整只削减低优先级预约
//   - Fixed: 固定功率预约，任何时段的分配功率都不允许被降容方案削减
type ChargeRequest struct {
	RequestID    string    `json:"request_id"`
	StationID    string    `json:"station_id"`
	VehicleID    string    `json:"vehicle_id"`
	Arrival      time.Time `json:"arrival"`
	Departure    time.Time `json:"departure"`
	MinEnergyUWh int64     `json:"min_energy_uwh"`
	MaxPowerW    int64     `json:"max_power_w"`
	Priority     int       `json:"priority"`
	Fixed        bool      `json:"fixed"`
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
	Priority   int              `json:"priority"`
	Fixed      bool             `json:"fixed"`
	Allocation []SlotAllocation `json:"allocation"`
	Status     PlanStatus       `json:"status"`
	CreatedAt  time.Time        `json:"created_at"`
	UpdatedAt  time.Time        `json:"updated_at"`
	Revision   int              `json:"revision"`
}

// CapacityEvent 描述站点可用功率的临时下降（降容事件）。
// 半开时段 [Start, End) 内站点可用功率被覆盖为 PowerW，事件结束后恢复原配置；
// EventID 是外部事件号（仅用于追踪，不参与去重；同一事件可多次准备，各得独立方案）。
// PowerW 必须低于事件所覆盖时段的原容量，否则不属于“容量下降”，返回 KindParameter。
type CapacityEvent struct {
	EventID   string    `json:"event_id"`
	StationID string    `json:"station_id"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	PowerW    int64     `json:"power_w"`
}

// AdjustmentItem 是降容方案中一笔预约的调整明细（覆盖其完整在场窗口）。
// FromRevision 是方案生成时该预约的版本号；确认时必须仍与此一致，否则方案过期。
// 固定预约与无需变动的预约也会出现在方案中，Allocation 与当前分配相同，
// 仅作为版本守卫——它们在确认前若被修改/取消，整份方案同样失败。
type AdjustmentItem struct {
	RequestID    string
	Priority     int
	Fixed        bool
	FromRevision int
	Allocation   []SlotAllocation // 调整后完整分配（含事件窗口之外不变的部分）
}

// CurtailmentPlan 是一份待确认的降容调整方案。
// 方案只在内存中持有、不入库；ConfirmToken 用于防止同一方案被重复确认。
type CurtailmentPlan struct {
	Token     string
	Event     CapacityEvent
	Segments  []Segment        // 应用事件后的站点完整时段配置
	Items     []AdjustmentItem // 受事件影响、需要随容量一起更新的预约
	CreatedAt time.Time
}

// 方案状态摘要行，便于调用方在确认后核对每笔预约的落实结果。
type AdjustmentResult struct {
	RequestID    string
	FromRevision int
	ToRevision   int
	Allocation   []SlotAllocation
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

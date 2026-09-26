# go-fleet-charging

车队车辆跨时段充电容量预订服务。充电站按离散时段配置可用功率，车辆提交
到达/离开时间、最低所需电量与每时段最大功率，服务在容量约束下生成逐时段
的充电计划。

开发环境：Go 1.23.0。

运行测试：

    go test ./... -race

## 核心概念

- **时段（Slot）**：站点配置的离散时间区间，左闭右开 `[Start, End)`，各有
  独立容量。时段互不重叠。
- **请求（Request）**：`ExternalID`（幂等键）、`VehicleID`、`Arrival`、
  `Departure`、`MinEnergy`、`MaxPower`。请求区间可跨多个相邻时段。
- **计划（Plan）**：请求被接受后生成的逐时段功率分配。每个时段的总分配
  不超过站点容量，且计划总能量保证不低于 `MinEnergy`。
- **单位**：功率为整数毫瓦（`Milliwatts`），能量为整数毫焦
  （`Millijoules`，毫瓦·秒）。所有时间必须整秒对齐，能量 = 功率 × 整秒数，
  全程整数运算，无浮点误差。

## API 概览

```go
svc, err := fleetcharging.NewService(fleetcharging.NewFileStore("data/state.json"))
err  = svc.ConfigureSlots([]fleetcharging.Slot{...})
plan, err := svc.Submit(fleetcharging.Request{...})   // 幂等
plan, err  = svc.Modify(plan.ID, fleetcharging.UpdateRequest{...})
plan, err  = svc.Cancel(plan.ID)
occ, err  := svc.Occupancy(from, to)                  // 逐时段占用查询
plan, err  = svc.GetPlan(planID)
```

## 行为保证

1. **整体接受或整体拒绝**：任一约束（容量、最低电量、参数、时间）不满足
   时计划被整体拒绝，不留下任何部分占用。
2. **幂等**：相同 `ExternalID` + 相同内容返回原计划；相同 `ExternalID` +
   不同内容返回幂等冲突错误。
3. **并发安全**：所有变更操作持有同一把互斥锁，提交可串行化。多车并发
   争用时，只有仍满足全部容量约束的计划组合能提交成功。
4. **修改原子替换**：`Modify` 在计算新分配时排除本计划的旧占用，成功后
   一次性替换；失败时原计划保持不变。修改与取消只允许在车辆到达前进行。
5. **时间竞态安全**：修改/取消与时钟越过到达点的竞争由同一把锁消解——
   要么操作先完成，要么先越过到达点而操作被拒绝；容量不泄漏、不重复释放。
6. **持久化**：每次状态变更先写存储再生效（失败自动回滚内存状态）。
   `FileStore` 采用临时文件 + rename 原子落盘；重启后计划、占用与幂等表
   完整恢复。

## 错误分类

所有业务错误为 `*fleetcharging.Error`，用 `IsKind(err, kind)` 区分：

| Kind | 含义 |
|---|---|
| `KindInvalidParam` | 参数不合法（空字段、区间倒置、未对齐整秒、超出已配置时段等） |
| `KindCapacity` | 容量不足，无法在约束内凑足最低电量 |
| `KindTime` | 时间约束不满足（到达点已过、到达不在未来） |
| `KindState` | 状态不允许（计划已取消、存在生效计划时重配站点） |
| `KindIdempotency` | 外部请求号冲突：同号不同内容 |

## 测试覆盖

`service_test.go` 覆盖：时段配置校验、能量精确核算、左闭右开区间语义、
容量超限整体拒绝、跨时段部分占用回滚、参数/时间/状态/幂等错误分类、
并发提交的可行组合判定、修改原子替换、到达后修改/取消拒绝、取消只释放
一次、取消与时钟越过到达点的并发竞态（50 轮）、并发修改与取消、内存与
文件两种存储的持久化恢复。

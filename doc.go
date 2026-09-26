// Package fleetcharging 实现车队车辆跨离散时段的充电容量预订服务：
// 站点时段功率配置、计划提交/修改/取消、逐时段占用查询、
// 精确整数能量核算、外部请求号幂等与持久化。
//
// 概览见项目 README；入口类型为 Service，通过 NewService 构造。
package fleetcharging

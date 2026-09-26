package fleetcharging

import (
	"errors"
	"fmt"
)

// ErrorKind 是错误类别，用于把失败原因区分成：
// 参数、容量、时间、状态、幂等 五类。
type ErrorKind string

const (
	// KindParameter 请求或配置本身不合法（缺字段、负值、未对齐、站点未知等）。
	KindParameter ErrorKind = "parameter"
	// KindCapacity 容量不足：当前已占用功率无法再满足本次计划所需能量。
	KindCapacity ErrorKind = "capacity"
	// KindTime 时间语义不合法：到达不早于离开、到达时间已过、超出配置时段等。
	KindTime ErrorKind = "time"
	// KindState 状态冲突：计划不存在、已取消、车辆已抵达后再改/取消等。
	KindState ErrorKind = "state"
	// KindIdempotency 幂等冲突：同一外部请求号对应了不同的请求内容。
	KindIdempotency ErrorKind = "idempotency"
)

// Error 是服务对外返回的统一错误类型，用 Kind 区分类别，Op 记录发生位置。
type Error struct {
	Kind ErrorKind
	Op   string
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	msg := string(e.Kind)
	if e.Op != "" {
		msg += " at " + e.Op
	}
	if e.Msg != "" {
		msg += ": " + e.Msg
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// Is 让 errors.Is 可以按类别匹配哨兵错误。
// 例如 errors.Is(err, fleetcharging.ErrCapacity)。
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	return t.Kind == e.Kind
}

// 各类别的哨兵错误，仅承载 Kind，具体信息看返回的 Error.Msg。
var (
	ErrParameter  = &Error{Kind: KindParameter}
	ErrCapacity   = &Error{Kind: KindCapacity}
	ErrTime       = &Error{Kind: KindTime}
	ErrState      = &Error{Kind: KindState}
	ErrIdempotent = &Error{Kind: KindIdempotency}
)

// KindOf 返回错误类别；非本服务错误返回空字符串。
func KindOf(err error) ErrorKind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}

func fail(kind ErrorKind, op, msg string, args ...any) error {
	return &Error{Kind: kind, Op: op, Msg: fmt.Sprintf(msg, args...)}
}

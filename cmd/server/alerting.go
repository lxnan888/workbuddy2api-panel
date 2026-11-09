package main

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/alert"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
)

// alertSource 把账号池健康与 WAF 状态适配成 alert.Source。
type alertSource struct {
	pool *pool.Pool
	h    *server.Handler
}

func (s alertSource) Health(realm string) alert.Health {
	h := s.pool.RealmHealth(realm)
	return alert.Health{
		Total: h.Total, Healthy: h.Healthy, Cooling: h.Cooling, Disabled: h.Disabled,
		Breaker: h.Breaker, Degraded: h.Degraded, InFlight: h.Concurrency,
	}
}

func (s alertSource) WAFActive() bool { return s.h.WAFActive() }

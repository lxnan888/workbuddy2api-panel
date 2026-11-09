package panel

import (
	"context"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// startTaskJob 手动为账号启动一个后台全量任务作业（202 + 轮询 job 状态）。
func (p *Panel) startTaskJob(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	job, started, err := p.StartAccountTaskJob(uid, "manual")
	if err != nil {
		status := http.StatusConflict
		switch err {
		case errTaskJobAccount:
			status = http.StatusNotFound
		case errTaskJobsNotStarted, errTaskJobsStopped:
			status = http.StatusServiceUnavailable
		}
		writeErr(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "started": started, "job": job})
}

const (
	globalTaskWriteMessage = "国际版账号不支持个人成长任务"
	noPersonalTaskMessage  = "企业版 / API Key 账号不支持个人成长任务"
)

// autoTaskRunEvent 后台任务进度事件（taskjobs 持久化作业消费）。
type autoTaskRunEvent struct {
	Kind      string // phase, task_start, result
	TaskCode  string
	Result    map[string]any
	Completed bool
}

// runAutoAllObserved 执行单账号全量自动任务并逐项上报结果。
// 当前执行器不支持阶段级取消；ctx 在开始前检查一次，单项结果逐条上报。
func (p *Panel) runAutoAllObserved(ctx context.Context, a *auth.Auth, observe func(autoTaskRunEvent)) []map[string]any {
	if ctx != nil && ctx.Err() != nil {
		return nil
	}
	results := p.runAutoAll(a)
	if observe != nil {
		for _, r := range results {
			code, _ := r["task_code"].(string)
			observe(autoTaskRunEvent{Kind: "result", TaskCode: code, Result: r, Completed: true})
		}
	}
	return results
}

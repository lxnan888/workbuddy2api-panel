// bodylimit.go 入站请求体的安全上限（server.max_payload_bytes）。
//
// 定位：这不是「大请求预拦截」，而是防「大到能把进程读死」的上传。chat 路径此前
// 无界 io.ReadAll，带 key 的客户端或跑飞的脚本一次几百 MB 的 body 就能在内存有限
// 的机器上把网关连同面板一起 OOM。上限刻意远高于真实会话（长会话多图 base64 实测
// 在几 MB 量级），只当护栏，不当内容策略。
//
// 超限时的两个动作缺一不可：先排空有限字节、再回 413。直接 return 会让 net/http
// 关掉连接，客户端往往只看到 connection reset，看不到那份 413。
package server

import (
	"io"
	"log"
	"net/http"
	"strconv"
	"time"
)

// defaultMaxPayloadBytes 缺省上限（64 MiB）。允许范围由配置层校验
// （server.max_payload_bytes），这里只兜「没配」。
const defaultMaxPayloadBytes int64 = 64 << 20

// slowBodyWarn 读体超过该时长就打一条 WARN：正常 body 是瞬间到的，读体慢通常
// 说明卡在客户端或中间层的上传/缓冲。阈值取 3s：环回/LAN 上正常请求是毫秒级。
const slowBodyWarn = 3 * time.Second

// bodyReadError 读体失败。TooLarge 时调用方按各自协议的形状回 413，其余按既有的
// 400 invalid_request 处理。
type bodyReadError struct {
	TooLarge bool
	Limit    int64
	Err      error
}

func (e *bodyReadError) Error() string {
	if e.TooLarge {
		return "request body exceeds " + strconv.FormatInt(e.Limit, 10) + " bytes (server.max_payload_bytes)"
	}
	if e.Err == nil {
		return "read body"
	}
	return "read body: " + e.Err.Error()
}

// readBody 读入请求体并施加安全上限：最多读 limit+1 字节，多出来的那一个字节就是
// 「超限」的证据。声明了 Content-Length 的超大上传与块上传在这里是同一条路径——
// 不去看头，因为头可以撒谎，读进去的字节不会。
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, *bodyReadError) {
	limit := h.maxPayload()
	start := time.Now()
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if wait := time.Since(start); wait > slowBodyWarn {
		// 只记元信息：路径、读了多长、多慢、声明了多少。不含 body 内容。
		log.Printf("WARN: [server] slow request body: %s %s read %d bytes in %s (Content-Length=%d)",
			r.Method, r.URL.Path, len(body), wait.Round(time.Millisecond), r.ContentLength)
	}
	if int64(len(body)) > limit {
		drainBody(w, r, limit)
		return nil, &bodyReadError{TooLarge: true, Limit: limit}
	}
	if err != nil {
		return nil, &bodyReadError{Err: err}
	}
	return body, nil
}

// drainBody 排空剩余上传（最多 limit 字节或 2s 到点即停）。限时是为了不让一个慢速
// 客户端把连接和 goroutine 拖住；结束后复位读 deadline，避免影响 keep-alive 上的下个请求。
func drainBody(w http.ResponseWriter, r *http.Request, limit int64) {
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return
	}
	defer rc.SetReadDeadline(time.Time{})
	_, _ = io.CopyN(io.Discard, r.Body, limit)
}

// writeBodyError 按协议的两种错误形状回包（OpenAI / Anthropic）。
func (h *Handler) writeBodyError(w http.ResponseWriter, berr *bodyReadError, anthropic bool) {
	status := http.StatusBadRequest
	code := "invalid_request"
	if berr.TooLarge {
		status = http.StatusRequestEntityTooLarge
		code = "payload_too_large"
	}
	if anthropic {
		writeMessagesError(w, status, "invalid_request_error", berr.Error())
		return
	}
	writeOpenAIError(w, status, code, berr.Error())
}

// maxPayload 返回当前生效的入站 body 上限（热改优先；配置 <=0 时回退默认 64 MiB）。
func (h *Handler) maxPayload() int64 {
	if n := h.loadLive().MaxPayloadBytes; n > 0 {
		return n
	}
	if h.cfg.MaxPayloadBytes > 0 {
		return h.cfg.MaxPayloadBytes
	}
	return defaultMaxPayloadBytes
}

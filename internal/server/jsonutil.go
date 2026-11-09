package server

import "encoding/json"

// asString 取 object 的字符串字段（非 string 返回空串）。
func asString(v any) string {
	s, _ := v.(string)
	return s
}

// numberToInt 把 JSON 数字转 int（float64/int/int64/json.Number 四种来源）。
func numberToInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

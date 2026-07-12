package webhook

import "encoding/json"

func Decode(body []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func Field[T any](payload map[string]any, key string) (T, bool) {
	v, ok := payload[key].(T)
	return v, ok
}

package handler

import "strconv"

func formatCount(n int64) string {
	raw := strconv.FormatInt(n, 10)
	if len(raw) <= 3 {
		return raw
	}

	commas := (len(raw) - 1) / 3
	out := make([]byte, 0, len(raw)+commas)
	for i, digit := range []byte(raw) {
		if i > 0 && (len(raw)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, digit)
	}
	return string(out)
}

package redact

import (
	"fmt"

	"github.com/sirupsen/logrus"
)

type logHook struct{ r *Redactor }

// Hook redacts before formatting, so logger output changes or split writes
// cannot bypass filtering. Numeric/bool structured fields remain typed.
func (r *Redactor) Hook() logrus.Hook     { return &logHook{r: r} }
func (h *logHook) Levels() []logrus.Level { return logrus.AllLevels }
func (h *logHook) Fire(entry *logrus.Entry) error {
	entry.Message = h.r.Text(entry.Message)
	for key, value := range entry.Data {
		clean := value
		switch v := value.(type) {
		case string:
			clean = h.r.Text(v)
		case error:
			clean = h.r.Text(v.Error())
		case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		default:
			text := fmt.Sprint(value)
			if filtered := h.r.Text(text); filtered != text {
				clean = filtered
			}
		}
		cleanKey := h.r.Text(key)
		if cleanKey != key {
			delete(entry.Data, key)
		}
		entry.Data[cleanKey] = clean
	}
	return nil
}

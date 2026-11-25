package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Spec struct {
	every  time.Duration
	minute []bool
	hour   []bool
	dom    []bool
	month  []bool
	dow    []bool
}

func Parse(expr string) (Spec, error) {
	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return Spec{}, fmt.Errorf("empty cron expression")
	}
	if strings.HasPrefix(trimmed, "@every ") {
		durStr := strings.TrimSpace(strings.TrimPrefix(trimmed, "@every "))
		dur, err := time.ParseDuration(durStr)
		if err != nil {
			return Spec{}, fmt.Errorf("invalid @every duration %q: %w", durStr, err)
		}
		if dur <= 0 {
			return Spec{}, fmt.Errorf("@every duration must be positive: %s", dur)
		}
		return Spec{every: dur}, nil
	}

	fields := strings.Fields(trimmed)
	if len(fields) != 5 {
		return Spec{}, fmt.Errorf("expected 5 cron fields or @every <duration>, got %d", len(fields))
	}

	minute, err := parseField(fields[0], 0, 59, false)
	if err != nil {
		return Spec{}, fmt.Errorf("minute field: %w", err)
	}
	hour, err := parseField(fields[1], 0, 23, false)
	if err != nil {
		return Spec{}, fmt.Errorf("hour field: %w", err)
	}
	dom, err := parseField(fields[2], 1, 31, false)
	if err != nil {
		return Spec{}, fmt.Errorf("day-of-month field: %w", err)
	}
	month, err := parseField(fields[3], 1, 12, false)
	if err != nil {
		return Spec{}, fmt.Errorf("month field: %w", err)
	}
	dow, err := parseField(fields[4], 0, 7, true)
	if err != nil {
		return Spec{}, fmt.Errorf("day-of-week field: %w", err)
	}

	return Spec{
		minute: minute,
		hour:   hour,
		dom:    dom,
		month:  month,
		dow:    dow,
	}, nil
}

func (s Spec) Due(now, lastRun time.Time) bool {
	if s.every > 0 {
		if lastRun.IsZero() {
			return true
		}
		return now.Sub(lastRun) >= s.every
	}
	if !s.matches(now) {
		return false
	}
	if lastRun.IsZero() {
		return true
	}
	return lastRun.Truncate(time.Minute).Before(now.Truncate(time.Minute))
}

func (s Spec) matches(t time.Time) bool {
	if len(s.minute) == 0 {
		return false
	}
	return s.minute[t.Minute()] &&
		s.hour[t.Hour()] &&
		s.dom[t.Day()] &&
		s.month[int(t.Month())] &&
		s.dow[int(t.Weekday())]
}

func parseField(field string, minVal, maxVal int, wrapSunday bool) ([]bool, error) {
	size := maxVal + 1
	if wrapSunday {
		size = 7
	}
	bits := make([]bool, size)

	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty list item in %q", field)
		}
		step := 1
		rangePart := part
		if idx := strings.IndexByte(part, '/'); idx >= 0 {
			rangePart = part[:idx]
			stepVal, err := strconv.Atoi(part[idx+1:])
			if err != nil || stepVal <= 0 {
				return nil, fmt.Errorf("invalid step in %q", part)
			}
			step = stepVal
		}

		start, end := minVal, maxVal
		if rangePart != "*" {
			if idx := strings.IndexByte(rangePart, '-'); idx >= 0 {
				sVal, err := strconv.Atoi(rangePart[:idx])
				if err != nil {
					return nil, fmt.Errorf("invalid range start in %q", part)
				}
				eVal, err := strconv.Atoi(rangePart[idx+1:])
				if err != nil {
					return nil, fmt.Errorf("invalid range end in %q", part)
				}
				start, end = sVal, eVal
			} else {
				val, err := strconv.Atoi(rangePart)
				if err != nil {
					return nil, fmt.Errorf("invalid value in %q", part)
				}
				start = val
				if strings.ContainsRune(part, '/') {
					end = maxVal
				} else {
					end = val
				}
			}
		}

		if start < minVal || end > maxVal || start > end {
			return nil, fmt.Errorf("value out of bounds [%d,%d] in %q", minVal, maxVal, part)
		}
		for v := start; v <= end; v += step {
			idx := v
			if wrapSunday && v == 7 {
				idx = 0
			}
			bits[idx] = true
		}
	}
	return bits, nil
}

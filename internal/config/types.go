package config

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	v, err := ParseDuration(node.Value)
	if err != nil {
		return &Error{Line: node.Line, Message: fmt.Sprintf("invalid duration %q", node.Value), Hint: "durations look like 500ms, 30s, 20m, 6h, 72h or 7d; false means off"}
	}
	*d = Duration(v)
	return nil
}

// ParseDuration reads a Go duration, plus the day unit `dboss cron` already accepts (7d) and
// `false` for a key whose 0 means off, so a switch reads like one.
func ParseDuration(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	switch value {
	case "false", "off":
		return 0, nil
	}
	if days, found := strings.CutSuffix(value, "d"); found {
		count, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", value)
		}
		return time.Duration(count) * 24 * time.Hour, nil
	}
	return time.ParseDuration(value)
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		var nanos int64
		if err := json.Unmarshal(data, &nanos); err != nil {
			return err
		}
		*d = Duration(nanos)
		return nil
	}
	value, err := ParseDuration(text)
	if err != nil {
		return fmt.Errorf("invalid duration %q", text)
	}
	*d = Duration(value)
	return nil
}

func (d Duration) MarshalYAML() (any, error)    { return time.Duration(d).String(), nil }
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }
func (d Duration) Value() time.Duration         { return time.Duration(d) }

type Size int64

func (s *Size) UnmarshalYAML(node *yaml.Node) error {
	v, err := ParseSize(node.Value)
	if err != nil {
		return &Error{Line: node.Line, Message: fmt.Sprintf("invalid size %q", node.Value), Hint: "sizes look like 1024, 512k, 10m or 1g"}
	}
	*s = Size(v)
	return nil
}

func ParseSize(value string) (int64, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "0" {
		return 0, nil
	}
	multiplier := int64(1)
	if len(value) > 0 {
		switch value[len(value)-1] {
		case 'k':
			multiplier = 1 << 10
		case 'm':
			multiplier = 1 << 20
		case 'g':
			multiplier = 1 << 30
		default:
			if value[len(value)-1] < '0' || value[len(value)-1] > '9' {
				return 0, fmt.Errorf("invalid size %q (use bytes, k, m, or g)", value)
			}
			value += "b"
		}
	}
	if value == "" {
		return 0, fmt.Errorf("invalid size %q (use bytes, k, m, or g)", value)
	}
	n, err := strconv.ParseInt(value[:len(value)-1], 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", value)
	}
	return n * multiplier, nil
}

func (s Size) MarshalYAML() (any, error) { return s.String(), nil }
func (s Size) String() string {
	value := int64(s)
	if value == 0 {
		return "0"
	}
	for _, unit := range []struct {
		suffix string
		bytes  int64
	}{{"g", 1 << 30}, {"m", 1 << 20}, {"k", 1 << 10}} {
		if value%unit.bytes == 0 {
			return fmt.Sprintf("%d%s", value/unit.bytes, unit.suffix)
		}
	}
	return strconv.FormatInt(value, 10)
}

// List is a []string that also accepts a single scalar in YAML, so `allow_ips: 10.0.0.0/8` and
// `allow_ips: [10.0.0.0/8]` mean the same thing. It marshals as a sequence.
type List []string

func (l *List) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag == "!!null" {
			*l = nil
			return nil
		}
		*l = List{node.Value}
		return nil
	case yaml.SequenceNode:
		var items []string
		if err := node.Decode(&items); err != nil {
			return err
		}
		*l = List(items)
		return nil
	}
	return &Error{Line: node.Line, Message: "must be a value or a list of values"}
}

// Autostart is when an app comes up on its own: true with the host and on resume, false only on
// an explicit run or console start, button only after a POST to its wake page. Any request wakes
// a false app, but only a POST wakes a button app, so crawlers and favicon probes cannot start it.
type Autostart string

const (
	AutostartOn     Autostart = "true"
	AutostartOff    Autostart = "false"
	AutostartButton Autostart = "button"
)

// Starts reports whether the host should bring the app up on boot and on resume.
func (a Autostart) Starts() bool { return a == AutostartOn }

// UnmarshalYAML accepts the booleans true and false and the string "button".
func (a *Autostart) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag == "!!bool" {
		*a = AutostartOff
		if node.Value == "true" {
			*a = AutostartOn
		}
		return nil
	}
	switch value := Autostart(node.Value); value {
	case AutostartOn, AutostartOff, AutostartButton:
		*a = value
		return nil
	}
	return &Error{Line: node.Line, Message: "must be true, false or button"}
}

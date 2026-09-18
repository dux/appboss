package config

import (
	"fmt"
	"reflect"
)

// Overrides is every defaults key as a pointer, so an absent key is distinguishable from its
// zero value. It is the top level of an app file; host defaults are merged onto it with apply.
type Overrides struct {
	ProcessOverrides `yaml:",inline"`
	WebOverrides     `yaml:",inline"`
}

// ProcessOverrides is the subset allowed under processes.<name>. Decoding is strict, so a Web
// key there is rejected as unknown.
type ProcessOverrides struct {
	IdleStop       *Duration         `yaml:"idle_stop,omitempty" json:"idle_stop,omitempty"`
	Health         *string           `yaml:"health,omitempty" json:"health,omitempty"`
	HealthInterval *Duration         `yaml:"health_interval,omitempty" json:"health_interval,omitempty"`
	HealthTimeout  *Duration         `yaml:"health_timeout,omitempty" json:"health_timeout,omitempty"`
	StopTimeout    *Duration         `yaml:"stop_timeout,omitempty" json:"stop_timeout,omitempty"`
	StopSignal     *string           `yaml:"stop_signal,omitempty" json:"stop_signal,omitempty"`
	Restart        *string           `yaml:"restart,omitempty" json:"restart,omitempty"`
	MaxRestarts    *int              `yaml:"max_restarts,omitempty" json:"max_restarts,omitempty"`
	RestartReset   *Duration         `yaml:"restart_reset,omitempty" json:"restart_reset,omitempty"`
	RestartBackoff []any             `yaml:"restart_backoff,omitempty" json:"restart_backoff,omitempty"`
	LogMaxSize     *Size             `yaml:"log_max_size,omitempty" json:"log_max_size,omitempty"`
	LogKeep        *int              `yaml:"log_keep,omitempty" json:"log_keep,omitempty"`
	LogTailLines   *int              `yaml:"log_tail_lines,omitempty" json:"log_tail_lines,omitempty"`
	LogRetention   *Duration         `yaml:"log_retention,omitempty" json:"log_retention,omitempty"`
	LogFlush       *Duration         `yaml:"log_flush,omitempty" json:"log_flush,omitempty"`
	Shell          *bool             `yaml:"shell,omitempty" json:"shell,omitempty"`
	Env            map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Resources      *string           `yaml:"resources,omitempty" json:"resources,omitempty"`
	MemoryMax      *Size             `yaml:"memory_max,omitempty" json:"memory_max,omitempty"`
	CPUMax         *int              `yaml:"cpu_max,omitempty" json:"cpu_max,omitempty"`
}

type WebOverrides struct {
	Static          *string           `yaml:"static,omitempty" json:"static,omitempty"`
	StaticImmutable List              `yaml:"static_immutable,omitempty" json:"static_immutable,omitempty"`
	MaxBody         *Size             `yaml:"max_body,omitempty" json:"max_body,omitempty"`
	BasicAuth       map[string]string `yaml:"basic_auth,omitempty" json:"-"`
	AllowIPs        List              `yaml:"allow_ips,omitempty" json:"allow_ips,omitempty"`
	Headers         map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	MaintenancePage *string           `yaml:"maintenance_page,omitempty" json:"maintenance_page,omitempty"`
}

// apply copies every non-nil field of overrides onto the field of the same name in target.
// Pointers are dereferenced, slices replace, maps merge key by key into a fresh map so the
// shared defaults are never mutated. Used for host defaults -> app and app -> process.
func apply(target any, overrides any) {
	applyValue(reflect.ValueOf(target).Elem(), reflect.ValueOf(overrides))
}

func applyValue(target, overrides reflect.Value) {
	for i := 0; i < overrides.NumField(); i++ {
		field := overrides.Type().Field(i)
		value := overrides.Field(i)
		if field.Anonymous {
			applyValue(target, value)
			continue
		}
		if value.IsNil() {
			continue
		}
		dest := target.FieldByName(field.Name)
		if !dest.IsValid() {
			panic(fmt.Sprintf("config: %s has no field %s", target.Type(), field.Name))
		}
		switch value.Kind() {
		case reflect.Pointer:
			dest.Set(value.Elem())
		case reflect.Map:
			merged := reflect.MakeMapWithSize(dest.Type(), dest.Len()+value.Len())
			for iter := dest.MapRange(); iter.Next(); {
				merged.SetMapIndex(iter.Key(), iter.Value())
			}
			for iter := value.MapRange(); iter.Next(); {
				merged.SetMapIndex(iter.Key(), iter.Value())
			}
			dest.Set(merged)
		default:
			dest.Set(value)
		}
	}
}

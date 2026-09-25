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
	IdleStop           *Duration         `yaml:"idle_stop,omitempty" json:"idle_stop,omitempty"`
	Health             *string           `yaml:"-" json:"-"`
	LivenessInterval   *Duration         `yaml:"liveness_interval,omitempty" json:"liveness_interval,omitempty"`
	HealthTimeout      *Duration         `yaml:"health_timeout,omitempty" json:"health_timeout,omitempty"`
	UnhealthyThreshold *int              `yaml:"unhealthy_threshold,omitempty" json:"unhealthy_threshold,omitempty"`
	StopTimeout        *Duration         `yaml:"stop_timeout,omitempty" json:"stop_timeout,omitempty"`
	StopSignal         *string           `yaml:"stop_signal,omitempty" json:"stop_signal,omitempty"`
	Restart            *string           `yaml:"restart,omitempty" json:"restart,omitempty"`
	MaxRestarts        *int              `yaml:"max_restarts,omitempty" json:"max_restarts,omitempty"`
	LogRetention       *Duration         `yaml:"log_retention,omitempty" json:"log_retention,omitempty"`
	StdoutRetention    *Duration         `yaml:"stdout_retention,omitempty" json:"stdout_retention,omitempty"`
	TmpClean           *Duration         `yaml:"tmp_clean,omitempty" json:"tmp_clean,omitempty"`
	Env                map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	MemoryMax          *Size             `yaml:"memory_max,omitempty" json:"memory_max,omitempty"`
	CPUMax             *int              `yaml:"cpu_max,omitempty" json:"cpu_max,omitempty"`
}

type WebOverrides struct {
	HealthEndpoint   *string           `yaml:"health_endpoint,omitempty" json:"health_endpoint,omitempty"`
	StaticImmutable  List              `yaml:"static_immutable,omitempty" json:"static_immutable,omitempty"`
	StaticExtensions List              `yaml:"static_extensions,omitempty" json:"static_extensions,omitempty"`
	MaxBody          *Size             `yaml:"max_body,omitempty" json:"max_body,omitempty"`
	BasicAuth        map[string]string `yaml:"basic_auth,omitempty" json:"-"`
	AllowIPs         List              `yaml:"allow_ips,omitempty" json:"allow_ips,omitempty"`
	Deny             List              `yaml:"deny,omitempty" json:"deny,omitempty"`
	Headers          map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	Alerts           *AlertsOverrides  `yaml:"alerts,omitempty" json:"alerts,omitempty"`
	Events           *EventsOverrides  `yaml:"events,omitempty" json:"events,omitempty"`
	Auth             List              `yaml:"auth,omitempty" json:"auth,omitempty"`
	SessionTTL       *Duration         `yaml:"session_ttl,omitempty" json:"session_ttl,omitempty"`
	AuthCog          *AuthCogPath      `yaml:"authcog,omitempty" json:"authcog,omitempty"`
}

// PubsubOverrides is the web process's pubsub mapping as pointers, so an app can set one key and
// keep the default for the rest. Secret never leaves the process as JSON, like basic_auth.
type PubsubOverrides struct {
	Path           *string `yaml:"path,omitempty" json:"path,omitempty"`
	Secret         *string `yaml:"secret,omitempty" json:"-"`
	Replay         *int    `yaml:"replay,omitempty" json:"replay,omitempty"`
	MaxClients     *int    `yaml:"max_clients,omitempty" json:"max_clients,omitempty"`
	MaxMessageSize *Size   `yaml:"max_message_size,omitempty" json:"max_message_size,omitempty"`
	ClientEvents   *bool   `yaml:"client_events,omitempty" json:"client_events,omitempty"`
	Test           *bool   `yaml:"test,omitempty" json:"test,omitempty"`
}

// AlertsOverrides is the alerts block as pointers, merged key by key like pubsub.
type AlertsOverrides struct {
	ErrorRate *int      `yaml:"error_rate,omitempty" json:"error_rate,omitempty"`
	SlowP95   *Duration `yaml:"slow_p95,omitempty" json:"slow_p95,omitempty"`
}

// EventsOverrides is the events block as pointers; views and funnels merge by name.
type EventsOverrides struct {
	Retention *Duration              `yaml:"retention,omitempty" json:"retention,omitempty"`
	Views     map[string]string      `yaml:"views,omitempty" json:"views,omitempty"`
	Funnels   map[string]EventFunnel `yaml:"funnels,omitempty" json:"funnels,omitempty"`
}

// apply copies every non-nil field of overrides onto the field of the same name in target.
// Pointers are dereferenced, a pointer to a nested overrides block merges into its struct key by
// key (so an absent key keeps the default), slices replace, and maps merge key by key into a
// fresh map so the shared defaults are never mutated. Used for host defaults -> app and
// app -> process.
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
			if value.Elem().Kind() == reflect.Struct && dest.Kind() == reflect.Struct {
				applyValue(dest, value.Elem())
				continue
			}
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

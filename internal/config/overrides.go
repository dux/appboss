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
	Health             *string           `yaml:"health,omitempty" json:"health,omitempty"`
	HealthInterval     *Duration         `yaml:"health_interval,omitempty" json:"health_interval,omitempty"`
	HealthTimeout      *Duration         `yaml:"health_timeout,omitempty" json:"health_timeout,omitempty"`
	UnhealthyThreshold *int              `yaml:"unhealthy_threshold,omitempty" json:"unhealthy_threshold,omitempty"`
	StopTimeout        *Duration         `yaml:"stop_timeout,omitempty" json:"stop_timeout,omitempty"`
	StopSignal         *string           `yaml:"stop_signal,omitempty" json:"stop_signal,omitempty"`
	Restart            *string           `yaml:"restart,omitempty" json:"restart,omitempty"`
	MaxRestarts        *int              `yaml:"max_restarts,omitempty" json:"max_restarts,omitempty"`
	RestartReset       *Duration         `yaml:"restart_reset,omitempty" json:"restart_reset,omitempty"`
	RestartBackoff     []any             `yaml:"restart_backoff,omitempty" json:"restart_backoff,omitempty"`
	LogMaxSize         *Size             `yaml:"log_max_size,omitempty" json:"log_max_size,omitempty"`
	LogKeep            *int              `yaml:"log_keep,omitempty" json:"log_keep,omitempty"`
	LogTailLines       *int              `yaml:"log_tail_lines,omitempty" json:"log_tail_lines,omitempty"`
	LogRetention       *Duration         `yaml:"log_retention,omitempty" json:"log_retention,omitempty"`
	StdoutRetention    *Duration         `yaml:"stdout_retention,omitempty" json:"stdout_retention,omitempty"`
	LogFlush           *Duration         `yaml:"log_flush,omitempty" json:"log_flush,omitempty"`
	Shell              *bool             `yaml:"shell,omitempty" json:"shell,omitempty"`
	Env                map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Resources          *string           `yaml:"resources,omitempty" json:"resources,omitempty"`
	MemoryMax          *Size             `yaml:"memory_max,omitempty" json:"memory_max,omitempty"`
	CPUMax             *int              `yaml:"cpu_max,omitempty" json:"cpu_max,omitempty"`
}

type WebOverrides struct {
	HealthEndpoint   *string           `yaml:"health_endpoint,omitempty" json:"health_endpoint,omitempty"`
	Static           *string           `yaml:"static,omitempty" json:"static,omitempty"`
	StaticImmutable  List              `yaml:"static_immutable,omitempty" json:"static_immutable,omitempty"`
	StaticExtensions List              `yaml:"static_extensions,omitempty" json:"static_extensions,omitempty"`
	MaxBody          *Size             `yaml:"max_body,omitempty" json:"max_body,omitempty"`
	BasicAuth        map[string]string `yaml:"basic_auth,omitempty" json:"-"`
	AllowIPs         List              `yaml:"allow_ips,omitempty" json:"allow_ips,omitempty"`
	Headers          map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	MaintenancePage  *string           `yaml:"maintenance_page,omitempty" json:"maintenance_page,omitempty"`
	ErrorPagePath    *string           `yaml:"error_page_path,omitempty" json:"error_page_path,omitempty"`
	Pubsub           *PubsubOverrides  `yaml:"pubsub,omitempty" json:"pubsub,omitempty"`
	Alerts           *AlertsOverrides  `yaml:"alerts,omitempty" json:"alerts,omitempty"`
	Auth             *AuthOverrides    `yaml:"auth,omitempty" json:"auth,omitempty"`
}

// PubsubOverrides is the pubsub block as pointers, so an app can override one key and keep the
// host defaults for the rest.
type PubsubOverrides struct {
	Path           *string `yaml:"path,omitempty" json:"path,omitempty"`
	Secret         *string `yaml:"secret,omitempty" json:"secret,omitempty"`
	Replay         *int    `yaml:"replay,omitempty" json:"replay,omitempty"`
	MaxClients     *int    `yaml:"max_clients,omitempty" json:"max_clients,omitempty"`
	MaxMessageSize *Size   `yaml:"max_message_size,omitempty" json:"max_message_size,omitempty"`
	ClientEvents   *bool   `yaml:"client_events,omitempty" json:"client_events,omitempty"`
	Test           *bool   `yaml:"test,omitempty" json:"test,omitempty"`
}

func (o *PubsubOverrides) applyOverride(target reflect.Value) {
	pubsub, ok := target.Addr().Interface().(*Pubsub)
	if !ok {
		return
	}
	if o.Path != nil {
		pubsub.Path = *o.Path
	}
	if o.Secret != nil {
		pubsub.Secret = *o.Secret
	}
	if o.Replay != nil {
		pubsub.Replay = *o.Replay
	}
	if o.MaxClients != nil {
		pubsub.MaxClients = *o.MaxClients
	}
	if o.MaxMessageSize != nil {
		pubsub.MaxMessageSize = *o.MaxMessageSize
	}
	if o.ClientEvents != nil {
		pubsub.ClientEvents = *o.ClientEvents
	}
	if o.Test != nil {
		pubsub.Test = *o.Test
	}
}

// AlertsOverrides is the alerts block as pointers, merged key by key like pubsub.
type AlertsOverrides struct {
	Window      *Duration `yaml:"window,omitempty" json:"window,omitempty"`
	MinRequests *int      `yaml:"min_requests,omitempty" json:"min_requests,omitempty"`
	ErrorRate   *int      `yaml:"error_rate,omitempty" json:"error_rate,omitempty"`
	SlowP95     *Duration `yaml:"slow_p95,omitempty" json:"slow_p95,omitempty"`
}

func (o *AlertsOverrides) applyOverride(target reflect.Value) {
	alerts, ok := target.Addr().Interface().(*Alerts)
	if !ok {
		return
	}
	if o.Window != nil {
		alerts.Window = *o.Window
	}
	if o.MinRequests != nil {
		alerts.MinRequests = *o.MinRequests
	}
	if o.ErrorRate != nil {
		alerts.ErrorRate = *o.ErrorRate
	}
	if o.SlowP95 != nil {
		alerts.SlowP95 = *o.SlowP95
	}
}

// AuthOverrides is the auth block as pointers. allow_emails replaces the host list, like every
// other list.
type AuthOverrides struct {
	AllowEmails List      `yaml:"allow_emails,omitempty" json:"allow_emails,omitempty"`
	SessionTTL  *Duration `yaml:"session_ttl,omitempty" json:"session_ttl,omitempty"`
}

func (o *AuthOverrides) applyOverride(target reflect.Value) {
	auth, ok := target.Addr().Interface().(*Auth)
	if !ok {
		return
	}
	if o.AllowEmails != nil {
		auth.AllowEmails = o.AllowEmails
	}
	if o.SessionTTL != nil {
		auth.SessionTTL = *o.SessionTTL
	}
}

// overrideApplier is a nested shared block that merges itself field by field, so an absent key
// keeps the value from defaults:.
type overrideApplier interface {
	applyOverride(target reflect.Value)
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
		if applier, ok := value.Interface().(overrideApplier); ok {
			applier.applyOverride(dest)
			continue
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

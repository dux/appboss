package ops

import (
	"errors"
	"fmt"
	"strings"

	"dboss/internal/apps"
	"dboss/internal/notify"
)

// ErrRescan marks a config save whose file landed but whose rescan failed.
var ErrRescan = errors.New("saved, but rescan failed")

// ConfigResult is what a config save changed: the file as written, apps the rescan could not
// load and host keys that only apply on the next start.
type ConfigResult struct {
	File            apps.ConfigFile `json:"file"`
	Invalid         []string        `json:"invalid"`
	RestartRequired []string        `json:"restart_required"`
}

// SaveConfig is every console config save: write the file, rescan so the change applies, audit
// it as action and tell the operator when a host key now waits for a restart. write returns the
// file it touched even on failure, so a conflict can hand the current revision back.
func (s *Service) SaveConfig(actor, action, id string, write func() (apps.ConfigFile, error)) (ConfigResult, error) {
	file, err := write()
	if err != nil {
		s.Audit(actor, file.App, action, id, err)
		return ConfigResult{File: file}, err
	}
	rescan, err := s.rescan()
	if err != nil {
		s.Audit(actor, file.App, action, file.ID, err)
		return ConfigResult{File: file}, fmt.Errorf("%w: %w", ErrRescan, err)
	}
	s.Audit(actor, file.App, action, file.ID, nil)
	if len(rescan.RestartRequired) > 0 {
		s.notify(notify.ConfigChanged, file.App, "restart required: "+strings.Join(rescan.RestartRequired, ", "))
	}
	return ConfigResult{File: file, Invalid: rescan.Invalid, RestartRequired: rescan.RestartRequired}, nil
}

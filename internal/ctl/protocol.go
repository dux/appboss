package ctl

import "deploy-boss/internal/ops"

// Request is the wire form of one action. It is the same type both transports build, so the
// control socket and the console cannot drift apart.
type Request = ops.Request

type Response struct {
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

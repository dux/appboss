package ctl

import "encoding/json"

type Request struct {
	Method  string          `json:"method"`
	App     string          `json:"app,omitempty"`
	Process string          `json:"process,omitempty"`
	Lines   int             `json:"lines,omitempty"`
	On      bool            `json:"on,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

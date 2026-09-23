// Package logx is dboss's own leveled logger. It wraps the standard logger, so the daemon log
// sink still receives every line, and drops calls below daemon.log_level.
package logx

import (
	"log"
	"strings"
	"sync/atomic"
)

type level int32

const (
	debugLevel level = iota
	infoLevel
	warnLevel
	errorLevel
)

var current atomic.Int32

func init() { current.Store(int32(infoLevel)) }

// SetLevel sets the minimum level to print: debug, info, warn or error. An unknown name keeps
// the current level.
func SetLevel(name string) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		current.Store(int32(debugLevel))
	case "info":
		current.Store(int32(infoLevel))
	case "warn":
		current.Store(int32(warnLevel))
	case "error":
		current.Store(int32(errorLevel))
	}
}

func Debugf(format string, args ...any) {
	if current.Load() <= int32(debugLevel) {
		log.Printf(format, args...)
	}
}

func Infof(format string, args ...any) {
	if current.Load() <= int32(infoLevel) {
		log.Printf(format, args...)
	}
}

func Warnf(format string, args ...any) {
	if current.Load() <= int32(warnLevel) {
		log.Printf(format, args...)
	}
}

func Errorf(format string, args ...any) {
	if current.Load() <= int32(errorLevel) {
		log.Printf(format, args...)
	}
}

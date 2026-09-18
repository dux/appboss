//go:build !linux && !darwin

package sysinfo

func collectHost() Host { return hostBasics() }

func osName() string { return "" }

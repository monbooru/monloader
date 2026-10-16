// Package logx gates the stdlib logger by level.
package logx

import (
	"log"
	"strings"
	"sync/atomic"
)

type Level int32

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
)

var level atomic.Int32

func Set(name string) {
	var l Level
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "info":
		l = LevelInfo
	case "debug":
		l = LevelDebug
	default:
		l = LevelWarn
	}
	level.Store(int32(l))
}

// Get returns the level's name in the form Set takes.
func Get() string {
	switch Level(level.Load()) {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	}
	return "warn"
}

func Enabled(l Level) bool { return l >= Level(level.Load()) }

func Debugf(format string, a ...any) {
	if Enabled(LevelDebug) {
		log.Printf("DEBUG "+format, a...)
	}
}

func Infof(format string, a ...any) {
	if Enabled(LevelInfo) {
		log.Printf("INFO "+format, a...)
	}
}

func Warnf(format string, a ...any)  { log.Printf("WARN "+format, a...) }
func Errorf(format string, a ...any) { log.Printf("ERROR "+format, a...) }

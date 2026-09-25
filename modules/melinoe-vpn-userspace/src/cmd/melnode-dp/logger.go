/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 */

package main

import (
	"log"
	"os"
)

type Logger struct {
	Errorf func(format string, args ...any)
}

const (
	LogLevelSilent = iota
	LogLevelError
)

func DiscardLogf(format string, args ...any) {}

func NewLogger(level int, prepend string) *Logger {
	logger := &Logger{DiscardLogf}
	logf := func(prefix string) func(string, ...any) {
		return log.New(os.Stdout, prefix+": "+prepend, log.Ldate|log.Ltime).Printf
	}
	if level >= LogLevelError {
		logger.Errorf = logf("ERROR")
	}
	return logger
}

// Copyright 2024 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package run

import (
	"io"
	"os"

	"gitea.com/gitea/act_runner/internal/pkg/report"
	log "github.com/sirupsen/logrus"
)

type JobLoggerFactoryWithInfoLevel struct{}

// WithJobLogger implements [runner.JobLoggerFactory].
func (j *JobLoggerFactoryWithInfoLevel) WithJobLogger() *log.Logger {
	jobLogger := log.New()
	jobLogger.SetLevel(log.InfoLevel)
	return jobLogger
}

type JobLoggerWithReporter struct {
	Reporter      *report.Reporter
	LogToTerminal bool
}

// WithJobLogger implements [runner.JobLoggerFactory].
func (j *JobLoggerWithReporter) WithJobLogger() *log.Logger {
	jobLogger := log.New()
	if j.LogToTerminal {
		jobLogger.SetOutput(os.Stdout)
	} else {
		jobLogger.SetOutput(io.Discard)
	}
	jobLogger.AddHook(j.Reporter)
	return jobLogger
}

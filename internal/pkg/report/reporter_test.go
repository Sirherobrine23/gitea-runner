// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package report

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	runnerv1 "code.gitea.io/actions-proto-go/runner/v1"
	connect_go "connectrpc.com/connect"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"gitea.com/gitea/act_runner/internal/pkg/client/mocks"
)

func TestReporter_parseLogRow(t *testing.T) {
	tests := []struct {
		name               string
		debugOutputEnabled bool
		args               []string
		want               []string
	}{
		{
			"No command", false,
			[]string{"Hello, world!"},
			[]string{"Hello, world!"},
		},
		{
			"Add-mask", false,
			[]string{
				"foo mysecret bar",
				"::add-mask::mysecret",
				"foo mysecret bar",
			},
			[]string{
				"foo mysecret bar",
				"<nil>",
				"foo *** bar",
			},
		},
		{
			"Debug enabled", true,
			[]string{
				"::debug::GitHub Actions runtime token access controls",
			},
			[]string{
				"GitHub Actions runtime token access controls",
			},
		},
		{
			"Debug not enabled", false,
			[]string{
				"::debug::GitHub Actions runtime token access controls",
			},
			[]string{
				"<nil>",
			},
		},
		{
			"notice", false,
			[]string{
				"::notice file=file.name,line=42,endLine=48,title=Cool Title::Gosh, that's not going to work",
			},
			[]string{
				"::notice file=file.name,line=42,endLine=48,title=Cool Title::Gosh, that's not going to work",
			},
		},
		{
			"warning", false,
			[]string{
				"::warning file=file.name,line=42,endLine=48,title=Cool Title::Gosh, that's not going to work",
			},
			[]string{
				"::warning file=file.name,line=42,endLine=48,title=Cool Title::Gosh, that's not going to work",
			},
		},
		{
			"error", false,
			[]string{
				"::error file=file.name,line=42,endLine=48,title=Cool Title::Gosh, that's not going to work",
			},
			[]string{
				"::error file=file.name,line=42,endLine=48,title=Cool Title::Gosh, that's not going to work",
			},
		},
		{
			"group", false,
			[]string{
				"::group::",
				"::endgroup::",
			},
			[]string{
				"::group::",
				"::endgroup::",
			},
		},
		{
			"stop-commands", false,
			[]string{
				"::add-mask::foo",
				"::stop-commands::myverycoolstoptoken",
				"::add-mask::bar",
				"::debug::Stuff",
				"myverycoolstoptoken",
				"::add-mask::baz",
				"::myverycoolstoptoken::",
				"::add-mask::wibble",
				"foo bar baz wibble",
			},
			[]string{
				"<nil>",
				"<nil>",
				"::add-mask::bar",
				"::debug::Stuff",
				"myverycoolstoptoken",
				"::add-mask::baz",
				"<nil>",
				"<nil>",
				"*** bar baz ***",
			},
		},
		{
			"unknown command", false,
			[]string{
				"::set-mask::foo",
			},
			[]string{
				"::set-mask::foo",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Reporter{
				logReplacer:        strings.NewReplacer(),
				debugOutputEnabled: tt.debugOutputEnabled,
			}
			for idx, arg := range tt.args {
				rv := r.parseLogRow(&log.Entry{Message: arg})
				got := "<nil>"

				if rv != nil {
					got = rv.Content
				}

				assert.Equal(t, tt.want[idx], got)
			}
		})
	}
}

func TestReporter_Fire(t *testing.T) {
	t.Run("ignore command lines", func(t *testing.T) {
		client := mocks.NewClient(t)
		client.On("UpdateLog", mock.Anything, mock.Anything).Return(func(_ context.Context, req *connect_go.Request[runnerv1.UpdateLogRequest]) (*connect_go.Response[runnerv1.UpdateLogResponse], error) {
			t.Logf("Received UpdateLog: %s", req.Msg.String())
			return connect_go.NewResponse(&runnerv1.UpdateLogResponse{
				AckIndex: req.Msg.Index + int64(len(req.Msg.Rows)),
			}), nil
		})
		client.On("UpdateTask", mock.Anything, mock.Anything).Return(func(_ context.Context, req *connect_go.Request[runnerv1.UpdateTaskRequest]) (*connect_go.Response[runnerv1.UpdateTaskResponse], error) {
			t.Logf("Received UpdateTask: %s", req.Msg.String())
			return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		taskCtx, err := structpb.NewStruct(map[string]interface{}{})
		require.NoError(t, err)
		reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{
			Context: taskCtx,
		})
		reporter.RunDaemon()
		defer func() {
			assert.NoError(t, reporter.Close(""))
		}()
		reporter.ResetSteps(5)

		dataStep0 := map[string]interface{}{
			"stage":      "Main",
			"stepNumber": 0,
			"raw_output": true,
		}

		assert.NoError(t, reporter.Fire(&log.Entry{Message: "regular log line", Data: dataStep0}))
		assert.NoError(t, reporter.Fire(&log.Entry{Message: "::debug::debug log line", Data: dataStep0}))
		assert.NoError(t, reporter.Fire(&log.Entry{Message: "regular log line", Data: dataStep0}))
		assert.NoError(t, reporter.Fire(&log.Entry{Message: "::debug::debug log line", Data: dataStep0}))
		assert.NoError(t, reporter.Fire(&log.Entry{Message: "::debug::debug log line", Data: dataStep0}))
		assert.NoError(t, reporter.Fire(&log.Entry{Message: "regular log line", Data: dataStep0}))
		assert.NoError(t, reporter.Fire(&log.Entry{Message: "composite step result", Data: map[string]interface{}{
			"stage":      "Main",
			"stepID":     []string{"0", "0"},
			"stepNumber": 0,
			"raw_output": true,
			"stepResult": "failure",
		}}))
		assert.Equal(t, runnerv1.Result_RESULT_UNSPECIFIED, reporter.state.Steps[0].Result)
		assert.NoError(t, reporter.Fire(&log.Entry{Message: "step result", Data: map[string]interface{}{
			"stage":      "Main",
			"stepNumber": 0,
			"raw_output": true,
			"stepResult": "success",
		}}))
		assert.Equal(t, runnerv1.Result_RESULT_SUCCESS, reporter.state.Steps[0].Result)

		assert.Equal(t, int64(5), reporter.state.Steps[0].LogLength)
	})
}

// TestReporter_EphemeralRunnerDeletion reproduces the exact scenario from
// https://gitea.com/gitea/act_runner/issues/793:
//
//  1. RunDaemon calls ReportLog(false) — runner is still alive
//  2. Close() updates state to Result=FAILURE (between RunDaemon's ReportLog and ReportState)
//  3. RunDaemon's ReportState() would clone the completed state and send it,
//     but the fix makes ReportState return early when closed, preventing this
//  4. Close's ReportLog(true) succeeds because the runner was not deleted
func TestReporter_EphemeralRunnerDeletion(t *testing.T) {
	runnerDeleted := false

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *connect_go.Request[runnerv1.UpdateLogRequest]) (*connect_go.Response[runnerv1.UpdateLogResponse], error) {
			if runnerDeleted {
				return nil, fmt.Errorf("runner has been deleted")
			}
			return connect_go.NewResponse(&runnerv1.UpdateLogResponse{
				AckIndex: req.Msg.Index + int64(len(req.Msg.Rows)),
			}), nil
		},
	)
	client.On("UpdateTask", mock.Anything, mock.Anything).Maybe().Return(
		func(_ context.Context, req *connect_go.Request[runnerv1.UpdateTaskRequest]) (*connect_go.Response[runnerv1.UpdateTaskResponse], error) {
			// Server deletes ephemeral runner when it receives a completed state
			if req.Msg.State != nil && req.Msg.State.Result != runnerv1.Result_RESULT_UNSPECIFIED {
				runnerDeleted = true
			}
			return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	taskCtx, err := structpb.NewStruct(map[string]interface{}{})
	require.NoError(t, err)
	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx})
	reporter.ResetSteps(1)

	// Fire a log entry to create pending data
	assert.NoError(t, reporter.Fire(&log.Entry{
		Message: "build output",
		Data:    log.Fields{"stage": "Main", "stepNumber": 0, "raw_output": true},
	}))

	// Step 1: RunDaemon calls ReportLog(false) — runner is still alive
	assert.NoError(t, reporter.ReportLog(false))

	// Step 2: Close() updates state — sets Result=FAILURE and marks steps cancelled.
	// In the real race, this happens while RunDaemon is between ReportLog and ReportState.
	reporter.stateMu.Lock()
	reporter.closed = true
	for _, v := range reporter.state.Steps {
		if v.Result == runnerv1.Result_RESULT_UNSPECIFIED {
			v.Result = runnerv1.Result_RESULT_CANCELLED
		}
	}
	reporter.state.Result = runnerv1.Result_RESULT_FAILURE
	reporter.logRows = append(reporter.logRows, &runnerv1.LogRow{
		Time:    timestamppb.Now(),
		Content: "Early termination",
	})
	reporter.state.StoppedAt = timestamppb.Now()
	reporter.stateMu.Unlock()

	// Step 3: RunDaemon's ReportState() — with the fix, this returns early
	// because closed=true, preventing the server from deleting the runner.
	assert.NoError(t, reporter.ReportState(false))
	assert.False(t, runnerDeleted, "runner must not be deleted by RunDaemon's ReportState")

	// Step 4: Close's final log upload succeeds because the runner is still alive.
	// Flush pending rows first, then send the noMore signal (matching Close's retry behavior).
	assert.NoError(t, reporter.ReportLog(false))
	// Acknowledge Close as done in daemon
	close(reporter.daemon)
	err = reporter.ReportLog(true)
	assert.NoError(t, err, "final log upload must not fail: runner should not be deleted before Close finishes sending logs")
	err = reporter.ReportState(true)
	assert.NoError(t, err, "final state update should work: runner should not be deleted before Close finishes sending logs")
}

func TestReporter_RunDaemonClose_Race(t *testing.T) {
	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *connect_go.Request[runnerv1.UpdateLogRequest]) (*connect_go.Response[runnerv1.UpdateLogResponse], error) {
			return connect_go.NewResponse(&runnerv1.UpdateLogResponse{
				AckIndex: req.Msg.Index + int64(len(req.Msg.Rows)),
			}), nil
		},
	)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *connect_go.Request[runnerv1.UpdateTaskRequest]) (*connect_go.Response[runnerv1.UpdateTaskResponse], error) {
			return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	taskCtx, err := structpb.NewStruct(map[string]interface{}{})
	require.NoError(t, err)
	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{
		Context: taskCtx,
	})
	reporter.ResetSteps(1)

	// Start the daemon loop in a separate goroutine.
	// RunDaemon reads r.closed and reschedules itself via time.AfterFunc.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		reporter.RunDaemon()
	}()

	// Close concurrently — this races with RunDaemon on r.closed.
	assert.NoError(t, reporter.Close(""))

	// Cancel context so pending AfterFunc callbacks exit quickly.
	cancel()
	wg.Wait()
	time.Sleep(2 * time.Second)
}

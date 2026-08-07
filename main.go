// Copyright 2022 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"os/signal"
	"syscall"

	"gitea.com/gitea/runner/internal/app/cmd"
	"gitea.com/gitea/runner/internal/pkg/ver"
)

// version is injected at build time with `-ldflags "-X main.version=v1.2.3"`.
var version = "dev"

func main() {
	ver.SetVersion(version)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// run the command
	cmd.Execute(ctx)
}

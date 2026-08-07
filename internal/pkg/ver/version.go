// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package ver

var version = "dev"

// SetVersion records the version injected into package main at build time.
func SetVersion(v string) {
	if v != "" {
		version = v
	}
}

func Version() string {
	return version
}

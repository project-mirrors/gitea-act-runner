// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package ver

// go build -ldflags "-X gitea.com/gitea/runner/internal/pkg/ver.version=1.2.3"
var version = "dev"

const MinGiteaVersion = "1.26.2" // also in the README.md prerequisites

func Version() string {
	return version
}

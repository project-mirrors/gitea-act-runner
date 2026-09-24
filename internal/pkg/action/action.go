// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package action holds the native actions run by `uses: builtin:<name>`.
package action

import (
	"gitea.com/gitea/runner/act/container"

	"gitea.dev/actionslib/pkg/model"
)

type Context struct {
	Container       container.ExecutionsEnvironment
	Github          *model.GithubContext
	Inputs          map[string]string
	HostWorkdir     string
	Workspace       string // HostWorkdir as seen inside Container
	BindWorkdir     bool
	NoSkipCheckout  bool
	UseGitIgnore    bool
	InsecureSkipTLS bool
}

// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package lookpath

import (
	"runtime"
	"strings"
)

func getenv(env map[string]string, name string) string {
	if runtime.GOOS == "windows" {
		for key, value := range env {
			if strings.EqualFold(name, key) {
				return value
			}
		}
	}
	return env[name]
}

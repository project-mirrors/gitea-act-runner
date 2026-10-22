// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package process

import (
	"path/filepath"
	"testing"
)

func TestDirContainsPath(t *testing.T) {
	base := filepath.Join("base", "job1")

	cases := []struct {
		name   string
		dir    string
		target string
		want   bool
	}{
		{"same dir", base, base, true},
		{"direct child", base, filepath.Join(base, "app.exe"), true},
		{"deep child", base, filepath.Join(base, "sub", "sub", "app.exe"), true},
		{"parent is not within child", filepath.Join(base, "sub"), base, false},
		{"name-prefix sibling spared", base, filepath.Join("base", "job10", "app.exe"), false},
		{"unrelated", base, filepath.Join("other", "app.exe"), false},
		{"empty dir", "", base, false},
		{"empty target", base, "", false},
		{"dot dir", ".", base, false},
		{"case-insensitive hit", base, filepath.Join("base", "JOB1", "app.exe"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dirContainsPath(tc.dir, tc.target)
			if got != tc.want {
				t.Fatalf("dirContainsPath(%q, %q) = %v, want %v", tc.dir, tc.target, got, tc.want)
			}
		})
	}
}

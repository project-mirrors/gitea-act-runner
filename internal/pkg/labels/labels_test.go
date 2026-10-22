// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package labels

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	tests := []struct {
		args string
		want *Label
	}{
		{"ubuntu:docker://node:18", &Label{Name: "ubuntu", Schema: "docker", Arg: "//node:18"}},
		{"ubuntu:host", &Label{Name: "ubuntu", Schema: "host"}},
		{"ubuntu", &Label{Name: "ubuntu", Schema: "host"}},
		{"pool:e57e18d4-10d4-406f-93bf-60f127221bdd", &Label{Name: "pool:e57e18d4-10d4-406f-93bf-60f127221bdd", Schema: "host", Opaque: true}},
		{"ubuntu:vm:ubuntu-18.04", &Label{Name: "ubuntu:vm:ubuntu-18.04", Schema: "host", Opaque: true}},
		{"", nil},
	}
	for _, tt := range tests {
		t.Run(tt.args, func(t *testing.T) {
			got, err := Parse(tt.args)
			if tt.want == nil {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func mustParse(t *testing.T, strs ...string) Labels {
	t.Helper()
	ls := make(Labels, 0, len(strs))
	for _, s := range strs {
		l, err := Parse(s)
		require.NoError(t, err)
		ls = append(ls, l)
	}
	return ls
}

func TestRequireDocker(t *testing.T) {
	tests := []struct {
		name string
		strs []string
		want bool
	}{
		{"empty", nil, false},
		{"only host", []string{"ubuntu:host", "self-hosted"}, false},
		{"has docker", []string{"ubuntu:host", "ubuntu:docker://node:18"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, mustParse(t, tt.strs...).RequireDocker())
		})
	}
}

func TestPickPlatform(t *testing.T) {
	ls := mustParse(t,
		"ubuntu:docker://node:18",
		"self-hosted:host",
		"k8s:kubernetes://node:24",
	)

	tests := []struct {
		name   string
		runsOn []string
		want   string
	}{
		{"docker strips leading slashes", []string{"ubuntu"}, "node:18"},
		{"kubernetes keeps its scheme", []string{"k8s"}, "kubernetes://node:24"},
		{"host maps to self-hosted marker", []string{"self-hosted"}, SelfHostedPlatform},
		{"first match wins", []string{"self-hosted", "ubuntu"}, SelfHostedPlatform},
		{"unknown label picks nothing", []string{"windows"}, ""},
		{"no runsOn picks nothing", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, ls.PickPlatform(tt.runsOn))
		})
	}
}

func TestNames(t *testing.T) {
	ls := mustParse(t, "ubuntu:docker://node:18", "self-hosted:host", "pool:e57e18d4")
	require.Equal(t, []string{"ubuntu", "self-hosted", "pool:e57e18d4"}, ls.Names())
	require.Empty(t, Labels{}.Names())
}

func TestToStrings(t *testing.T) {
	ls := mustParse(t,
		"ubuntu:docker://node:18",
		"self-hosted:host",
		"bare",
		"pool:e57e18d4",
	)
	require.Equal(t, []string{
		"ubuntu:docker://node:18",
		"self-hosted:host",
		"bare:host",
		"pool:e57e18d4",
	}, ls.ToStrings())
}

func TestOpaqueLabelRoundTrip(t *testing.T) {
	const raw = "pool:e57e18d4-10d4-406f-93bf-60f127221bdd"

	ls := mustParse(t, raw)
	require.Equal(t, []string{raw}, ls.ToStrings())

	again := mustParse(t, ls.ToStrings()...)
	require.Equal(t, ls, again)
	require.Equal(t, []string{raw}, again.Names())
	require.False(t, again.RequireDocker())
	require.Equal(t, SelfHostedPlatform, again.PickPlatform([]string{raw}))
}

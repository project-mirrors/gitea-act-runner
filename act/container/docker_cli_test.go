// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// This file is based on https://github.com/docker/cli/blob/4a63305d74332de5ceba7fcbccbc3cbb7412f5ba/cli/command/container/opts_test.go with:
// * appended with license information
// * added tests for the locally changed parseDevice, validateDevice and invalidParameter
//
// docker/cli is licensed under the Apache License, Version 2.0.
// See DOCKER_LICENSE for the full license text.
//

//nolint:gocritic // verbatim copy from docker/cli tests
package container

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	networktypes "github.com/moby/moby/api/types/network"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustParseMAC(s string) networktypes.HardwareAddr {
	mac, err := net.ParseMAC(s)
	if err != nil {
		panic(err)
	}
	return networktypes.HardwareAddr(mac)
}

func TestValidateAttach(t *testing.T) {
	valid := []string{
		"stdin",
		"stdout",
		"stderr",
		"STDIN",
		"STDOUT",
		"STDERR",
	}
	if _, err := validateAttach("invalid"); err == nil {
		t.Fatal("Expected error with [valid streams are STDIN, STDOUT and STDERR], got nothing")
	}

	for _, attach := range valid {
		value, err := validateAttach(attach)
		if err != nil {
			t.Fatal(err)
		}
		if value != strings.ToLower(attach) {
			t.Fatalf("Expected [%v], got [%v]", attach, value)
		}
	}
}

func parseRun(args []string) (*container.Config, *container.HostConfig, *networktypes.NetworkingConfig, error) {
	flags, copts := setupRunFlags()
	if err := flags.Parse(args); err != nil {
		return nil, nil, nil, err
	}
	containerCfg, err := parse(flags, copts, runtime.GOOS)
	if err != nil {
		return nil, nil, nil, err
	}
	return containerCfg.Config, containerCfg.HostConfig, containerCfg.NetworkingConfig, err
}

func setupRunFlags() (*pflag.FlagSet, *containerOptions) {
	flags := pflag.NewFlagSet("run", pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = nil
	return flags, addFlags(flags)
}

func mustParse(t *testing.T, args string) (*container.Config, *container.HostConfig, *networktypes.NetworkingConfig) {
	t.Helper()
	config, hostConfig, nwConfig, err := parseRun(strings.Split(args, " "))
	require.NoError(t, err)
	return config, hostConfig, nwConfig
}

func TestParseRunLinks(t *testing.T) {
	tests := []struct {
		name               string
		input              string
		expHostConfigLinks []string
		expNetConfigLinks  map[string][]string
	}{
		// Default bridge - legacy links ...
		{
			name:               "default/onelink",
			input:              "--link a:b",
			expHostConfigLinks: []string{"a:b"},
			expNetConfigLinks:  map[string][]string{"default": nil},
		},
		{
			name:               "default/twolinks",
			input:              "--link a:b --link c:d",
			expHostConfigLinks: []string{"a:b", "c:d"},
			expNetConfigLinks:  map[string][]string{"default": nil},
		},
		{
			name:               "bridge/onelink",
			input:              "--network bridge --link a:b",
			expHostConfigLinks: []string{"a:b"},
			// expNetConfigLinks - no EndpointsConfig is created for a single named network with no options set.
			// See the "For backward compatibility" comment in parseNetworkOpts().
		},
		{
			name:              "default/nolinks",
			expNetConfigLinks: map[string][]string{"default": nil},
		},

		// User-defined bridge - links become DNS aliases ...
		{
			name:               "userdefnet/onelink",
			input:              "--network userdefnet --link a:b",
			expHostConfigLinks: []string{"a:b"},
			expNetConfigLinks:  map[string][]string{"userdefnet": {"a:b"}},
		},
		{
			name:               "userdefnet/twolinks",
			input:              "--network userdefnet --link a:b --link c:d",
			expHostConfigLinks: []string{"a:b", "c:d"},
			expNetConfigLinks:  map[string][]string{"userdefnet": {"a:b", "c:d"}},
		},
		{
			name:  "userdefnet/nolinks",
			input: "--network userdefnet",
		},
		{
			// Link options are applied to the first network (and there's no "advanced syntax"
			// link key, like "--network name=userdefnet,link=a:b").
			name:               "links apply to the first network",
			input:              "--network userdefnet --link a:b --network bar --link c:d",
			expHostConfigLinks: []string{"a:b", "c:d"},
			expNetConfigLinks:  map[string][]string{"userdefnet": {"a:b", "c:d"}, "bar": nil},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, hostConfig, netConfig := mustParse(t, tc.input)
			assert.Equal(t, tc.expHostConfigLinks, hostConfig.Links)
			assert.Len(t, netConfig.EndpointsConfig, len(tc.expNetConfigLinks))
			for netName, expLinks := range tc.expNetConfigLinks {
				nc, ok := netConfig.EndpointsConfig[netName]
				require.True(t, ok)
				assert.Equal(t, expLinks, nc.Links)
			}
		})
	}
}

func TestParseRunWithoutUmask(t *testing.T) {
	_, hostConfig, _, err := parseRun([]string{"ubuntu", "bash"})
	require.NoError(t, err)
	require.Nil(t, hostConfig.Umask)
}

func TestParseRunUmask(t *testing.T) {
	_, hostConfig, _, err := parseRun([]string{"--umask", "0022", "ubuntu", "bash"})
	require.NoError(t, err)
	require.NotNil(t, hostConfig.Umask)
	require.Equal(t, uint32(0o22), *hostConfig.Umask)
}

func TestParseRunAttach(t *testing.T) {
	for _, tc := range []struct {
		input                 string
		stdin, stdout, stderr bool
	}{
		{"", false, true, true},
		{"-i", true, true, true},
		{"-a stdin", true, false, false},
		{"-a stdin -a stdout", true, true, false},
		{"-a stdin -a stdout -a stderr", true, true, true},
	} {
		t.Run(tc.input, func(t *testing.T) {
			config, _, _ := mustParse(t, tc.input)
			require.Equal(t, tc.stdin, config.AttachStdin)
			require.Equal(t, tc.stdout, config.AttachStdout)
			require.Equal(t, tc.stderr, config.AttachStderr)
		})
	}
}

func TestParseRunWithInvalidArgs(t *testing.T) {
	tests := []struct {
		args  []string
		error string
	}{
		{
			args:  []string{"-a", "ubuntu", "bash"},
			error: `invalid argument "ubuntu" for "-a, --attach" flag: valid streams are STDIN, STDOUT and STDERR`,
		},
		{
			args:  []string{"-a", "invalid", "ubuntu", "bash"},
			error: `invalid argument "invalid" for "-a, --attach" flag: valid streams are STDIN, STDOUT and STDERR`,
		},
		{
			args:  []string{"-a", "invalid", "-a", "stdout", "ubuntu", "bash"},
			error: `invalid argument "invalid" for "-a, --attach" flag: valid streams are STDIN, STDOUT and STDERR`,
		},
		{
			args:  []string{"-a", "stdout", "-a", "stderr", "-z", "ubuntu", "bash"},
			error: `unknown shorthand flag: 'z' in -z`,
		},
		{
			args:  []string{"-a", "stdin", "-z", "ubuntu", "bash"},
			error: `unknown shorthand flag: 'z' in -z`,
		},
		{
			args:  []string{"-a", "stdout", "-z", "ubuntu", "bash"},
			error: `unknown shorthand flag: 'z' in -z`,
		},
		{
			args:  []string{"-a", "stderr", "-z", "ubuntu", "bash"},
			error: `unknown shorthand flag: 'z' in -z`,
		},
		{
			args:  []string{"-z", "--rm", "ubuntu", "bash"},
			error: `unknown shorthand flag: 'z' in -z`,
		},
	}
	flags, _ := setupRunFlags()
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			require.EqualError(t, flags.Parse(tc.args), tc.error)
		})
	}
}

func TestParseWithVolumes(t *testing.T) {
	for _, tc := range []struct {
		unix, windows []string
		binds         int
	}{
		{[]string{`/tmp`}, []string{`c:\tmp`}, 0},
		{[]string{`/tmp`, `/var`}, []string{`c:\tmp`, `c:\var`}, 0},
		{[]string{`/hostTmp:/containerTmp`}, []string{os.Getenv("TEMP") + `:c:\containerTmp`}, 1},
		{[]string{`/hostTmp:/containerTmp`, `/hostVar:/containerVar`}, []string{os.Getenv("ProgramData") + `:c:\ContainerPD`, os.Getenv("TEMP") + `:c:\containerTmp`}, 2},
		{[]string{`/hostTmp:/containerTmp:ro`, `/hostVar:/containerVar:rw`}, []string{os.Getenv("TEMP") + `:c:\containerTmp:rw`, os.Getenv("ProgramData") + `:c:\ContainerPD:rw`}, 2},
		{[]string{`/hostTmp:/containerTmp:ro,Z`, `/hostVar:/containerVar:rw,Z`}, nil, 2},
		{[]string{`/hostTmp:/containerTmp:Z`, `/hostVar:/containerVar:z`}, nil, 2},
		{[]string{`/hostTmp:/containerTmp`, `/containerVar`}, []string{os.Getenv("TEMP") + `:c:\containerTmp`, `c:\containerTmp`}, 1},
		{nil, []string{os.Getenv("SystemDrive") + `\:d:`}, 1},
	} {
		volumes := tc.unix
		if runtime.GOOS == "windows" {
			volumes = tc.windows
		}
		if len(volumes) == 0 {
			continue
		}
		t.Run(strings.Join(volumes, ","), func(t *testing.T) {
			var args []string
			for _, volume := range volumes {
				args = append(args, "-v", volume)
			}
			config, hostConfig, _, err := parseRun(args)
			require.NoError(t, err)
			if tc.binds == 0 {
				assert.Nil(t, hostConfig.Binds)
			} else {
				assert.ElementsMatch(t, volumes[:tc.binds], hostConfig.Binds)
			}
			expectedVolumes := map[string]struct{}{}
			for _, volume := range volumes[tc.binds:] {
				expectedVolumes[volume] = struct{}{}
			}
			assert.Equal(t, expectedVolumes, config.Volumes)
		})
	}
}

// Simple parse with MacAddress validation
func TestParseWithMacAddress(t *testing.T) {
	invalidMacAddress := "--mac-address=invalidMacAddress"
	validMacAddress := "--mac-address=92:d0:c6:0a:29:33"
	if _, _, _, err := parseRun([]string{invalidMacAddress, "img", "cmd"}); err != nil && err.Error() != "invalidMacAddress is not a valid mac address" {
		t.Fatalf("Expected an error with %v mac-address, got %v", invalidMacAddress, err)
	}
	_, hostConfig, nwConfig := mustParse(t, validMacAddress)
	defaultNw := hostConfig.NetworkMode.NetworkName()
	if nwConfig.EndpointsConfig[defaultNw].MacAddress.String() != "92:d0:c6:0a:29:33" {
		t.Fatalf("Expected the default endpoint to have the MacAddress '92:d0:c6:0a:29:33' set, got '%v'", nwConfig.EndpointsConfig[defaultNw].MacAddress)
	}
}

func TestRunFlagsParseWithMemory(t *testing.T) {
	flags, _ := setupRunFlags()
	args := []string{"--memory=invalid", "img", "cmd"}
	err := flags.Parse(args)
	require.ErrorContains(t, err, `invalid argument "invalid" for "-m, --memory" flag`)

	_, hostconfig, _ := mustParse(t, "--memory=1G")
	assert.Equal(t, int64(1073741824), hostconfig.Memory)
}

func TestParseWithMemorySwap(t *testing.T) {
	flags, _ := setupRunFlags()
	args := []string{"--memory-swap=invalid", "img", "cmd"}
	err := flags.Parse(args)
	require.ErrorContains(t, err, `invalid argument "invalid" for "--memory-swap" flag`)

	_, hostconfig, _ := mustParse(t, "--memory-swap=1G")
	assert.Equal(t, int64(1073741824), hostconfig.MemorySwap)

	_, hostconfig, _ = mustParse(t, "--memory-swap=-1")
	assert.Equal(t, int64(-1), hostconfig.MemorySwap)
}

func TestParseHostname(t *testing.T) {
	for _, tc := range []struct {
		input, hostname, domainname string
	}{
		{"--hostname=hostname", "hostname", ""},
		{"--hostname=host-name", "host-name", ""},
		{"--hostname=hostname123", "hostname123", ""},
		{"--hostname=123hostname", "123hostname", ""},
		{"--hostname=hostname-of-63-bytes-long-should-be-valid-and-without-any-error", "hostname-of-63-bytes-long-should-be-valid-and-without-any-error", ""},
		{"--hostname=hostname.domainname", "hostname.domainname", ""},
		{"--hostname=hostname.domainname.tld", "hostname.domainname.tld", ""},
		{"--domainname=domainname", "", "domainname"},
		{"--domainname=domain-name", "", "domain-name"},
		{"--domainname=domainname123", "", "domainname123"},
		{"--domainname=123domainname", "", "123domainname"},
		{"--domainname=domainname-63-bytes-long-should-be-valid-and-without-any-errors", "", "domainname-63-bytes-long-should-be-valid-and-without-any-errors"},
		{"--hostname=some.prefix --domainname=domainname", "some.prefix", "domainname"},
		{"--hostname=another-prefix --domainname=domainname.tld", "another-prefix", "domainname.tld"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			config, _, _ := mustParse(t, tc.input)
			require.Equal(t, tc.hostname, config.Hostname)
			require.Equal(t, tc.domainname, config.Domainname)
		})
	}
}

func TestParseWithExpose(t *testing.T) {
	t.Run("invalid", func(t *testing.T) {
		tests := map[string]string{
			":":                   `invalid range format for --expose: invalid start port ':': invalid syntax`,
			"8080:9090":           `invalid range format for --expose: invalid start port '8080:9090': invalid syntax`,
			"/tcp":                `invalid range format for --expose: invalid start port '': value is empty`,
			"/udp":                `invalid range format for --expose: invalid start port '': value is empty`,
			"NaN/tcp":             `invalid range format for --expose: invalid start port 'NaN': invalid syntax`,
			"NaN-NaN/tcp":         `invalid range format for --expose: invalid start port 'NaN': invalid syntax`,
			"8080-NaN/tcp":        `invalid range format for --expose: invalid end port 'NaN': invalid syntax`,
			"1234567890-8080/tcp": `invalid range format for --expose: invalid start port '1234567890': value out of range`,
		}
		for expose, expectedError := range tests {
			t.Run(expose, func(t *testing.T) {
				_, _, _, err := parseRun([]string{fmt.Sprintf("--expose=%v", expose), "img", "cmd"})
				require.EqualError(t, err, expectedError)
			})
		}
	})
	t.Run("valid", func(t *testing.T) {
		tests := map[string][]networktypes.Port{
			"8080/tcp":      {networktypes.MustParsePort("8080/tcp")},
			"8080/udp":      {networktypes.MustParsePort("8080/udp")},
			"8080/ncp":      {networktypes.MustParsePort("8080/ncp")},
			"8080-8080/udp": {networktypes.MustParsePort("8080/udp")},
			"8080-8082/tcp": {networktypes.MustParsePort("8080/tcp"), networktypes.MustParsePort("8081/tcp"), networktypes.MustParsePort("8082/tcp")},
		}
		for expose, exposedPorts := range tests {
			t.Run(expose, func(t *testing.T) {
				config, _, _, err := parseRun([]string{fmt.Sprintf("--expose=%v", expose), "img", "cmd"})
				require.NoError(t, err)
				for _, port := range exposedPorts {
					_, ok := config.ExposedPorts[port]
					assert.True(t, ok, "missing port %q in exposed ports: %#+v", port, config.ExposedPorts[port])
				}
			})
		}
	})

	t.Run("merge with published", func(t *testing.T) {
		config, hostConfig, _, err := parseRun([]string{"--publish=80", "--publish=127.0.0.1:8080:80", "--publish=[::1]:8081:80", "--expose=80-81/tcp"})
		require.NoError(t, err)
		assert.Equal(t, networktypes.PortMap{
			networktypes.MustParsePort("80/tcp"): {
				{},
				{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "8080"},
				{HostIP: netip.MustParseAddr("::1"), HostPort: "8081"},
			},
		}, hostConfig.PortBindings)
		assert.Len(t, config.ExposedPorts, 2)
		ports := []networktypes.Port{networktypes.MustParsePort("80/tcp"), networktypes.MustParsePort("81/tcp")}
		for _, port := range ports {
			_, ok := config.ExposedPorts[port]
			assert.True(t, ok, "missing port %q in exposed ports: %#+v", port, config.ExposedPorts[port])
		}
	})
}

func TestParseDevice(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Windows and macOS validate server-side")
	}
	testCases := []struct {
		devices        []string
		deviceMapping  *container.DeviceMapping
		deviceRequests []container.DeviceRequest
	}{
		{
			devices: []string{"/dev/snd"},
			deviceMapping: &container.DeviceMapping{
				PathOnHost:        "/dev/snd",
				PathInContainer:   "/dev/snd",
				CgroupPermissions: "rwm",
			},
		},
		{
			devices: []string{"/dev/snd:rw"},
			deviceMapping: &container.DeviceMapping{
				PathOnHost:        "/dev/snd",
				PathInContainer:   "/dev/snd",
				CgroupPermissions: "rw",
			},
		},
		{
			devices: []string{"/dev/snd:/something"},
			deviceMapping: &container.DeviceMapping{
				PathOnHost:        "/dev/snd",
				PathInContainer:   "/something",
				CgroupPermissions: "rwm",
			},
		},
		{
			devices: []string{"/dev/snd:/something:rw"},
			deviceMapping: &container.DeviceMapping{
				PathOnHost:        "/dev/snd",
				PathInContainer:   "/something",
				CgroupPermissions: "rw",
			},
		},
		{
			devices:       []string{"vendor.com/class=name"},
			deviceMapping: nil,
			deviceRequests: []container.DeviceRequest{
				{
					Driver:    "cdi",
					DeviceIDs: []string{"vendor.com/class=name"},
				},
			},
		},
		{
			devices: []string{"vendor.com/class=name", "/dev/snd:/something:rw"},
			deviceMapping: &container.DeviceMapping{
				PathOnHost:        "/dev/snd",
				PathInContainer:   "/something",
				CgroupPermissions: "rw",
			},
			deviceRequests: []container.DeviceRequest{
				{
					Driver:    "cdi",
					DeviceIDs: []string{"vendor.com/class=name"},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("%s", tc.devices), func(t *testing.T) {
			var args []string
			for _, d := range tc.devices {
				args = append(args, fmt.Sprintf("--device=%v", d))
			}
			args = append(args, "img", "cmd")

			_, hostconfig, _, err := parseRun(args)

			require.NoError(t, err)

			if tc.deviceMapping != nil {
				if assert.Len(t, hostconfig.Devices, 1) {
					assert.Equal(t, *tc.deviceMapping, hostconfig.Devices[0])
				}
			} else {
				assert.Empty(t, hostconfig.Devices)
			}

			assert.Equal(t, tc.deviceRequests, hostconfig.DeviceRequests)
		})
	}
}

func TestParseDeviceByServerOS(t *testing.T) {
	tests := []struct {
		name     string
		device   string
		serverOS string
		want     container.DeviceMapping
		wantErr  string
	}{
		{
			name:     "linux source only",
			device:   "/dev/snd",
			serverOS: "linux",
			want: container.DeviceMapping{
				PathOnHost:        "/dev/snd",
				PathInContainer:   "/dev/snd",
				CgroupPermissions: "rwm",
			},
		},
		{
			name:     "linux source and mode",
			device:   "/dev/snd:rw",
			serverOS: "linux",
			want: container.DeviceMapping{
				PathOnHost:        "/dev/snd",
				PathInContainer:   "/dev/snd",
				CgroupPermissions: "rw",
			},
		},
		{
			name:     "linux source target and mode",
			device:   "/dev/snd:/container/snd:m",
			serverOS: "linux",
			want: container.DeviceMapping{
				PathOnHost:        "/dev/snd",
				PathInContainer:   "/container/snd",
				CgroupPermissions: "m",
			},
		},
		{
			name:     "windows passes value through",
			device:   `class/GUID`,
			serverOS: "windows",
			want: container.DeviceMapping{
				PathOnHost: `class/GUID`,
			},
		},
		{
			name:     "invalid server OS",
			device:   "/dev/snd",
			serverOS: "plan9",
			wantErr:  "unknown server OS: plan9",
		},
		{
			name:     "too many linux fields",
			device:   "/dev/snd:/container/snd:rw:extra",
			serverOS: "linux",
			wantErr:  "invalid device specification: /dev/snd:/container/snd:rw:extra",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDevice(tc.device, tc.serverOS)
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestParseNetworkConfig(t *testing.T) {
	tests := []struct {
		name            string
		flags           []string
		expected        map[string]*networktypes.EndpointSettings
		expectedHostCfg container.HostConfig
		expectedErr     string
	}{
		{
			name:            "single-network-legacy",
			flags:           []string{"--network", "net1"},
			expected:        map[string]*networktypes.EndpointSettings{},
			expectedHostCfg: container.HostConfig{NetworkMode: "net1"},
		},
		{
			name:            "single-network-advanced",
			flags:           []string{"--network", "name=net1"},
			expected:        map[string]*networktypes.EndpointSettings{},
			expectedHostCfg: container.HostConfig{NetworkMode: "net1"},
		},
		{
			name: "single-network-legacy-with-options",
			flags: []string{
				"--ip", "172.20.88.22",
				"--ip6", "2001:db8::8822",
				"--link", "foo:bar",
				"--link", "bar:baz",
				"--link-local-ip", "169.254.2.2",
				"--link-local-ip", "fe80::169:254:2:2",
				"--network", "name=net1",
				"--network-alias", "web1",
				"--network-alias", "web2",
			},
			expected: map[string]*networktypes.EndpointSettings{
				"net1": {
					IPAMConfig: &networktypes.EndpointIPAMConfig{
						IPv4Address:  netip.MustParseAddr("172.20.88.22"),
						IPv6Address:  netip.MustParseAddr("2001:db8::8822"),
						LinkLocalIPs: []netip.Addr{netip.MustParseAddr("169.254.2.2"), netip.MustParseAddr("fe80::169:254:2:2")},
					},
					Links:   []string{"foo:bar", "bar:baz"},
					Aliases: []string{"web1", "web2"},
				},
			},
			expectedHostCfg: container.HostConfig{NetworkMode: "net1"},
		},
		{
			name: "multiple-network-advanced-mixed",
			flags: []string{
				"--ip", "172.20.88.22",
				"--ip6", "2001:db8::8822",
				"--link", "foo:bar",
				"--link", "bar:baz",
				"--link-local-ip", "169.254.2.2",
				"--link-local-ip", "fe80::169:254:2:2",
				"--network", "name=net1,driver-opt=field1=value1",
				"--network-alias", "web1",
				"--network-alias", "web2",
				"--network", "net2",
				"--network", "name=net3,alias=web3,driver-opt=field3=value3,ip=172.20.88.22,ip6=2001:db8::8822",
				"--network", "name=net4,mac-address=02:32:1c:23:00:04,link-local-ip=169.254.169.254",
			},
			expected: map[string]*networktypes.EndpointSettings{
				"net1": {
					DriverOpts: map[string]string{"field1": "value1"},
					IPAMConfig: &networktypes.EndpointIPAMConfig{
						IPv4Address:  netip.MustParseAddr("172.20.88.22"),
						IPv6Address:  netip.MustParseAddr("2001:db8::8822"),
						LinkLocalIPs: []netip.Addr{netip.MustParseAddr("169.254.2.2"), netip.MustParseAddr("fe80::169:254:2:2")},
					},
					Links:   []string{"foo:bar", "bar:baz"},
					Aliases: []string{"web1", "web2"},
				},
				"net2": {},
				"net3": {
					DriverOpts: map[string]string{"field3": "value3"},
					IPAMConfig: &networktypes.EndpointIPAMConfig{
						IPv4Address: netip.MustParseAddr("172.20.88.22"),
						IPv6Address: netip.MustParseAddr("2001:db8::8822"),
					},
					Aliases: []string{"web3"},
				},
				"net4": {
					MacAddress: mustParseMAC("02:32:1c:23:00:04"),
					IPAMConfig: &networktypes.EndpointIPAMConfig{
						LinkLocalIPs: []netip.Addr{netip.MustParseAddr("169.254.169.254")},
					},
				},
			},
			expectedHostCfg: container.HostConfig{NetworkMode: "net1"},
		},
		{
			name:  "single-network-advanced-with-options",
			flags: []string{"--network", "name=net1,alias=web1,alias=web2,driver-opt=field1=value1,driver-opt=field2=value2,ip=172.20.88.22,ip6=2001:db8::8822,mac-address=02:32:1c:23:00:04"},
			expected: map[string]*networktypes.EndpointSettings{
				"net1": {
					DriverOpts: map[string]string{
						"field1": "value1",
						"field2": "value2",
					},
					IPAMConfig: &networktypes.EndpointIPAMConfig{
						IPv4Address: netip.MustParseAddr("172.20.88.22"),
						IPv6Address: netip.MustParseAddr("2001:db8::8822"),
					},
					Aliases:    []string{"web1", "web2"},
					MacAddress: mustParseMAC("02:32:1c:23:00:04"),
				},
			},
			expectedHostCfg: container.HostConfig{NetworkMode: "net1"},
		},
		{
			name:            "multiple-networks",
			flags:           []string{"--network", "net1", "--network", "name=net2"},
			expected:        map[string]*networktypes.EndpointSettings{"net1": {}, "net2": {}},
			expectedHostCfg: container.HostConfig{NetworkMode: "net1"},
		},
		{
			name:  "advanced-options-with-standalone-mac-address-flag",
			flags: []string{"--network=name=net1,alias=foobar", "--mac-address", "52:0f:f3:dc:50:10"},
			expected: map[string]*networktypes.EndpointSettings{
				"net1": {
					Aliases:    []string{"foobar"},
					MacAddress: mustParseMAC("52:0f:f3:dc:50:10"),
				},
			},
			expectedHostCfg: container.HostConfig{NetworkMode: "net1"},
		},
		{
			name:        "conflict-network",
			flags:       []string{"--network", "duplicate", "--network", "name=duplicate"},
			expectedErr: `network "duplicate" is specified multiple times`,
		},
		{
			name:        "conflict-options-alias",
			flags:       []string{"--network", "name=net1,alias=web1", "--network-alias", "web1"},
			expectedErr: `conflicting options: cannot specify both --network-alias and per-network alias`,
		},
		{
			name:        "conflict-options-ip",
			flags:       []string{"--network", "name=net1,ip=172.20.88.22,ip6=2001:db8::8822", "--ip", "172.20.88.22"},
			expectedErr: `conflicting options: cannot specify both --ip and per-network IPv4 address`,
		},
		{
			name:        "conflict-options-ip6",
			flags:       []string{"--network", "name=net1,ip=172.20.88.22,ip6=2001:db8::8822", "--ip6", "2001:db8::8822"},
			expectedErr: `conflicting options: cannot specify both --ip6 and per-network IPv6 address`,
		},
		{
			name:        "invalid-mixed-network-types",
			flags:       []string{"--network", "name=host", "--network", "net1"},
			expectedErr: `conflicting options: cannot attach both user-defined and non-user-defined network-modes`,
		},
		{
			name:        "conflict-options-link-local-ip",
			flags:       []string{"--network", "name=net1,link-local-ip=169.254.169.254", "--link-local-ip", "169.254.10.8"},
			expectedErr: `conflicting options: cannot specify both --link-local-ip and per-network link-local IP addresses`,
		},
		{
			name:        "conflict-options-mac-address",
			flags:       []string{"--network", "name=net1,mac-address=02:32:1c:23:00:04", "--mac-address", "02:32:1c:23:00:04"},
			expectedErr: `conflicting options: cannot specify both --mac-address and per-network MAC address`,
		},
		{
			name:        "invalid-mac-address",
			flags:       []string{"--network", "name=net1,mac-address=foobar"},
			expectedErr: "foobar is not a valid mac address",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, hConfig, nwConfig, err := parseRun(tc.flags)

			if tc.expectedErr != "" {
				require.EqualError(t, err, tc.expectedErr)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.expectedHostCfg.NetworkMode, hConfig.NetworkMode)
			require.Equal(t, tc.expected, nwConfig.EndpointsConfig)
		})
	}
}

func TestParseModes(t *testing.T) {
	// pid ko
	flags, copts := setupRunFlags()
	args := []string{"--pid=container:", "img", "cmd"}
	require.NoError(t, flags.Parse(args))
	_, err := parse(flags, copts, runtime.GOOS)
	require.ErrorContains(t, err, "--pid: invalid PID mode")

	// pid ok
	_, hostconfig, _, err := parseRun([]string{"--pid=host", "img", "cmd"})
	require.NoError(t, err)
	if !hostconfig.PidMode.Valid() {
		t.Fatalf("Expected a valid PidMode, got %v", hostconfig.PidMode)
	}

	// uts ko
	_, _, _, err = parseRun([]string{"--uts=container:", "img", "cmd"}) //nolint:dogsled // verbatim copy from docker/cli tests
	require.ErrorContains(t, err, "--uts: invalid UTS mode")

	// uts ok
	_, hostconfig, _, err = parseRun([]string{"--uts=host", "img", "cmd"})
	require.NoError(t, err)
	if !hostconfig.UTSMode.Valid() {
		t.Fatalf("Expected a valid UTSMode, got %v", hostconfig.UTSMode)
	}
}

func TestRunFlagsParseShmSize(t *testing.T) {
	// shm-size ko
	flags, _ := setupRunFlags()
	args := []string{"--shm-size=a128m", "img", "cmd"}
	expectedErr := `invalid argument "a128m" for "--shm-size" flag:`
	err := flags.Parse(args)
	require.ErrorContains(t, err, expectedErr)

	// shm-size ok
	_, hostconfig, _, err := parseRun([]string{"--shm-size=128m", "img", "cmd"})
	require.NoError(t, err)
	if hostconfig.ShmSize != 134217728 {
		t.Fatalf("Expected a valid ShmSize, got %d", hostconfig.ShmSize)
	}
}

func TestParseRestartPolicy(t *testing.T) {
	tests := []struct {
		input       string
		expected    container.RestartPolicy
		expectedErr string
	}{
		{
			input: "",
		},
		{
			input: "no",
			expected: container.RestartPolicy{
				Name: container.RestartPolicyDisabled,
			},
		},
		{
			input:       ":1",
			expectedErr: "invalid restart policy format: no policy provided before colon",
		},
		{
			input: "always",
			expected: container.RestartPolicy{
				Name: container.RestartPolicyAlways,
			},
		},
		{
			input:       "always:2:3",
			expectedErr: "invalid restart policy format: maximum retry count must be an integer",
		},
		{
			input: "on-failure:1",
			expected: container.RestartPolicy{
				Name:              container.RestartPolicyOnFailure,
				MaximumRetryCount: 1,
			},
		},
		{
			input:       "on-failure:invalid",
			expectedErr: "invalid restart policy format: maximum retry count must be an integer",
		},
		{
			input: "unless-stopped",
			expected: container.RestartPolicy{
				Name: container.RestartPolicyUnlessStopped,
			},
		},
		{
			input:       "unless-stopped:invalid",
			expectedErr: "invalid restart policy format: maximum retry count must be an integer",
		},

		// Unknown / invalid combinations: validation is handled by the daemon>
		{
			input:    "anything:123",
			expected: container.RestartPolicy{Name: "anything", MaximumRetryCount: 123},
		},
		{
			input:    "negative:-123",
			expected: container.RestartPolicy{Name: "negative", MaximumRetryCount: -123},
		},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			_, hostConfig, _, err := parseRun([]string{"--restart=" + tc.input, "img", "cmd"})
			if tc.expectedErr != "" {
				assert.Nil(t, hostConfig)
				assert.EqualError(t, err, tc.expectedErr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.expected, hostConfig.RestartPolicy)
			}
		})
	}
}

func TestParseRestartPolicyAutoRemove(t *testing.T) {
	_, _, _, err := parseRun([]string{"--rm", "--restart=always", "img", "cmd"}) //nolint:dogsled // verbatim copy from docker/cli tests
	const expected = "conflicting options: cannot specify both --restart and --rm"
	assert.EqualError(t, err, expected)
}

func TestParseHealth(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		expected *container.HealthConfig
		wantErr  string
	}{
		{[]string{"--no-healthcheck", "img", "cmd"}, &container.HealthConfig{Test: []string{"NONE"}}, ""},
		{[]string{"--health-cmd=/check.sh -q", "img", "cmd"}, &container.HealthConfig{Test: []string{"CMD-SHELL", "/check.sh -q"}}, ""},
		{[]string{"--no-healthcheck", "--health-cmd=/check.sh -q", "img", "cmd"}, nil, "--no-healthcheck conflicts with --health-* options"},
		{
			[]string{"--health-timeout=2s", "--health-retries=3", "--health-interval=4.5s", "--health-start-period=5s", "--health-start-interval=1s", "img", "cmd"},
			&container.HealthConfig{Timeout: 2 * time.Second, Retries: 3, Interval: 4500 * time.Millisecond, StartPeriod: 5 * time.Second, StartInterval: time.Second},
			"",
		},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			config, _, _, err := parseRun(tc.args)
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.expected, config.Healthcheck)
			}
		})
	}
}

func TestParseLoggingOpts(t *testing.T) {
	// logging opts ko
	if _, _, _, err := parseRun([]string{"--log-driver=none", "--log-opt=anything", "img", "cmd"}); err == nil || err.Error() != "invalid logging opts for driver none" {
		t.Fatalf("Expected an error with message 'invalid logging opts for driver none', got %v", err)
	}
	// logging opts ok
	_, hostconfig, _, err := parseRun([]string{"--log-driver=syslog", "--log-opt=something", "img", "cmd"})
	if err != nil {
		t.Fatal(err)
	}
	if hostconfig.LogConfig.Type != "syslog" || len(hostconfig.LogConfig.Config) != 1 {
		t.Fatalf("Expected a 'syslog' LogConfig with one config, got %v", hostconfig.RestartPolicy)
	}
}

func TestParseFileVariables(t *testing.T) {
	for _, tc := range []struct {
		flag, file, key string
	}{
		{"env", "valid.env", "ENV"},
		{"label", "valid.label", "LABEL"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			expectedError := "no such file or directory"
			if runtime.GOOS == "windows" {
				expectedError = "The system cannot find the file specified."
			}
			_, _, _, err := parseRun([]string{"--" + tc.flag + "-file=nonexistent"})
			require.EqualError(t, err, "--"+tc.flag+"-file: open nonexistent: "+expectedError)
			for _, extra := range []bool{false, true} {
				args := []string{"--" + tc.flag + "-file=testdata/" + tc.file}
				expected := []string{tc.key + "1=value1"}
				if extra {
					args = append(args, "--"+tc.flag+"="+tc.key+"2=value2")
					expected = append(expected, tc.key+"2=value2")
				}
				config, _, _, err := parseRun(args)
				require.NoError(t, err)
				if tc.flag == "env" {
					assert.Equal(t, expected, config.Env)
				} else {
					assert.Len(t, config.Labels, len(expected))
					for _, label := range expected {
						key, value, _ := strings.Cut(label, "=")
						assert.Equal(t, value, config.Labels[key])
					}
				}
			}
		})
	}
}

func TestParseEnvfileVariablesWithBOMUnicode(t *testing.T) {
	config, _, _, err := parseRun([]string{"--env-file=testdata/utf8.env"})
	require.NoError(t, err)
	assert.Equal(t, []string{"FOO=BAR", "HELLO=您好", "BAR=FOO"}, config.Env)

	for _, file := range []string{"utf16.env", "utf16be.env"} {
		t.Run(file, func(t *testing.T) {
			_, _, _, err := parseRun([]string{"--env-file=testdata/" + file})
			require.ErrorContains(t, err, "invalid utf8 bytes at line")
		})
	}
}

func TestParseEntryPoint(t *testing.T) {
	config, _, _, err := parseRun([]string{"--entrypoint=anything", "cmd", "img"})
	require.NoError(t, err)
	assert.Equal(t, []string{"anything"}, config.Entrypoint)
}

func TestValidateDevice(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Windows and macOS validate server-side")
	}
	valid := []string{
		"/home",
		"/home:/home",
		"/home:/something/else",
		"/with space",
		"/home:/with space",
		"relative:/absolute-path",
		"hostPath:/containerPath:r",
		"/hostPath:/containerPath:rw",
		"/hostPath:/containerPath:mrw",
	}
	invalid := map[string]string{
		"":        "bad format for path: ",
		"./":      "./ is not an absolute path",
		"../":     "../ is not an absolute path",
		"/:../":   "../ is not an absolute path",
		"/:path":  "path is not an absolute path",
		":":       "bad format for path: :",
		"/tmp:":   " is not an absolute path",
		":test":   "bad format for path: :test",
		":/test":  "bad format for path: :/test",
		"tmp:":    " is not an absolute path",
		":test:":  "bad format for path: :test:",
		"::":      "bad format for path: ::",
		":::":     "bad format for path: :::",
		"/tmp:::": "bad format for path: /tmp:::",
		":/tmp::": "bad format for path: :/tmp::",
		"path:ro": "ro is not an absolute path",
		"path:rr": "rr is not an absolute path",
		"a:/b:ro": "bad mode specified: ro",
		"a:/b:rr": "bad mode specified: rr",
	}

	for _, path := range valid {
		if _, err := validateDevice(path, runtime.GOOS); err != nil {
			t.Fatalf("ValidateDevice(`%q`) should succeed: error %q", path, err)
		}
	}

	for path, expectedError := range invalid {
		if _, err := validateDevice(path, runtime.GOOS); err == nil {
			t.Fatalf("ValidateDevice(`%q`) should have failed validation", path)
		} else if err.Error() != expectedError {
			t.Fatalf("ValidateDevice(`%q`) error should contain %q, got %q", path, expectedError, err.Error())
		}
	}
}

func TestValidateDeviceByServerOS(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		serverOS  string
		want      string
		wantError string
	}{
		{
			name:     "linux preserves three-field container path",
			value:    "/host:/container/../device:rw",
			serverOS: "linux",
			want:     "/host:/container/../device:rw",
		},
		{
			name:     "linux source path can be relative when target is absolute",
			value:    "relative-host:/container/device",
			serverOS: "linux",
			want:     "relative-host:/container/device",
		},
		{
			name:     "windows defers validation",
			value:    `class/GUID`,
			serverOS: "windows",
			want:     `class/GUID`,
		},
		{
			name:      "linux rejects bad mode",
			value:     "/host:/container:ro",
			serverOS:  "linux",
			wantError: "bad mode specified: ro",
		},
		{
			name:      "linux target must be absolute",
			value:     "/host:relative",
			serverOS:  "linux",
			wantError: "relative is not an absolute path",
		},
		{
			name:      "unknown server OS",
			value:     "/dev/snd",
			serverOS:  "plan9",
			wantError: "unknown server OS: plan9",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateDevice(tc.value, tc.serverOS)
			if tc.wantError != "" {
				require.EqualError(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestDeviceCgroupRulesAndInvalidParameter(t *testing.T) {
	got, err := validateDeviceCgroupRule("c 1:3 rwm")
	require.NoError(t, err)
	require.Equal(t, "c 1:3 rwm", got)

	_, err = validateDeviceCgroupRule("invalid")
	require.EqualError(t, err, "invalid device cgroup format 'invalid'")

	if invalidParameter(nil) != nil {
		t.Fatal("invalidParameter(nil) should be nil")
	}
	cause := errors.New("bad input")
	err = invalidParameter(cause)
	var invalid interface{ InvalidParameter() }
	require.ErrorAs(t, err, &invalid)
	require.ErrorIs(t, err, cause)
	require.Condition(t, func() bool { return invalidParameter(err) == err })
}

func TestParseSystemPaths(t *testing.T) {
	tests := []struct {
		doc                       string
		in, out, masked, readonly []string
	}{
		{
			doc: "not set",
			in:  []string{},
			out: []string{},
		},
		{
			doc: "not set, preserve other options",
			in: []string{
				"seccomp=unconfined",
				"apparmor=unconfined",
				"label=user:USER",
				"foo=bar",
			},
			out: []string{
				"seccomp=unconfined",
				"apparmor=unconfined",
				"label=user:USER",
				"foo=bar",
			},
		},
		{
			doc:      "unconfined",
			in:       []string{"systempaths=unconfined"},
			out:      []string{},
			masked:   []string{},
			readonly: []string{},
		},
		{
			doc:      "unconfined and other options",
			in:       []string{"foo=bar", "bar=baz", "systempaths=unconfined"},
			out:      []string{"foo=bar", "bar=baz"},
			masked:   []string{},
			readonly: []string{},
		},
		{
			doc: "unknown option",
			in:  []string{"foo=bar", "systempaths=unknown", "bar=baz"},
			out: []string{"foo=bar", "systempaths=unknown", "bar=baz"},
		},
	}

	for _, tc := range tests {
		securityOpts, maskedPaths, readonlyPaths := parseSystemPaths(tc.in)
		require.Equal(t, tc.out, securityOpts)
		require.Equal(t, tc.masked, maskedPaths)
		require.Equal(t, tc.readonly, readonlyPaths)
	}
}

func TestConvertToStandardNotation(t *testing.T) {
	valid := map[string][]string{
		"20:10/tcp":               {"target=10,published=20"},
		"40:30":                   {"40:30"},
		"20:20 80:4444":           {"20:20", "80:4444"},
		"1500:2500/tcp 1400:1300": {"target=2500,published=1500", "1400:1300"},
		"1500:200/tcp 90:80/tcp":  {"published=1500,target=200", "target=80,published=90"},
	}

	invalid := [][]string{
		{"published=1500,target:444"},
		{"published=1500,444"},
		{"published=1500,target,444"},
	}

	for key, ports := range valid {
		convertedPorts, err := convertToStandardNotation(ports)
		if err != nil {
			require.NoError(t, err)
		}
		require.Equal(t, strings.Split(key, " "), convertedPorts)
	}

	for _, ports := range invalid {
		if _, err := convertToStandardNotation(ports); err == nil {
			t.Fatalf("ConvertToStandardNotation(`%q`) should have failed conversion", ports)
		}
	}
}

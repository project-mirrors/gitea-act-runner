// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	mobyclient "github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsolatedCacheContainer(t *testing.T) {
	ctx := context.Background()
	cache, lan := netip.MustParseAddr("172.18.0.3"), netip.MustParseAddr("192.168.1.20")
	client := &mockDockerClient{}
	client.On("ContainerList", ctx, mobyclient.ContainerListOptions{}).Return(mobyclient.ContainerListResult{Items: []container.Summary{{}, {
		ID: "0123456789abcdef",
		NetworkSettings: &container.NetworkSettingsSummary{Networks: map[string]*network.EndpointSettings{
			"compose": {NetworkID: "c0ffee", IPAddress: cache},
			"lan":     {NetworkID: "beef", IPAddress: lan},
		}},
	}}}, nil)
	for ref, inspected := range map[string]network.Network{
		"c0ffee": {ID: "c0ffee", Name: "compose", Driver: "bridge"},
		"beef":   {ID: "beef", Name: "lan", Driver: "macvlan"},
		"c0f":    {ID: "c0ffee"},
		"jobs":   {ID: "f00d"},
	} {
		client.On("NetworkInspect", ctx, ref, mobyclient.NetworkInspectOptions{}).
			Return(mobyclient.NetworkInspectResult{Network: network.Inspect{Network: inspected}}, nil)
	}
	check := func(addr netip.Addr, jobNetwork, want string) {
		got, err := isolatedCacheContainer(ctx, client, addr, jobNetwork)
		require.NoError(t, err)
		assert.Equal(t, want, got, jobNetwork)
	}
	check(cache, "", "0123456789ab")
	check(cache, "jobs", "0123456789ab")
	check(cache, "c0f", "")
	check(cache, "host", "")
	check(lan, "", "")
	check(netip.MustParseAddr("10.0.0.5"), "", "")
}

func TestIsAddressPoolExhausted(t *testing.T) {
	assert.True(t, isAddressPoolExhausted(cerrdefs.ErrInvalidArgument.WithMessage("Error response from daemon: all predefined address pools have been fully subnetted")))
	assert.True(t, isAddressPoolExhausted(errors.New("could not find an available, non-overlapping IPv4 address pool among the defaults to assign to the network")))
	assert.False(t, isAddressPoolExhausted(cerrdefs.ErrInvalidArgument.WithMessage("invalid subnet 10.0.0.0/8: it overlaps with an existing network")))
}

func (m *mockDockerClient) NetworkConnect(ctx context.Context, id string, opts mobyclient.NetworkConnectOptions) (mobyclient.NetworkConnectResult, error) {
	return mobyclient.NetworkConnectResult{}, m.Called(ctx, id, opts).Error(0)
}

func (m *mockDockerClient) NetworkDisconnect(ctx context.Context, id string, opts mobyclient.NetworkDisconnectOptions) (mobyclient.NetworkDisconnectResult, error) {
	return mobyclient.NetworkDisconnectResult{}, m.Called(ctx, id, opts).Error(0)
}

func TestConnectAndDisconnectNetwork(t *testing.T) {
	ctx := context.Background()
	connect := func(answer error) error {
		client := &mockDockerClient{}
		client.On("NetworkConnect", ctx, "jobs", mobyclient.NetworkConnectOptions{
			Container:      "0123456789ab",
			EndpointConfig: &network.EndpointSettings{Aliases: []string{"0123456789ab"}},
		}).Return(answer).Once()
		return connectNetwork(ctx, client, "jobs", "0123456789ab")
	}
	require.NoError(t, connect(nil))
	require.NoError(t, connect(errors.New("endpoint with name runner already exists in network jobs")))
	require.NoError(t, connect(errors.New("container runner already connected to network jobs")))
	require.ErrorIs(t, connect(context.Canceled), context.Canceled)

	disconnect := func(answer error) error {
		client := &mockDockerClient{}
		client.On("NetworkDisconnect", ctx, "jobs", mobyclient.NetworkDisconnectOptions{
			Container: "0123456789ab", Force: true,
		}).Return(answer).Once()
		return disconnectNetwork(ctx, client, "jobs", "0123456789ab")
	}
	require.NoError(t, disconnect(cerrdefs.ErrNotFound))
	require.NoError(t, disconnect(errors.New("container runner is not connected to the network jobs")))
	require.ErrorIs(t, disconnect(context.Canceled), context.Canceled)
}

// Of this runner's networks, only the ones nothing but its own cache container is attached to
// and old enough to predate any job now starting are the runner's to reclaim. An unexpected
// NetworkRemove fails the test on its own, since testify has no expectation to match it against.
func TestRemoveOrphanNetworks(t *testing.T) {
	ctx := context.Background()
	cutoff := time.Date(2026, time.April, 29, 20, 0, 0, 0, time.UTC)
	client := &mockDockerClient{}
	client.On("NetworkList", ctx, mobyclient.NetworkListOptions{
		Filters: make(mobyclient.Filters).Add("label", runnerUUIDLabel+"=runner-1"),
	}).Return(mobyclient.NetworkListResult{Items: []network.Summary{
		{ID: "orphan"},
		{ID: "busy"},
		{ID: "starting"},
		{ID: "cached"},
		{ID: "shared"},
	}}, nil)
	client.On("NetworkInspect", ctx, "orphan", mobyclient.NetworkInspectOptions{}).
		Return(mobyclient.NetworkInspectResult{}, nil)
	client.On("NetworkInspect", ctx, "busy", mobyclient.NetworkInspectOptions{}).
		Return(mobyclient.NetworkInspectResult{Network: network.Inspect{Containers: map[string]network.EndpointResource{"c": {}}}}, nil)
	client.On("NetworkInspect", ctx, "starting", mobyclient.NetworkInspectOptions{}).
		Return(mobyclient.NetworkInspectResult{Network: network.Inspect{
			Network:    network.Network{Created: cutoff.Add(time.Second)},
			Containers: map[string]network.EndpointResource{"0123456789abcdef": {}},
		}}, nil)
	client.On("NetworkInspect", ctx, "cached", mobyclient.NetworkInspectOptions{}).
		Return(mobyclient.NetworkInspectResult{Network: network.Inspect{Containers: map[string]network.EndpointResource{"0123456789abcdef": {}}}}, nil)
	client.On("NetworkInspect", ctx, "shared", mobyclient.NetworkInspectOptions{}).
		Return(mobyclient.NetworkInspectResult{Network: network.Inspect{Containers: map[string]network.EndpointResource{"0123456789abcdef": {}, "job": {}}}}, nil)
	client.On("NetworkRemove", ctx, "orphan", mobyclient.NetworkRemoveOptions{}).
		Return(mobyclient.NetworkRemoveResult{}, nil)
	client.On("NetworkDisconnect", ctx, "cached", mobyclient.NetworkDisconnectOptions{Container: "0123456789abcdef", Force: true}).
		Return(nil).Once()
	client.On("NetworkRemove", ctx, "cached", mobyclient.NetworkRemoveOptions{}).
		Return(mobyclient.NetworkRemoveResult{}, nil).Once()

	require.NoError(t, removeOrphanNetworks(ctx, client, "runner-1", "0123456789ab", cutoff))
	client.AssertExpectations(t)
}

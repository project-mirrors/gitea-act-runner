// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package common

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPreferredIP(t *testing.T) {
	assert.Nil(t, preferredIP(nil))
	assert.Equal(t, net.ParseIP("2001:db8::1"), preferredIP([]interfaceIP{{IP: net.ParseIP("2001:db8::1")}}))
	assert.Equal(t, net.ParseIP("2001:db8::1"), preferredIP([]interfaceIP{
		{IP: net.ParseIP("192.0.2.1"), Name: "wlan0"},
		{IP: net.ParseIP("2001:db8::1"), Name: "eth0"},
	}))
}

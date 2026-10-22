// Copyright 2023 The Gitea Authors. All rights reserved.
// Copyright 2021 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package common

import (
	"net"
	"sort"
	"strings"
)

type interfaceIP struct {
	net.IP
	Name string
}

// GetOutboundIP returns an outbound IP address of this machine.
// It tries to access the internet and returns the local IP address of the connection.
// If the machine cannot access the internet, it returns a preferred IP address from network interfaces.
// It returns nil if no IP address is found.
func GetOutboundIP() net.IP {
	// See https://stackoverflow.com/a/37382208
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err == nil {
		defer conn.Close()
		if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
			return addr.IP
		}
	}

	// So the machine cannot access the internet. Pick an IP address from network interfaces.
	if ifs, err := net.Interfaces(); err == nil {
		var ips []interfaceIP
		for _, i := range ifs {
			if addrs, err := i.Addrs(); err == nil {
				for _, addr := range addrs {
					var ip net.IP
					switch v := addr.(type) {
					case *net.IPNet:
						ip = v.IP
					case *net.IPAddr:
						ip = v.IP
					}
					if ip.IsGlobalUnicast() {
						ips = append(ips, interfaceIP{ip, i.Name})
					}
				}
			}
		}
		return preferredIP(ips)
	}

	return nil
}

func preferredIP(ips []interfaceIP) net.IP {
	if len(ips) == 0 {
		return nil
	}
	sort.Slice(ips, func(i, j int) bool {
		first, second := ips[i], ips[j]

		// ethernet is preferred
		if vi, vj := strings.HasPrefix(first.Name, "e"), strings.HasPrefix(second.Name, "e"); vi != vj {
			return vi
		}

		// IPv4 is preferred
		if vi, vj := first.To4() != nil, second.To4() != nil; vi != vj {
			return vi
		}

		// en0 is preferred to en1
		if first.Name != second.Name {
			return first.Name < second.Name
		}

		// fallback
		return first.String() < second.String()
	})
	return ips[0].IP
}

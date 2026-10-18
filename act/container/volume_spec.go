// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/moby/moby/api/types/mount"
)

type VolumeSpec struct {
	Type     mount.Type
	Source   string
	Target   string
	ReadOnly bool
	NoCopy   bool
}

// ParseVolume parses spec like `docker run -v` does, without knowing the target platform.
func ParseVolume(spec string) (VolumeSpec, error) {
	switch len(spec) {
	case 0:
		return VolumeSpec{}, errors.New("invalid empty volume spec")
	case 1, 2:
		return VolumeSpec{Type: mount.TypeVolume, Target: spec}, nil
	}

	spec = string([]rune(spec)) // each invalid UTF-8 byte becomes U+FFFD, as in the JSON the daemon receives
	var parts []string
	for rest := spec; ; {
		start := 0
		if hasDriveLetter(rest) {
			start = strings.IndexByte(rest, ':') + 1
		}
		end := strings.IndexByte(rest[start:], ':')
		if end < 0 {
			parts = append(parts, rest)
			break
		}
		parts = append(parts, rest[:start+end])
		rest = rest[start+end+1:]
	}
	if len(parts) > 3 {
		return VolumeSpec{}, fmt.Errorf("invalid spec: %s: too many colons", spec)
	}
	if slices.Contains(parts, "") {
		return VolumeSpec{}, fmt.Errorf("invalid spec: %s: empty section between colons", spec)
	}

	volume := VolumeSpec{Type: mount.TypeVolume, Target: parts[0]}
	if len(parts) > 1 {
		volume.Source, volume.Target = parts[0], parts[1]
		if isHostPath(volume.Source) {
			volume.Type = mount.TypeBind
		}
	}
	if len(parts) == 3 {
		for option := range strings.SplitSeq(parts[2], ",") {
			switch option {
			case "ro":
				volume.ReadOnly = true
			case "rw":
				volume.ReadOnly = false
			case "nocopy":
				volume.NoCopy = true
			}
		}
	}
	return volume, nil
}

func hasDriveLetter(s string) bool {
	letter, size := utf8.DecodeRuneInString(s)
	return size < len(s) && s[size] == ':' && unicode.IsLetter(letter)
}

func isHostPath(source string) bool {
	return strings.ContainsAny(source[:1], "./~") || strings.HasPrefix(source, `\\`) || hasDriveLetter(source)
}

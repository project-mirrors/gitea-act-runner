// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package labels

import (
	"errors"
	"strings"
)

const (
	SchemeHost   = "host"
	SchemeDocker = "docker"

	// SelfHostedPlatform is the platform marker act treats as "run on the host".
	SelfHostedPlatform = "-self-hosted"
)

type Label struct {
	Name   string
	Schema string
	Arg    string
	// Opaque marks a label whose name contains a colon but no supported schema,
	// like "pool:e57e18d4-...". It is kept verbatim and behaves like a host label.
	Opaque bool
}

func Parse(str string) (*Label, error) {
	if str == "" {
		return nil, errors.New("empty label")
	}

	splits := strings.SplitN(str, ":", 3)
	label := &Label{
		Name:   splits[0],
		Schema: SchemeHost,
		Arg:    "",
	}
	if len(splits) >= 2 {
		label.Schema = splits[1]
	}
	if len(splits) >= 3 {
		label.Arg = splits[2]
	}
	if label.Schema != SchemeHost && label.Schema != SchemeDocker {
		// Not a schema we know: the colon belongs to the label name itself.
		return &Label{
			Name:   str,
			Schema: SchemeHost,
			Opaque: true,
		}, nil
	}
	return label, nil
}

type Labels []*Label

func (l Labels) RequireDocker() bool {
	for _, label := range l {
		if label.Schema == SchemeDocker {
			return true
		}
	}
	return false
}

// PickPlatform returns the platform of the first runs-on entry this runner has a label for, or "".
func (l Labels) PickPlatform(runsOn []string) string {
	platforms := make(map[string]string, len(l))
	for _, label := range l {
		switch label.Schema {
		case SchemeDocker:
			// "//" will be ignored
			platforms[label.Name] = strings.TrimPrefix(label.Arg, "//")
		case SchemeHost:
			platforms[label.Name] = SelfHostedPlatform
		default:
			// unreachable: Parse only produces host or docker schemas
			continue
		}
	}
	for _, v := range runsOn {
		if v, ok := platforms[v]; ok {
			return v
		}
	}
	return ""
}

func (l Labels) Names() []string {
	names := make([]string, 0, len(l))
	for _, label := range l {
		names = append(names, label.Name)
	}
	return names
}

func (l Labels) ToStrings() []string {
	ls := make([]string, 0, len(l))
	for _, label := range l {
		lbl := label.Name
		if !label.Opaque && label.Schema != "" {
			lbl += ":" + label.Schema
			if label.Arg != "" {
				lbl += ":" + label.Arg
			}
		}
		ls = append(ls, lbl)
	}
	return ls
}

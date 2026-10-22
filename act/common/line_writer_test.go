// Copyright 2020 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package common

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLineWriterBuffersAndFlushesLines(t *testing.T) {
	var lines []string
	lineWriter := NewLineWriter(func(s string) bool {
		lines = append(lines, s)
		return true
	})

	assert := assert.New(t)
	write := func(s string) {
		n, err := lineWriter.Write([]byte(s))
		require.NoError(t, err)
		assert.Equal(len(s), n, s)
	}

	write("")
	write("hello")
	write(" ")
	write("world!!\nextra")
	write(" line\n and another\nlast")
	write(" line\n")
	write("no newline here...")

	expected := []string{"hello world!!\n", "extra line\n", " and another\n", "last line\n"}
	assert.Equal(expected, lines)

	FlushWriter(lineWriter)
	expected = append(expected, "no newline here...")
	assert.Equal(expected, lines)

	FlushWriter(lineWriter)
	assert.Equal(expected, lines)
}

func TestFlushWriterIgnoresNonFlusher(t *testing.T) {
	assert.NotPanics(t, func() { FlushWriter(io.Discard) })
}

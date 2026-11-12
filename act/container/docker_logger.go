// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd || freebsd))

package container

import (
	"bufio"
	"cmp"
	"encoding/json/v2"
	"errors"
	"io"

	"gitea.com/gitea/runner/act/common/logging"
)

type dockerMessage struct {
	ID          string `json:"id"`
	Stream      string `json:"stream"`
	Error       string `json:"error"`
	ErrorDetail struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
	Status   string `json:"status"`
	Progress string `json:"progress"`
}

func logDockerResponse(logger *logging.Logger, dockerResponse io.ReadCloser, log func(string, ...any)) error {
	defer dockerResponse.Close()

	scanner := bufio.NewScanner(dockerResponse)
	scanner.Buffer(nil, 64*1024*1024) // a build echoes long instructions in one record
	for scanner.Scan() {
		line := scanner.Bytes()
		var msg dockerMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			logger.Debugf("Unable to unmarshal line [%s] ==> %v", string(line), err)
			continue
		}

		if message := cmp.Or(msg.Error, msg.ErrorDetail.Message); message != "" {
			log("%s", message)
			return errors.New(message)
		}

		switch {
		case msg.Status != "":
			if msg.Progress != "" {
				log("%s :: %s :: %s\n", msg.Status, msg.ID, msg.Progress)
			} else {
				log("%s :: %s\n", msg.Status, msg.ID)
			}
		case msg.Stream != "":
			log("%s", msg.Stream)
		default:
			logger.Debugf("Unable to handle line: %s", string(line))
		}
	}

	return scanner.Err()
}

// Copyright 2023 The Gitea Authors. All rights reserved.
// Copyright 2023 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifactcache

type Request struct {
	Key     string `json:"key" `
	Version string `json:"version"`
	Size    int64  `json:"cacheSize"`
}

type Cache struct {
	ID        uint64 `json:"id"`
	Repo      string `json:"repo"`
	Key       string `json:"key"`
	Version   string `json:"version"`
	Size      int64  `json:"cacheSize"`
	Owner     string `json:"owner"` // hashed bearer, so a job's own retry is not another job's reservation
	Complete  bool   `json:"complete"`
	UsedAt    int64  `json:"usedAt"`
	CreatedAt int64  `json:"createdAt"`
}

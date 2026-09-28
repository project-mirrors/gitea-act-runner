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
	ID        uint64 `json:"id" boltholdKey:"ID"`
	Repo      string `json:"repo" boltholdIndex:"Repo"`
	Key       string `json:"key" boltholdIndex:"Key"`
	Version   string `json:"version" boltholdIndex:"Version"`
	Size      int64  `json:"cacheSize"`
	Owner     string `json:"owner"` // hashed bearer, so a job's own retry is not another job's reservation
	Complete  bool   `json:"complete" boltholdIndex:"Complete"`
	UsedAt    int64  `json:"usedAt" boltholdIndex:"UsedAt"`
	CreatedAt int64  `json:"createdAt" boltholdIndex:"CreatedAt"`
}

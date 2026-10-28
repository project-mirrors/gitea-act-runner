// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifacts

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json/v2"
	"time"
)

func LocalRuntimeToken(run string, lifetime time.Duration) string { // an unsecured JWT with the claims the toolkit and buildx read, its run hashed for scp and its jti an unguessable bearer
	now := time.Now()
	claims, _ := json.Marshal(map[string]any{
		"scp": "Actions.Results:" + fileName(run) + ":1",
		"ac":  `[{"Scope":"","Permission":3}]`,
		"nbf": now.Unix(),
		"exp": now.Add(lifetime).Unix(),
		"jti": rand.Text(),
	})
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(claims) + "."
}

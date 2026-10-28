// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifacts

import "time"

const artifactServicePath = "/twirp/github.actions.results.api.v1.ArtifactService"

type ( // the ArtifactService messages in the protobuf JSON mapping, which writes int64 as strings
	createArtifactResponse struct {
		Ok              bool   `json:"ok"`
		SignedUploadURL string `json:"signedUploadUrl"`
	}

	finalizeArtifactRequest struct {
		WorkflowRunBackendID string
		Name                 string
		Size                 int64 `json:",string"`
		Hash                 string
	}
	finalizeArtifactResponse struct {
		Ok         bool  `json:"ok"`
		ArtifactID int64 `json:"artifactId,string"`
	}

	listArtifactsRequest struct {
		WorkflowRunBackendID string
		NameFilter           string
		IDFilter             int64 `json:",string"`
	}
	listArtifactsResponse struct {
		Artifacts []monolithArtifact `json:"artifacts"`
	}
	monolithArtifact struct {
		WorkflowRunBackendID string    `json:"workflowRunBackendId"` // the toolkit sends it back to GetSignedArtifactURL
		DatabaseID           int64     `json:"databaseId,string"`
		Name                 string    `json:"name"`
		Size                 int64     `json:"size,string"`
		CreatedAt            time.Time `json:"createdAt"` // whole seconds, the toolkit rejects the trimmed fractions json writes
		Digest               string    `json:"digest"`    // empty until the upload is finalized
	}

	getSignedArtifactURLRequest struct {
		WorkflowRunBackendID string
		Name                 string
	}
	getSignedArtifactURLResponse struct {
		SignedURL string `json:"signedUrl"`
	}

	createArtifactRequest  = getSignedArtifactURLRequest
	deleteArtifactRequest  = getSignedArtifactURLRequest
	deleteArtifactResponse = finalizeArtifactResponse
)

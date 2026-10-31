// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cmd

import (
	"gitea.com/gitea/runner/act/container"

	"github.com/spf13/cobra"
)

// loadKubernetesTransferCmd is what job pods run when kubernetes.transfer is on: the init container installs the binary, the job container fetches with it.
func loadKubernetesTransferCmd() *cobra.Command {
	transferCmd := &cobra.Command{
		Use:    container.KubernetesTransferCommand,
		Short:  "Copy files from the runner into a Kubernetes job pod",
		Hidden: true,
	}
	transferCmd.AddCommand(&cobra.Command{
		Use:   "install <path>",
		Short: "Copy this binary to path",
		Args:  cobra.ExactArgs(1),
		RunE: func(_cmd *cobra.Command, args []string) error {
			return container.InstallKubernetesTransferHelper(args[0])
		},
	}, &cobra.Command{
		Use:   "fetch <dir>",
		Short: "Extract the tar stream the transfer request on stdin points to into dir",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return container.FetchKubernetesTransfer(cmd.Context(), cmd.InOrStdin(), args[0])
		},
	})
	return transferCmd
}

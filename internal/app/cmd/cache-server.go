// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cmd

import (
	"cmp"
	"errors"
	"fmt"

	"gitea.com/gitea/runner/act/artifactcache"
	"gitea.com/gitea/runner/act/common/logging"
	"gitea.com/gitea/runner/internal/app/run"
	"gitea.com/gitea/runner/internal/pkg/config"

	"github.com/spf13/cobra"
)

type cacheServerArgs struct {
	Dir  string
	Host string
	Port uint16
}

func runCacheServer(configFile *string, cacheArgs *cacheServerArgs) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		cfg, err := config.LoadDefault(*configFile)
		if err != nil {
			return fmt.Errorf("invalid configuration: %w", err)
		}

		initLogging(cfg)
		if cfg.Cache.S3 != nil {
			return errors.New("cache-server does not support cache.s3; set cache.s3 on the runners instead")
		}

		if cfg.Cache.ExternalSecret == "" {
			return errors.New("cache.external_secret (or cache.external_secret_file) must be set for cache-server; configure the same value on each runner that points at this server via cache.external_server")
		}
		cacheHandler, err := artifactcache.StartHandler(artifactcache.Options{
			Dir:            cmp.Or(cacheArgs.Dir, cfg.Cache.Dir),
			OutboundIP:     cmp.Or(cacheArgs.Host, cfg.Cache.Host),
			Port:           cmp.Or(cacheArgs.Port, cfg.Cache.Port),
			InternalSecret: cfg.Cache.ExternalSecret,
			Policy:         run.CachePolicy(cfg),
			Logger:         logging.Default().With("module", "cache_request"),
		})
		if err != nil {
			return err
		}

		logging.Infof("cache server is listening on %v", cacheHandler.ExternalURL())

		<-cmd.Context().Done()

		return cacheHandler.Close()
	}
}

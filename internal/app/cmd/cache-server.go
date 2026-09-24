// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cmd

import (
	"errors"
	"fmt"

	"gitea.com/gitea/runner/act/artifactcache"
	"gitea.com/gitea/runner/internal/app/run"
	"gitea.com/gitea/runner/internal/pkg/config"

	log "github.com/sirupsen/logrus"
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

		var (
			dir  = cfg.Cache.Dir
			host = cfg.Cache.Host
			port = cfg.Cache.Port
		)

		// cacheArgs has higher priority
		if cacheArgs.Dir != "" {
			dir = cacheArgs.Dir
		}
		if cacheArgs.Host != "" {
			host = cacheArgs.Host
		}
		if cacheArgs.Port != 0 {
			port = cacheArgs.Port
		}

		secret := cfg.Cache.ExternalSecret
		if secret == "" {
			return errors.New("cache.external_secret (or cache.external_secret_file) must be set for cache-server; configure the same value on each runner that points at this server via cache.external_server")
		}
		cacheHandler, err := artifactcache.StartHandler(artifactcache.Options{
			Dir:            dir,
			OutboundIP:     host,
			Port:           port,
			InternalSecret: secret,
			Policy:         run.CachePolicy(cfg),
			Logger:         log.StandardLogger().WithField("module", "cache_request"),
		})
		if err != nil {
			return err
		}

		log.Infof("cache server is listening on %v", cacheHandler.ExternalURL())

		<-cmd.Context().Done()

		return cacheHandler.Close()
	}
}

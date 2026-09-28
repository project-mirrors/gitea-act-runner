// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gitea.com/gitea/runner/internal/pkg/config"

	"github.com/spf13/cobra"
)

func loadConfigCmd(configFile *string) *cobra.Command {
	configCmd := &cobra.Command{
		Use:   "config",
		Short: "Generate, read and edit config files",
		Args:  cobra.MaximumNArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	configCmd.AddCommand(loadGenerateConfigCmd("generate"))
	configCmd.AddCommand(loadInitConfigCmd(configFile))

	configCmd.AddCommand(&cobra.Command{
		Use:   "get <key>",
		Short: "Print the value of a config key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			file, err := resolveConfigFile(cmd, configFile)
			if err != nil {
				return err
			}
			value, err := config.GetValue(file, args[0])
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), value)
			return nil
		},
	})

	for _, sub := range []struct {
		use   string
		short string
		edit  func(file, key string, values ...string) error
	}{
		{"set <key> <value>...", "Set the value of a config key", config.SetValue},
		{"add <key> <value>...", "Append values to a list config key", config.AddValue},
		{"remove <key> <value>...", "Remove values from a list config key", config.RemoveValue},
	} {
		valueCmd := &cobra.Command{
			Use:   sub.use,
			Short: sub.short,
			Args:  cobra.MinimumNArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				file, err := resolveConfigFile(cmd, configFile)
				if err != nil {
					return err
				}
				return sub.edit(file, args[0], args[1:]...)
			},
		}
		valueCmd.Flags().SetInterspersed(false) // so a value such as `--cpus 2` is not parsed as a flag
		configCmd.AddCommand(valueCmd)
	}

	return configCmd
}

func loadInitConfigCmd(configFile *string) *cobra.Command {
	var force bool
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Write a minimal config file",
		Long:  "Write a minimal config file, leaving every option at its default.\nWithout --config it writes config.yaml in the working directory.",
		Args:  cobra.MaximumNArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			file, taken := *configFile, []string{*configFile}
			if file == "" {
				file, taken = defaultConfigFileNames[0], defaultConfigFileNames // any of them would shadow the new file
			}
			for _, name := range taken {
				if _, err := os.Stat(name); err == nil && !force {
					return fmt.Errorf("config file %q already exists, pass --force to overwrite it", name)
				}
			}
			if err := config.WriteFile(file, []byte(config.Minimal)); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote config file %q\n", file)
			return nil
		},
	}
	initCmd.Flags().BoolVarP(&force, "force", "f", false, "overwrite an existing config file")
	return initCmd
}

func loadGenerateConfigCmd(use string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: "Print the example config, which documents every option",
		Args:  cobra.MaximumNArgs(0),
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "%s", config.Example)
		},
	}
}

var defaultConfigFileNames = []string{"config.yaml", "config.yml"}

func resolveConfigFile(cmd *cobra.Command, configFile *string) (string, error) {
	if *configFile != "" {
		return *configFile, nil
	}

	var dirs []string
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	if exe, err := os.Executable(); err == nil {
		if dir := filepath.Dir(exe); !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}

	for _, dir := range dirs {
		for _, name := range defaultConfigFileNames {
			candidate := filepath.Join(dir, name)
			if stat, err := os.Stat(candidate); err == nil && !stat.IsDir() {
				fmt.Fprintf(cmd.ErrOrStderr(), "using config file %q\n", candidate)
				return candidate, nil
			}
		}
	}

	return "", fmt.Errorf("no %s found in %s, pass one with --config",
		strings.Join(defaultConfigFileNames, " or "), strings.Join(dirs, " or "))
}

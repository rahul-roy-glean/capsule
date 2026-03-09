package main

import "github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"

func (c *Config) ToLayeredConfig() *snapshot.LayeredConfig {
	cfg := &snapshot.LayeredConfig{
		BaseImage: c.Workload.BaseImage,
		Layers:    c.materializedLayers(),
	}
	cfg.DisplayName = c.Platform.GCPProject + "-" + c.Platform.Environment
	if cfg.DisplayName == "-" {
		cfg.DisplayName = "firecracker-workload"
	}

	if c.Workload.Config.Tier != "" {
		cfg.Config.Tier = c.Workload.Config.Tier
	}
	if c.Workload.Config.AutoPause != nil {
		cfg.Config.AutoPause = *c.Workload.Config.AutoPause
	} else if c.Session.Enabled {
		cfg.Config.AutoPause = c.Session.AutoPause
	}
	if c.Workload.Config.TTL > 0 {
		cfg.Config.TTL = c.Workload.Config.TTL
	} else if c.Session.TTLSeconds > 0 {
		cfg.Config.TTL = c.Session.TTLSeconds
	}
	if c.Workload.Config.AutoRollout != nil {
		cfg.Config.AutoRollout = *c.Workload.Config.AutoRollout
	} else {
		cfg.Config.AutoRollout = true
	}
	if c.Workload.Config.SessionMaxAgeSeconds > 0 {
		cfg.Config.SessionMaxAgeSeconds = c.Workload.Config.SessionMaxAgeSeconds
	} else if c.Session.Enabled {
		cfg.Config.SessionMaxAgeSeconds = 86400
	}
	cfg.Config.RootfsSizeGB = c.Workload.Config.RootfsSizeGB
	cfg.Config.RunnerUser = c.Workload.Config.RunnerUser
	cfg.Config.WorkspaceSizeGB = c.Workload.Config.WorkspaceSizeGB
	cfg.Config.NetworkPolicyPreset = c.Workload.Config.NetworkPolicyPreset
	cfg.Config.NetworkPolicy = c.Workload.Config.NetworkPolicy
	cfg.Config.Auth = c.Workload.Config.Auth

	if len(c.Workload.StartCommand.Command) > 0 {
		cfg.StartCommand = &snapshot.StartCommand{
			Command:    c.Workload.StartCommand.Command,
			Port:       c.Workload.StartCommand.Port,
			HealthPath: c.Workload.StartCommand.HealthPath,
			Env:        c.Workload.StartCommand.Env,
			RunAs:      c.Workload.StartCommand.RunAs,
		}
	}

	return cfg
}

func (c *Config) materializedLayers() []snapshot.LayerDef {
	if len(c.Workload.Layers) > 0 {
		return c.Workload.Layers
	}
	if len(c.Workload.SnapshotCommands) == 0 {
		return nil
	}

	cmds := make([]snapshot.SnapshotCommand, 0, len(c.Workload.SnapshotCommands))
	for _, cmd := range c.Workload.SnapshotCommands {
		cmds = append(cmds, snapshot.SnapshotCommand{
			Type:      cmd.Type,
			Args:      cmd.Args,
			RunAsRoot: cmd.RunAsRoot,
		})
	}
	return []snapshot.LayerDef{{
		Name:         "workload",
		InitCommands: cmds,
	}}
}

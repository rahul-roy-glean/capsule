package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

var (
	configPath = flag.String("config", "onboard.yaml", "Path to onboard configuration file")
	stepsFlag  = flag.String("steps", "", "Comma-separated list of steps to run (default: all)")
	dryRun     = flag.Bool("dry-run", false, "Validate configuration without executing steps")
	logLevel   = flag.String("log-level", "info", "Log level (debug, info, warn, error)")
	verbose    = flag.Bool("verbose", false, "Enable verbose output")
)

func main() {
	flag.Parse()

	// Setup logger
	logger := logrus.New()
	logger.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "15:04:05",
	})
	level, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		level = logrus.InfoLevel
	}
	if *verbose {
		level = logrus.DebugLevel
	}
	logger.SetLevel(level)
	log := logger.WithField("component", "onboard")

	log.Info("=== Bazel Firecracker Onboard ===")

	// Load configuration
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.WithError(err).Fatal("Failed to load configuration")
	}
	log.WithField("config", *configPath).Info("Configuration loaded")

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		log.WithError(err).Fatal("Configuration validation failed")
	}
	log.Info("Configuration validated successfully")

	if *dryRun {
		log.Info("Dry run mode - configuration is valid, no steps will be executed")
		fmt.Println("\nConfiguration summary:")
		fmt.Printf("  GCP Project:  %s\n", cfg.Platform.GCPProject)
		fmt.Printf("  Region:       %s\n", cfg.Platform.Region)
		fmt.Printf("  Zone:         %s\n", cfg.Platform.Zone)
		fmt.Printf("  Repository:   %s\n", cfg.Repository.URL)
		fmt.Printf("  CI System:    %s\n", cfg.CI.System)
		fmt.Printf("  Max Hosts:    %d\n", cfg.Hosts.MaxCount)
		fmt.Printf("  Max VMs/Host: %d\n", cfg.MicroVM.MaxPerHost)
		os.Exit(0)
	}

	// Determine which steps to run
	allSteps := GetAllSteps()
	var stepsToRun []Step
	if *stepsFlag != "" {
		stepNames := strings.Split(*stepsFlag, ",")
		for _, name := range stepNames {
			name = strings.TrimSpace(name)
			step, ok := GetStepByName(allSteps, name)
			if !ok {
				log.WithField("step", name).Fatal("Unknown step")
			}
			stepsToRun = append(stepsToRun, step)
		}
	} else {
		stepsToRun = allSteps
	}

	// Execute steps
	log.WithField("steps", len(stepsToRun)).Info("Starting onboard steps")
	startTime := time.Now()

	for i, step := range stepsToRun {
		stepLog := log.WithFields(logrus.Fields{
			"step":     step.Name,
			"progress": fmt.Sprintf("[%d/%d]", i+1, len(stepsToRun)),
		})
		stepLog.Infof("Starting: %s", step.Description)

		stepStart := time.Now()
		if err := step.Run(cfg, logger); err != nil {
			stepLog.WithError(err).Error("Step failed")
			os.Exit(1)
		}

		stepLog.WithField("duration", time.Since(stepStart).Round(time.Second)).Info("Step completed")
	}

	log.WithField("total_duration", time.Since(startTime).Round(time.Second)).Info("Onboard completed successfully!")
}

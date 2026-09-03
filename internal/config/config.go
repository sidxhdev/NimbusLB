// Package config loads YAML configuration and applies CLI flag overrides.
package config

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds all runtime settings for the load balancer.
type Config struct {
	Port           int           `yaml:"port"`
	MetricsPort    int           `yaml:"metrics_port"`
	HealthInterval time.Duration `yaml:"health_interval"`
	HealthTimeout  time.Duration `yaml:"health_timeout"`
	Retries        int           `yaml:"retries"`
	Backends       []string      `yaml:"backends"`
	OTLPEndpoint   string        `yaml:"otel_endpoint"`
}

// Load reads the YAML file at path (if non-empty), then applies any CLI flag
// overrides. Defaults are applied first, so partial configs are valid.
func Load() (*Config, error) {
	var (
		configPath  = flag.String("config", "config.yaml", "path to YAML config file (optional)")
		port        = flag.Int("port", 0, "listener port for proxied traffic")
		metricsPort = flag.Int("metrics-port", 0, "listener port for /metrics")
		backends    = flag.String("backends", "", "comma-separated backend URLs")
		hcInterval  = flag.Duration("hc-interval", 0, "health check interval")
		retries     = flag.Int("retries", 0, "failover retry attempts per request")
	)
	flag.Parse()

	cfg := &Config{
		Port:           8080,
		MetricsPort:    9090,
		HealthInterval: 10 * time.Second,
		HealthTimeout:  5 * time.Second,
		Retries:        3,
	}

	if *configPath != "" {
		if data, err := os.ReadFile(*configPath); err == nil {
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parsing %s: %w", *configPath, err)
			}
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("reading %s: %w", *configPath, err)
		}
	}

	// CLI overrides (only when explicitly set).
	if *port != 0 {
		cfg.Port = *port
	}
	if *metricsPort != 0 {
		cfg.MetricsPort = *metricsPort
	}
	if *backends != "" {
		cfg.Backends = strings.Split(*backends, ",")
	}
	if *hcInterval != 0 {
		cfg.HealthInterval = *hcInterval
	}
	if *retries != 0 {
		cfg.Retries = *retries
	}

	if len(cfg.Backends) == 0 {
		return nil, fmt.Errorf("no backends configured (set backends in config.yaml or pass -backends)")
	}
	return cfg, nil
}

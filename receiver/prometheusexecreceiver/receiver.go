// Copyright 2020, OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prometheusexecreceiver

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/config"
	sdconfig "github.com/prometheus/prometheus/discovery/config"
	"github.com/prometheus/prometheus/discovery/targetgroup"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configmodels"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver/prometheusreceiver"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/prometheusexecreceiver/subprocessmanager"
)

const (
	// template for port in strings
	portTemplate string = "{{port}}"
	// healthyProcessTime is the default time a process needs to stay alive to be considered healthy
	healthyProcessTime time.Duration = 30 * time.Minute
	// healthyCrashCount is the amount of times a process can crash (within the healthyProcessTime) before being considered unstable - it may be trying to find a port
	healthyCrashCount int = 3
	// delayMutiplier is the factor by which the delay scales
	delayMultiplier float64 = 2.0
	// initialDelay is the initial delay before a process is restarted
	initialDelay time.Duration = 1 * time.Second
	// default path to scrape metrics at endpoint
	defaultMetricsPath = "/metrics"
	// defaul timeout for a scrape
	defaultScrapeTimeout = 10 * time.Second
)

type prometheusExecReceiver struct {
	params   component.ReceiverCreateParams
	config   *Config
	consumer consumer.MetricsConsumer

	// Prometheus receiver config
	promReceiverConfig *prometheusreceiver.Config

	// Subprocess data
	subprocessConfig *subprocessmanager.SubprocessConfig
	port             int

	// Underlying receiver data
	prometheusReceiver component.MetricsReceiver

	// Shutdown channel
	shutdownCh chan struct{}
}

type runResult struct {
	elapsed       time.Duration
	subprocessErr error
}

// new returns a prometheusExecReceiver
func new(params component.ReceiverCreateParams, config *Config, consumer consumer.MetricsConsumer) (*prometheusExecReceiver, error) {
	if config.SubprocessConfig.Command == "" {
		return nil, fmt.Errorf("no command to execute entered in config file for %v", config.Name())
	}
	subprocessConfig := getSubprocessConfig(config)
	promReceiverConfig := getPromReceiverConfig(config)

	return &prometheusExecReceiver{
		params:             params,
		config:             config,
		consumer:           consumer,
		subprocessConfig:   subprocessConfig,
		promReceiverConfig: promReceiverConfig,
		port:               config.Port,
	}, nil
}

// getPromReceiverConfig returns the Prometheus receiver config
func getPromReceiverConfig(cfg *Config) *prometheusreceiver.Config {
	scrapeConfig := &config.ScrapeConfig{}

	scrapeConfig.ScrapeInterval = model.Duration(cfg.ScrapeInterval)
	scrapeConfig.ScrapeTimeout = model.Duration(defaultScrapeTimeout)
	scrapeConfig.Scheme = "http"
	scrapeConfig.MetricsPath = defaultMetricsPath
	scrapeConfig.JobName = extractName(cfg)
	scrapeConfig.HonorLabels = false
	scrapeConfig.HonorTimestamps = true

	// Set the proper target by creating one target inside a single target group (this is how Prometheus wants its scrape config)
	scrapeConfig.ServiceDiscoveryConfig = sdconfig.ServiceDiscoveryConfig{
		StaticConfigs: []*targetgroup.Group{
			{
				Targets: []model.LabelSet{
					{model.AddressLabel: model.LabelValue(fmt.Sprintf("localhost:%v", cfg.Port))},
				},
			},
		},
	}

	receiverSettings := &configmodels.ReceiverSettings{
		TypeVal: typeStr,
		NameVal: cfg.Name(),
	}

	return &prometheusreceiver.Config{
		ReceiverSettings: *receiverSettings,
		PrometheusConfig: &config.Config{
			ScrapeConfigs: []*config.ScrapeConfig{scrapeConfig},
		},
	}
}

// getSubprocessConfig returns the subprocess config
func getSubprocessConfig(cfg *Config) *subprocessmanager.SubprocessConfig {
	subprocessConfig := &subprocessmanager.SubprocessConfig{}

	subprocessConfig.Command = cfg.SubprocessConfig.Command
	subprocessConfig.Env = cfg.SubprocessConfig.Env

	return subprocessConfig
}

// extractName will return the receiver's given custom name (prometheus_exec/custom_name)
func extractName(cfg *Config) string {
	splitName := strings.SplitN(cfg.Name(), "/", 2)
	if len(splitName) > 1 && splitName[1] != "" {
		return splitName[1]
	}
	// fall back to the first part of the string, prometheus_exec
	return splitName[0]
}

// Start creates the configs and calls the function that handles the prometheus_exec receiver
func (per *prometheusExecReceiver) Start(ctx context.Context, host component.Host) error {
	// Shutdown channel
	per.shutdownCh = make(chan struct{})

	go per.manageProcess(context.Background(), host)

	return nil
}

// manageProcess is an infinite loop that handles starting and restarting Prometheus-receiver/subprocess pairs
func (per *prometheusExecReceiver) manageProcess(ctx context.Context, host component.Host) {
	var crashCount int

	for {

		receiver, err := per.createAndStartReceiver(ctx, host)
		if err != nil {
			per.params.Logger.Error("createReceiver() error", zap.String("error", err.Error()))
			return
		}

		elapsed := per.runProcess(ctx)

		err = receiver.Shutdown(ctx)
		if err != nil {
			per.params.Logger.Error("could not stop receiver associated to process, killing it", zap.String("error", err.Error()))
			return
		}

		crashCount = per.computeCrashCount(elapsed, crashCount)
		per.computeDelayAndSleep(elapsed, crashCount)

		// Exit loop if shutdown was signaled
		select {
		case <-per.shutdownCh:
			return
		default:
		}
	}
}

// createAndStartReceiver will create the underlying Prometheus receiver and generate a random port if one is needed, then start it
func (per *prometheusExecReceiver) createAndStartReceiver(ctx context.Context, host component.Host) (component.MetricsReceiver, error) {
	currentPort := per.port

	// Generate a port if none was specified
	if currentPort == 0 {
		var err error
		currentPort, err = generateRandomPort()
		if err != nil {
			return nil, fmt.Errorf("generateRandomPort() error - killing this single process/receiver: %w", err)
		}

		per.promReceiverConfig.PrometheusConfig.ScrapeConfigs[0].ServiceDiscoveryConfig.StaticConfigs[0].Targets = []model.LabelSet{
			{model.AddressLabel: model.LabelValue(fmt.Sprintf("localhost:%v", currentPort))},
		}
	}

	// Create and start the underlying Prometheus receiver
	factory := prometheusreceiver.NewFactory()
	receiver, err := factory.CreateMetricsReceiver(ctx, per.params, per.promReceiverConfig, per.consumer)
	if err != nil {
		return nil, fmt.Errorf("unable to create Prometheus receiver - killing this single process/receiver: %w", err)
	}

	per.subprocessConfig = per.fillPortPlaceholders(currentPort)

	err = receiver.Start(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("could not start receiver - killing this single process/receiver: %w", err)
	}

	return receiver, nil
}

// runProcess will run the process and return runtime, or handle a shutdown if one is triggered while the subprocess is running
func (per *prometheusExecReceiver) runProcess(ctx context.Context) time.Duration {
	childCtx, cancel := context.WithCancel(ctx)
	run := make(chan runResult, 1)

	go per.handleProcessResult(childCtx, run)

	select {
	case result := <-run:
		// Log the error from the subprocess without returning it since we want to restart the process if it exited
		if result.subprocessErr != nil {
			per.params.Logger.Info("Subprocess error", zap.String("error", result.subprocessErr.Error()))
		}
		cancel()
		return result.elapsed

	case <-per.shutdownCh:
		cancel()
		return 0
	}
}

// handleProcessResult calls the process manager's run function and pipes the return value into the channel
func (per *prometheusExecReceiver) handleProcessResult(childCtx context.Context, run chan<- runResult) {
	elapsed, subprocessErr := per.subprocessConfig.Run(childCtx, per.params.Logger)
	run <- runResult{elapsed, subprocessErr}
}

// computeDelayAndSleep will compute how long the process should delay before restarting and handle a shutdown while this goroutine waits
func (per *prometheusExecReceiver) computeDelayAndSleep(elapsed time.Duration, crashCount int) {
	sleepTime := getDelay(elapsed, healthyProcessTime, crashCount, healthyCrashCount)
	per.params.Logger.Info("Subprocess start delay", zap.String("time until process restarts", sleepTime.String()))

	select {
	case <-time.After(sleepTime):
		return

	case <-per.shutdownCh:
		return
	}
}

// computeCrashCount will compute crashCount according to runtime
func (per *prometheusExecReceiver) computeCrashCount(elapsed time.Duration, crashCount int) int {
	if elapsed > healthyProcessTime {
		return 1
	}
	crashCount++

	return crashCount
}

// fillPortPlaceholders will check if any of the strings in the process data have the {{port}} placeholder, and replace it if necessary
func (per *prometheusExecReceiver) fillPortPlaceholders(newPort int) *subprocessmanager.SubprocessConfig {
	port := strconv.Itoa(newPort)

	newConfig := *per.subprocessConfig

	newConfig.Command = strings.ReplaceAll(per.config.SubprocessConfig.Command, portTemplate, port)

	for i, env := range per.config.SubprocessConfig.Env {
		newConfig.Env[i].Value = strings.ReplaceAll(env.Value, portTemplate, port)
	}

	return &newConfig
}

// generateRandomPort will generate a random available port
func generateRandomPort() (int, error) {
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// getDelay will compute the delay for a given process according to its crash count and time alive using an exponential backoff algorithm
func getDelay(elapsed time.Duration, healthyProcessDuration time.Duration, crashCount int, healthyCrashCount int) time.Duration {
	// Return the initialDelay if the process is healthy (lasted longer than health duration) or has less or equal the allowed amount of crashes
	if elapsed > healthyProcessDuration || crashCount <= healthyCrashCount {
		return initialDelay
	}

	// Return initialDelay times 2 to the power of crashCount-healthyCrashCount (to offset for the allowed crashes) added to a random number
	return initialDelay * time.Duration(math.Pow(delayMultiplier, float64(crashCount-healthyCrashCount)+rand.Float64()))
}

// Shutdown stops the underlying Prometheus receiver.
func (per *prometheusExecReceiver) Shutdown(ctx context.Context) error {
	close(per.shutdownCh)
	return nil
}

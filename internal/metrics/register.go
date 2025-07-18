// Copyright Envoy Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package metrics

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/sdk/metric"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics"

	egv1a1 "github.com/envoyproxy/gateway/api/v1alpha1"
	"github.com/envoyproxy/gateway/internal/envoygateway/config"
	"github.com/envoyproxy/gateway/internal/metrics/restclient"
	"github.com/envoyproxy/gateway/internal/metrics/workqueue"
)

const (
	defaultEndpoint = "/metrics"
)

type Runner struct {
	cfg    *config.Server
	server *http.Server
}

func New(cfg *config.Server) *Runner {
	return &Runner{
		cfg: cfg,
	}
}

func (r *Runner) Start(ctx context.Context) error {
	metricsLogger := r.cfg.Logger.WithName("metrics")
	otel.SetLogger(metricsLogger.Logger)

	options, err := r.newOptions()
	if err != nil {
		return err
	}

	handler, err := r.registerForHandler(options)
	if err != nil {
		return err
	}

	if !options.pullOptions.disable {
		return r.start(options.address, handler)
	}

	return nil
}

func (r *Runner) Name() string {
	return "metrics"
}

func (r *Runner) Close() error {
	if r.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return r.server.Shutdown(ctx)
	}
	return nil
}

func (r *Runner) start(address string, handler http.Handler) error {
	handlers := http.NewServeMux()

	metricsLogger := r.cfg.Logger.WithName("metrics")
	metricsLogger.Info("starting metrics server", "address", address)
	if handler != nil {
		handlers.Handle(defaultEndpoint, handler)
	}

	r.server = &http.Server{
		Handler:           handlers,
		Addr:              address,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       15 * time.Second,
	}

	// Listen And Serve Metrics Server.
	go func() {
		if err := r.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			metricsLogger.Error(err, "start metrics server failed")
		}
	}()

	return nil
}

func (r *Runner) newOptions() (registerOptions, error) {
	newOpts := registerOptions{}
	newOpts.address = net.JoinHostPort(egv1a1.GatewayMetricsHost, fmt.Sprint(egv1a1.GatewayMetricsPort))

	if r.cfg.EnvoyGateway.DisablePrometheus() {
		newOpts.pullOptions.disable = true
	} else {
		newOpts.pullOptions.disable = false
		restclient.RegisterClientMetricsWithoutRequestTotal(metricsserver.Registry)
		// Workqueue metrics are already registered in controller-runtime. Use another registry.
		reg := prometheus.NewRegistry()
		workqueue.RegisterMetrics(reg)
		newOpts.pullOptions.registry = metricsserver.Registry
		newOpts.pullOptions.gatherer = prometheus.Gatherers{
			metricsserver.Registry, reg,
		}
	}

	for _, config := range r.cfg.EnvoyGateway.GetEnvoyGatewayTelemetry().Metrics.Sinks {
		sink := metricsSink{
			host:     config.OpenTelemetry.Host,
			port:     config.OpenTelemetry.Port,
			protocol: config.OpenTelemetry.Protocol,
		}

		// we do not explicitly set default values for ExporterInterval and ExporterTimeout
		// instead, let the upstream repository set default values for it
		if config.OpenTelemetry.ExportInterval != nil && len(*config.OpenTelemetry.ExportInterval) != 0 {
			interval, err := time.ParseDuration(string(*config.OpenTelemetry.ExportInterval))
			if err != nil {
				metricsLogger := r.cfg.Logger.WithName("metrics")
				metricsLogger.Error(err, "failed to parse exporter interval time format")
				return newOpts, err
			}

			sink.exportInterval = interval
		}
		if config.OpenTelemetry.ExportTimeout != nil && len(*config.OpenTelemetry.ExportTimeout) != 0 {
			timeout, err := time.ParseDuration(string(*config.OpenTelemetry.ExportTimeout))
			if err != nil {
				metricsLogger := r.cfg.Logger.WithName("metrics")
				metricsLogger.Error(err, "failed to parse exporter timeout time format")
				return newOpts, err
			}

			sink.exportTimeout = timeout
		}

		newOpts.pushOptions.sinks = append(newOpts.pushOptions.sinks, sink)
	}

	return newOpts, nil
}

// registerForHandler sets the global metrics registry to the provided Prometheus registerer.
// if enables prometheus, it will return a prom http handler.
func (r *Runner) registerForHandler(opts registerOptions) (http.Handler, error) {
	otelOpts := []metric.Option{}

	if err := r.registerOTELPromExporter(&otelOpts, opts); err != nil {
		return nil, err
	}
	if err := r.registerOTELHTTPexporter(&otelOpts, opts); err != nil {
		return nil, err
	}
	if err := r.registerOTELgRPCexporter(&otelOpts, opts); err != nil {
		return nil, err
	}
	otelOpts = append(otelOpts, stores.preAddOptions()...)

	mp := metric.NewMeterProvider(otelOpts...)
	otel.SetMeterProvider(mp)

	if !opts.pullOptions.disable {
		return promhttp.HandlerFor(opts.pullOptions.gatherer, promhttp.HandlerOpts{}), nil
	}
	return nil, nil
}

// registerOTELPromExporter registers OTEL prometheus exporter (PULL mode).
func (r *Runner) registerOTELPromExporter(otelOpts *[]metric.Option, opts registerOptions) error {
	if !opts.pullOptions.disable {
		promOpts := []otelprom.Option{
			otelprom.WithoutScopeInfo(),
			otelprom.WithoutTargetInfo(),
			otelprom.WithoutUnits(),
			otelprom.WithRegisterer(opts.pullOptions.registry),
			otelprom.WithoutCounterSuffixes(),
		}
		promreader, err := otelprom.New(promOpts...)
		if err != nil {
			return err
		}

		*otelOpts = append(*otelOpts, metric.WithReader(promreader))
		metricsLogger := r.cfg.Logger.WithName("metrics")
		metricsLogger.Info("initialized metrics pull endpoint", "address", opts.address, "endpoint", defaultEndpoint)
	}

	return nil
}

// registerOTELHTTPexporter registers OTEL HTTP metrics exporter (PUSH mode).
func (r *Runner) registerOTELHTTPexporter(otelOpts *[]metric.Option, opts registerOptions) error {
	for _, sink := range opts.pushOptions.sinks {
		if sink.protocol == egv1a1.HTTPProtocol {
			address := net.JoinHostPort(sink.host, fmt.Sprint(sink.port))
			httpexporter, err := otlpmetrichttp.New(
				context.Background(),
				otlpmetrichttp.WithEndpoint(address),
				otlpmetrichttp.WithInsecure(),
			)
			if err != nil {
				return err
			}

			periodOpts := []metric.PeriodicReaderOption{}
			// If we do not set the interval or timeout for the exporter,
			// we let the upstream set the default value for it.
			if sink.exportInterval != 0 {
				periodOpts = append(periodOpts, metric.WithInterval(sink.exportInterval))
			}
			if sink.exportTimeout != 0 {
				periodOpts = append(periodOpts, metric.WithTimeout(sink.exportTimeout))
			}

			otelreader := metric.NewPeriodicReader(httpexporter, periodOpts...)
			*otelOpts = append(*otelOpts, metric.WithReader(otelreader))
			metricsLogger := r.cfg.Logger.WithName("metrics")
			metricsLogger.Info("initialized otel http metrics push endpoint", "address", address)
		}
	}

	return nil
}

// registerOTELgRPCexporter registers OTEL gRPC metrics exporter (PUSH mode).
func (r *Runner) registerOTELgRPCexporter(otelOpts *[]metric.Option, opts registerOptions) error {
	for _, sink := range opts.pushOptions.sinks {
		if sink.protocol == egv1a1.GRPCProtocol {
			address := net.JoinHostPort(sink.host, fmt.Sprint(sink.port))
			httpexporter, err := otlpmetricgrpc.New(
				context.Background(),
				otlpmetricgrpc.WithEndpoint(address),
				otlpmetricgrpc.WithInsecure(),
			)
			if err != nil {
				return err
			}

			periodOpts := []metric.PeriodicReaderOption{}
			// If we do not set the interval or timeout for the exporter,
			// we let the upstream set the default value for it.
			if sink.exportInterval != 0 {
				periodOpts = append(periodOpts, metric.WithInterval(sink.exportInterval))
			}
			if sink.exportTimeout != 0 {
				periodOpts = append(periodOpts, metric.WithTimeout(sink.exportTimeout))
			}

			otelreader := metric.NewPeriodicReader(httpexporter, periodOpts...)
			*otelOpts = append(*otelOpts, metric.WithReader(otelreader))
			metricsLogger := r.cfg.Logger.WithName("metrics")
			metricsLogger.Info("initialized otel grpc metrics push endpoint", "address", address)
		}
	}

	return nil
}

type registerOptions struct {
	address     string
	pullOptions struct {
		registry prometheus.Registerer
		gatherer prometheus.Gatherer
		disable  bool
	}
	pushOptions struct {
		sinks []metricsSink
	}
}

type metricsSink struct {
	protocol       string
	host           string
	port           int32
	exportTimeout  time.Duration
	exportInterval time.Duration
}

// Package observability provides structured logging and Prometheus metrics.
package observability

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// NewLogger builds the process logger. Format is "json" or "text".
func NewLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("log level %q: %w", level, err)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	switch format {
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("log format %q must be json or text", format)
	}
}

// Metrics holds every Courier collector. The sync engine updates them.
type Metrics struct {
	Registry *prometheus.Registry

	SyncRuns         *prometheus.CounterVec
	SyncDuration     *prometheus.HistogramVec
	SyncLastSuccess  *prometheus.GaugeVec
	SyncInProgress   *prometheus.GaugeVec
	Files            *prometheus.GaugeVec
	Bytes            *prometheus.CounterVec
	Errors           *prometheus.CounterVec
	UpstreamDuration *prometheus.HistogramVec
	S3Duration       *prometheus.HistogramVec
	GCDeletedObjects *prometheus.CounterVec
	buildInfo        *prometheus.GaugeVec
}

// NewMetrics creates the collectors on a dedicated registry, together with Go
// runtime and process collectors.
func NewMetrics(version string) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		SyncRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "courier_sync_runs_total", Help: "Sync runs by result.",
		}, []string{"repo", "result"}),
		SyncDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "courier_sync_duration_seconds", Help: "Sync run duration.",
			Buckets: prometheus.ExponentialBuckets(1, 4, 8),
		}, []string{"repo"}),
		SyncLastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "courier_sync_last_success_timestamp_seconds", Help: "Unix time of the last successful sync.",
		}, []string{"repo"}),
		SyncInProgress: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "courier_sync_in_progress", Help: "1 while a sync is running.",
		}, []string{"repo"}),
		Files: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "courier_files", Help: "Files by state (mirrored, pending, failed).",
		}, []string{"repo", "state"}),
		Bytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "courier_bytes_total", Help: "Bytes transferred (downloaded, uploaded).",
		}, []string{"repo", "direction"}),
		Errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "courier_errors_total", Help: "Errors by stage and reason.",
		}, []string{"repo", "stage", "reason"}),
		UpstreamDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "courier_upstream_request_duration_seconds", Help: "Upstream HTTP request latency.",
			Buckets: prometheus.DefBuckets,
		}, []string{"repo", "status"}),
		S3Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "courier_s3_request_duration_seconds", Help: "S3 request latency.",
			Buckets: prometheus.DefBuckets,
		}, []string{"op", "status"}),
		GCDeletedObjects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "courier_gc_deleted_objects_total", Help: "Objects deleted by garbage collection.",
		}, []string{"repo"}),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "courier_build_info", Help: "Build information; value is always 1.",
		}, []string{"version"}),
	}
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.SyncRuns, m.SyncDuration, m.SyncLastSuccess, m.SyncInProgress, m.Files,
		m.Bytes, m.Errors, m.UpstreamDuration, m.S3Duration, m.GCDeletedObjects,
		m.buildInfo,
	)
	m.buildInfo.WithLabelValues(version).Set(1)
	return m
}

// InitRepo makes per-repo series visible (at zero) before the first sync, so
// alerts and dashboards do not depend on a sync having happened.
func (m *Metrics) InitRepo(repo string) {
	m.SyncInProgress.WithLabelValues(repo).Set(0)
	m.SyncLastSuccess.WithLabelValues(repo).Set(0)
	for _, r := range []string{"success", "failure", "cancelled"} {
		m.SyncRuns.WithLabelValues(repo, r)
	}
}

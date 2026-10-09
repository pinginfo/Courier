// Command courier mirrors package repositories into S3.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"courier/internal/config"
	"courier/internal/observability"
	"courier/internal/server"
	"courier/internal/storage"
	"courier/internal/syncer"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `courier %s

Usage:
  courier serve    -c config.yaml           run scheduler, API and metrics
  courier sync     -c config.yaml <repo>    run one sync now and exit
  courier validate -c config.yaml           validate the config file and exit
  courier version
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, usage, version)
		return errors.New("a command is required")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "serve":
		return cmdServe(rest)
	case "sync":
		return cmdSync(rest)
	case "validate":
		return cmdValidate(rest)
	case "version":
		fmt.Println(version)
		return nil
	case "help", "-h", "--help":
		fmt.Printf(usage, version)
		return nil
	default:
		fmt.Fprintf(os.Stderr, usage, version)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// parseFlags returns the config path and remaining positional arguments.
func parseFlags(name string, args []string) (string, []string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	path := fs.String("c", "/etc/courier/config.yaml", "path to the config file")
	if err := fs.Parse(args); err != nil {
		return "", nil, err
	}
	return *path, fs.Args(), nil
}

func cmdValidate(args []string) error {
	path, _, err := parseFlags("validate", args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	fmt.Printf("config OK: %d repo(s)\n", len(cfg.Repos))
	return nil
}

// app holds the wired dependencies shared by serve and sync.
type app struct {
	cfg     *config.Config
	metrics *observability.Metrics
	store   storage.Storage
	runner  *syncer.Runner
}

func setup(path string) (*app, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	logger, err := observability.NewLogger(os.Stderr, cfg.Log.Level, cfg.Log.Format)
	if err != nil {
		return nil, err
	}
	metrics := observability.NewMetrics(version)
	store, err := storage.NewS3(cfg.Storage, func(op, status string, d time.Duration) {
		metrics.S3Duration.WithLabelValues(op, status).Observe(d.Seconds())
	})
	if err != nil {
		return nil, err
	}
	runner, err := syncer.NewRunner(cfg, store, metrics, logger, "courier/"+version)
	if err != nil {
		return nil, err
	}
	for _, r := range cfg.Repos {
		metrics.InitRepo(r.Name)
	}
	return &app{cfg: cfg, metrics: metrics, store: store, runner: runner}, nil
}

func cmdSync(args []string) error {
	path, rest, err := parseFlags("sync", args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("usage: courier sync -c config.yaml <repo>")
	}
	a, err := setup(path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	res, err := a.runner.RunOnce(ctx, rest[0])
	if err != nil {
		return err
	}
	fmt.Printf("sync OK: planned=%d skipped=%d downloaded=%d bytes=%d published=%d deleted=%d in %s\n",
		res.Planned, res.Skipped, res.Downloaded, res.Bytes, res.Published, res.Deleted, res.Duration.Round(time.Millisecond))
	return nil
}

func cmdServe(args []string) error {
	path, _, err := parseFlags("serve", args)
	if err != nil {
		return err
	}
	a, err := setup(path)
	if err != nil {
		return err
	}
	logger, _ := observability.NewLogger(os.Stderr, a.cfg.Log.Level, a.cfg.Log.Format)
	logger.Info("courier starting", "version", version, "repos", len(a.cfg.Repos))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := a.runner.Start(ctx); err != nil {
		return err
	}
	err = server.Run(ctx, server.Options{
		Addr:     a.cfg.Server.Listen,
		Logger:   logger,
		Registry: a.metrics.Registry,
		Ready:    a.store.Ping,
		Status:   func() any { return a.runner.Status() },
		Trigger:  a.runner.Trigger,
		TriggerStatus: func(err error) int {
			switch {
			case errors.Is(err, syncer.ErrUnknownRepo):
				return http.StatusNotFound
			case errors.Is(err, syncer.ErrBusy):
				return http.StatusConflict
			case errors.Is(err, syncer.ErrUnsupported):
				return http.StatusNotImplemented
			default:
				return http.StatusInternalServerError
			}
		},
	})
	logger.Info("shutting down: waiting for running syncs to stop")
	a.runner.Stop()
	logger.Info("courier stopped")
	return err
}

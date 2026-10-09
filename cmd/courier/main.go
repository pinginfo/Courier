// Command courier mirrors package repositories into S3.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"courier/internal/config"
	"courier/internal/observability"
	"courier/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `courier ` + "%s" + `

Usage:
  courier serve    -c config.yaml   run scheduler, API and metrics
  courier validate -c config.yaml   validate the config file and exit
  courier sync     -c config.yaml <repo>   run one sync now (not implemented yet)
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
	case "validate":
		return cmdValidate(rest)
	case "sync":
		return errors.New("sync is not implemented yet")
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

func configFlag(name string, args []string) (string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	path := fs.String("c", "/etc/courier/config.yaml", "path to the config file")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	return *path, nil
}

func cmdValidate(args []string) error {
	path, err := configFlag("validate", args)
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

func cmdServe(args []string) error {
	path, err := configFlag("serve", args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	logger, err := observability.NewLogger(os.Stderr, cfg.Log.Level, cfg.Log.Format)
	if err != nil {
		return err
	}
	metrics := observability.NewMetrics(version)
	for _, r := range cfg.Repos {
		metrics.InitRepo(r.Name)
	}
	logger.Info("courier starting", "version", version, "repos", len(cfg.Repos))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err = server.Run(ctx, server.Options{
		Addr:     cfg.Server.Listen,
		Logger:   logger,
		Registry: metrics.Registry,
		Status: func() []server.RepoStatus {
			out := make([]server.RepoStatus, 0, len(cfg.Repos))
			for _, r := range cfg.Repos {
				out = append(out, server.RepoStatus{Name: r.Name, Type: r.Type, Schedule: r.Schedule})
			}
			return out
		},
	})
	logger.Info("courier stopped")
	return err
}

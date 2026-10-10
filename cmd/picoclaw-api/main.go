package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/sipeed/picoclaw/pkg/apiserve"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

func main() {
	if err := newCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

func newCommand() *cobra.Command {
	var (
		debug   bool
		home    string
		cfgPath string
		host    string
		port    int
	)

	cmd := &cobra.Command{
		Use:   "picoclaw-api",
		Short: "Serve PicoClaw agents over HTTP for other internal services",
		RunE: func(_ *cobra.Command, _ []string) error {
			if home == "" {
				home = apiserve.ResolveHome()
			}
			if cfgPath == "" {
				// Resolve from the effective home, not from the environment:
				// --home must pin the config file as well as the database,
				// otherwise the server reads ~/.picoclaw/config.json while
				// writing its state into the requested home.
				cfgPath = apiserve.ResolveConfigPathFor(home)
			}

			if debug {
				logger.SetLevel(logger.DEBUG)
			} else {
				logger.SetLevelFromString(config.ResolveGatewayLogLevel(cfgPath))
			}

			settings := apiserve.DefaultSettings(home)
			if host != "" {
				settings.Host = host
			}
			if port != 0 {
				settings.Port = port
			}

			srv, err := apiserve.New(apiserve.Options{
				Home:       home,
				ConfigPath: cfgPath,
				Debug:      debug,
				Settings:   &settings,
			})
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()
			return srv.Start(ctx)
		},
	}

	cmd.Flags().BoolVar(&debug, "debug", false, "enable debug logging")
	cmd.Flags().StringVar(&home, "home", "", "picoclaw home (default: PICOCLAW_HOME)")
	cmd.Flags().StringVar(&cfgPath, "config", "", "path to config.json (default: PICOCLAW_CONFIG)")
	cmd.Flags().StringVar(&host, "host", "",
		"bind address (default 127.0.0.1; use 0.0.0.0 to serve other hosts on an internal network)")
	cmd.Flags().IntVar(&port, "port", 0, "bind port (default 18900)")

	// With no subcommand the root RunE still serves HTTP; `keys` shares the same
	// binary and the same SQLite file, so key maintenance needs no extra process
	// and no management API.
	cmd.AddCommand(newKeysCommand())

	return cmd
}

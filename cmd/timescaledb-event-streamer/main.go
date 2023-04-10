package main

import (
	"fmt"
	"github.com/BurntSushi/toml"
	"github.com/noctarius/timescaledb-event-streamer/internal"
	"github.com/noctarius/timescaledb-event-streamer/internal/configuring"
	"github.com/noctarius/timescaledb-event-streamer/internal/configuring/sysconfig"
	"github.com/noctarius/timescaledb-event-streamer/internal/logging"
	"github.com/noctarius/timescaledb-event-streamer/internal/supporting"
	"github.com/noctarius/timescaledb-event-streamer/internal/version"
	"github.com/urfave/cli"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
)

var (
	configurationFile string
	verbose           bool
	debug             bool
	withCaller        bool
)

func main() {
	app := &cli.App{
		Name:  "timescaledb-event-streamer",
		Usage: "CDC (Chance Data Capture) for TimescaleDB Hypertable",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "config,c",
				Value:       "",
				Usage:       "Load configuration from `FILE`",
				Destination: &configurationFile,
			},
			&cli.BoolFlag{
				Name:        "verbose",
				Usage:       "Show verbose output",
				Destination: &verbose,
			},
			&cli.BoolFlag{
				Name:        "caller",
				Usage:       "Collect caller information for log messages",
				Destination: &withCaller,
			},
			&cli.BoolFlag{
				Name:        "debug,d",
				Usage:       "Show debug output",
				Destination: &withCaller,
			},
		},
		Action: start,
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

func start(*cli.Context) error {
	fmt.Printf("%s version %s (git revision %s; branch %s)\n",
		version.BinName, version.Version, version.CommitHash, version.Branch,
	)

	logging.WithCaller = withCaller
	logging.WithVerbose = verbose
	logging.WithDebug = debug

	config := &configuring.Config{}

	if configurationFile != "" {
		f, err := os.Open(configurationFile)
		if err != nil {
			return cli.NewExitError(fmt.Sprintf("Configuration file couldn't be opened: %v\n", err), 3)
		}

		b, err := io.ReadAll(f)
		if err != nil {
			return cli.NewExitError(fmt.Sprintf("Configuration file couldn't be read: %v\n", err), 4)
		}

		if err := toml.Unmarshal(b, &config); err != nil {
			return cli.NewExitError(fmt.Sprintf("Configuration file couldn't be decoded: %v\n", err), 5)
		}
	}

	if configuring.GetOrDefault(config, "postgresql.connection", "") == "" {
		return cli.NewExitError("PostgreSQL connection string required", 6)
	}

	systemConfig := sysconfig.NewSystemConfig(config)
	streamer, err := internal.NewStreamer(systemConfig)
	if err != nil {
		return err
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	done := supporting.NewWaiter()
	go func() {
		<-signals
		if err := streamer.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "Hard error when stopping replication: %v\n", err)
			os.Exit(1)
		}
		done.Signal()
	}()

	if err := streamer.Start(); err != nil {
		return err
	}

	if err := done.Await(); err != nil {
		return supporting.AdaptError(err, 10)
	}

	return nil
}

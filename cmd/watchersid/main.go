package main

import (
	"context"
	"errors"
	"flag"
	llog "log"
	"os"

	"go.uber.org/zap"

	"github.com/watchers-id/watchersid/pkg/log"
)

func main() {
	watchersidCmd, cfg := NewWatchersIDCommand()

	if err := watchersidCmd.Parse(os.Args[1:]); err != nil {
		llog.Fatalf("Failed to parse command line arguments: %v", err)
	}

	logger, err := log.NewZapLogger(cfg.verbose, cfg.jsonLogs)
	if err != nil {
		llog.Fatal(err)
	}
	//nolint:errcheck
	defer logger.Sync()

	cfg.logger = logger

	err = watchersidCmd.Run(context.Background())
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		logger.Fatal("Command failed.", zap.Error(err))
	}
}

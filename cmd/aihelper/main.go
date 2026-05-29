package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"AIHelper/internal/app"
	"AIHelper/internal/config"
)

func main() {
	configPath := flag.String("config", "configs/dev.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	application, err := app.New(cfg, os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init app: %v\n", err)
		os.Exit(1)
	}

	if err := application.Run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "run app: %v\n", err)
		os.Exit(1)
	}
}

package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

//go:embed all:assets
var assetsFS embed.FS

const defaultHopperDir = ".hopper"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	var err error
	switch os.Args[1] {
	case "init":
		err = cmdInit(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: hopper <command> [arguments]")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  init [path]   create a hopper directory (default: .hopper)")
	fmt.Fprintln(os.Stderr, "  run  [path]   run the agent against a hopper directory (default: .hopper)")
}

func cmdInit(args []string) error {
	cmd := flag.NewFlagSet("init", flag.ExitOnError)
	cmd.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: hopper init [path]")
		cmd.PrintDefaults()
	}
	cmd.Parse(args)

	path := defaultHopperDir
	if cmd.NArg() > 0 {
		path = cmd.Arg(0)
	}

	if entries, err := os.ReadDir(path); err == nil {
		if len(entries) > 0 {
			return fmt.Errorf("%s already exists and is not empty", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if err := extractAssets(path); err != nil {
		return err
	}
	fmt.Printf("initialized hopper directory at %s\n", path)
	return nil
}

func cmdRun(args []string) error {
	cmd := flag.NewFlagSet("run", flag.ExitOnError)
	verbose := cmd.Bool("v", false, "print the fully constructed conversation on every iteration")
	debug := cmd.Bool("debug", false, "wait for user input between iterations")
	cmd.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: hopper run [-v] [-debug] [path]")
		cmd.PrintDefaults()
	}
	cmd.Parse(args)

	path := defaultHopperDir
	if cmd.NArg() > 0 {
		path = cmd.Arg(0)
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("hopper directory %s not found (run `hopper init`)", path)
		}
		return err
	}

	client := anthropic.NewClient()
	agent := NewAgent(&client, path, *verbose, *debug)
	return agent.Run(context.TODO())
}

func extractAssets(dest string) error {
	return fs.WalkDir(assetsFS, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("assets", p)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, err := assetsFS.ReadFile(p)
		if err != nil {
			return err
		}
		mode := os.FileMode(0644)
		if strings.HasPrefix(rel, "programs"+string(filepath.Separator)) {
			mode = 0755
		}
		return os.WriteFile(target, data, mode)
	})
}

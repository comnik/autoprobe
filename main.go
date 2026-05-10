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
)

//go:embed all:assets
var assetsFS embed.FS

const defaultProbeDir = ".autoprobe"

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
	fmt.Fprintln(os.Stderr, "usage: autoprobe <command> [arguments]")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  init [--provider <p>] [--model <m>] [path]   create an autoprobe directory (default: .autoprobe)")
	fmt.Fprintln(os.Stderr, "  run  [path]                                  run the agent against an autoprobe directory (default: .autoprobe)")
}

func cmdInit(args []string) error {
	cmd := flag.NewFlagSet("init", flag.ExitOnError)
	providerName := cmd.String("provider", "anthropic", "LLM provider: anthropic | openai | google")
	model := cmd.String("model", "", "model id (provider-specific; empty means use the provider's default)")
	cmd.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: autoprobe init [--provider <name>] [--model <id>] [path]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "  --provider     anthropic (default), openai, or google")
		fmt.Fprintln(os.Stderr, "  --model        model id; empty uses the provider's default")
	}
	cmd.Parse(args)

	// Track which flags the user actually passed so we can preserve existing
	// config values on update when a flag was omitted.
	setFlags := map[string]bool{}
	cmd.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	if !validProvider(*providerName) {
		return fmt.Errorf("unknown provider %q (expected anthropic, openai, or google)", *providerName)
	}

	path := defaultProbeDir
	if cmd.NArg() > 0 {
		path = cmd.Arg(0)
	}

	info, err := os.Stat(path)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	update := false
	if exists {
		if !info.IsDir() {
			return fmt.Errorf("%s exists but is not a directory", path)
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			if !looksLikeProbeDir(path) {
				return fmt.Errorf("%s already exists and does not look like an autoprobe directory (expected programs/ and reinforcement/ subdirs)", path)
			}
			update = true
		}
	}

	if err := extractAssets(path); err != nil {
		return err
	}

	cfg := Config{Provider: *providerName, Model: *model}
	if update && configExists(path) {
		// Seed both the picker pre-selection and the no-flag fallback from the
		// existing file.
		existing, err := LoadConfig(path)
		if err != nil {
			return err
		}
		if !setFlags["provider"] {
			cfg.Provider = existing.Provider
		}
		if !setFlags["model"] {
			cfg.Model = existing.Model
		}
	}

	// Prompt interactively for whichever fields the user didn't pin via flag.
	// Both pinned → no TUI; one pinned → only the other screen shows.
	if !setFlags["provider"] || !setFlags["model"] {
		picked, err := runInitPicker(cfg, setFlags["provider"], setFlags["model"])
		if err != nil {
			return err
		}
		cfg = picked
	}
	if !validProvider(cfg.Provider) {
		return fmt.Errorf("unknown provider %q (expected anthropic, openai, or google)", cfg.Provider)
	}

	if err := WriteConfig(path, cfg); err != nil {
		return err
	}

	verb := "initialized"
	if update {
		verb = "updated"
	}
	fmt.Printf("%s autoprobe directory at %s (provider=%s", verb, path, cfg.Provider)
	if cfg.Model != "" {
		fmt.Printf(" model=%s", cfg.Model)
	}
	fmt.Println(")")
	return nil
}

func validProvider(name string) bool {
	switch name {
	case "anthropic", "openai", "google":
		return true
	}
	return false
}

func looksLikeProbeDir(path string) bool {
	for _, sub := range []string{"programs", "reinforcement"} {
		info, err := os.Stat(filepath.Join(path, sub))
		if err != nil || !info.IsDir() {
			return false
		}
	}
	return true
}

func cmdRun(args []string) error {
	cmd := flag.NewFlagSet("run", flag.ExitOnError)
	debug := cmd.Bool("debug", false, "wait for user input between iterations")
	goal := cmd.String("goal", "", "inline goal statement appended to the conversation as the last program output")
	cmd.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: autoprobe run [--debug] [--goal <text>] [path]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "  --debug        wait for user input between iterations")
		fmt.Fprintln(os.Stderr, "  --goal <text>  inline goal statement appended to the conversation as the last program output")
	}
	cmd.Parse(args)

	path := defaultProbeDir
	if cmd.NArg() > 0 {
		path = cmd.Arg(0)
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("autoprobe directory %s not found (run `autoprobe init`)", path)
		}
		return err
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("config file %s not found (re-run `autoprobe init` to create it)", configPath(path))
		}
		return err
	}

	provider, err := buildProvider(cfg.Provider, cfg.Model)
	if err != nil {
		return err
	}
	agent := NewAgent(provider, path, *goal, *debug)
	return agent.Run(context.TODO())
}

func buildProvider(name, model string) (Provider, error) {
	switch name {
	case "anthropic", "":
		return NewAnthropicProvider(model), nil
	case "openai":
		return NewOpenAIProvider(model), nil
	case "google":
		return NewGoogleProvider(model)
	default:
		return nil, fmt.Errorf("unknown provider %q (expected anthropic, openai, or google)", name)
	}
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

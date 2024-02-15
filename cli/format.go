package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"git.numtide.com/numtide/treefmt/cache"
	"git.numtide.com/numtide/treefmt/config"
	format2 "git.numtide.com/numtide/treefmt/format"
	"git.numtide.com/numtide/treefmt/walk"

	"github.com/charmbracelet/log"
	"golang.org/x/sync/errgroup"
)

var ErrFailOnChange = errors.New("unexpected changes detected, --fail-on-change is enabled")

func (f *Format) Run() error {
	start := time.Now()

	Cli.Configure()

	l := log.WithPrefix("format")

	defer func() {
		if err := cache.Close(); err != nil {
			l.Errorf("failed to close cache: %v", err)
		}
	}()

	// create an overall context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// read config
	cfg, err := config.ReadFile(Cli.ConfigFile)
	if err != nil {
		return fmt.Errorf("%w: failed to read config file", err)
	}

	globalExcludes, err := format2.CompileGlobs(cfg.Global.Excludes)

	// create optional formatter filter set
	formatterSet := make(map[string]bool)
	for _, name := range Cli.Formatters {
		_, ok := cfg.Formatters[name]
		if !ok {
			return fmt.Errorf("%w: formatter not found in config: %v", err, name)
		}
		formatterSet[name] = true
	}

	includeFormatter := func(name string) bool {
		if len(formatterSet) == 0 {
			return true
		} else {
			_, include := formatterSet[name]
			return include
		}
	}

	formatters := make(map[string]*format2.Formatter)

	// detect broken dependencies
	for name, formatterCfg := range cfg.Formatters {
		before := formatterCfg.Before
		if before != "" {
			// check child formatter exists
			_, ok := cfg.Formatters[before]
			if !ok {
				return fmt.Errorf("formatter %v is before %v but config for %v was not found", name, before, before)
			}
		}
	}

	// dependency cycle detection
	for name, formatterCfg := range cfg.Formatters {
		var ok bool
		var history []string
		childName := name
		for {
			// add to history
			history = append(history, childName)

			if formatterCfg.Before == "" {
				break
			} else if formatterCfg.Before == name {
				return fmt.Errorf("formatter cycle detected %v", strings.Join(history, " -> "))
			}

			// load child config
			childName = formatterCfg.Before
			formatterCfg, ok = cfg.Formatters[formatterCfg.Before]
			if !ok {
				return fmt.Errorf("formatter not found: %v", formatterCfg.Before)
			}
		}
	}

	// init formatters
	for name, formatterCfg := range cfg.Formatters {
		if !includeFormatter(name) {
			// remove this formatter
			delete(cfg.Formatters, name)
			l.Debugf("formatter %v is not in formatter list %v, skipping", name, Cli.Formatters)
			continue
		}

		formatter, err := format2.NewFormatter(name, formatterCfg, globalExcludes)
		if errors.Is(err, format2.ErrCommandNotFound) && Cli.AllowMissingFormatter {
			l.Debugf("formatter not found: %v", name)
			continue
		} else if err != nil {
			return fmt.Errorf("%w: failed to initialise formatter: %v", err, name)
		}

		formatters[name] = formatter
	}

	// iterate the initialised formatters configuring parent/child relationships
	for _, formatter := range formatters {
		if formatter.Before() != "" {
			child, ok := formatters[formatter.Before()]
			if !ok {
				// formatter has been filtered out by the user
				formatter.ResetBefore()
				continue
			}
			formatter.SetChild(child)
			child.SetParent(formatter)
		}
	}

	if err = cache.Open(Cli.TreeRoot, Cli.ClearCache, formatters); err != nil {
		return err
	}

	//
	completedCh := make(chan string, 1024)

	ctx = format2.SetCompletedChannel(ctx, completedCh)

	//
	eg, ctx := errgroup.WithContext(ctx)

	// start the formatters
	for name := range formatters {
		formatter := formatters[name]
		eg.Go(func() error {
			return formatter.Run(ctx)
		})
	}

	// determine paths to be formatted
	pathsCh := make(chan string, 1024)

	// update cache as paths are completed
	eg.Go(func() error {
		batchSize := 1024
		batch := make([]string, 0, batchSize)

		var changes int

		processBatch := func() error {
			if Cli.NoCache {
				changes += len(batch)
			} else {
				count, err := cache.Update(batch)
				if err != nil {
					return err
				}
				changes += count
			}
			batch = batch[:0]
			return nil
		}

	LOOP:
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case path, ok := <-completedCh:
				if !ok {
					break LOOP
				}
				batch = append(batch, path)
				if len(batch) == batchSize {
					if err = processBatch(); err != nil {
						return err
					}
				}
			}
		}

		// final flush
		if err = processBatch(); err != nil {
			return err
		}

		if Cli.FailOnChange && changes != 0 {
			return ErrFailOnChange
		}

		fmt.Printf("%v files changed in %v\n", changes, time.Now().Sub(start))
		return nil
	})

	eg.Go(func() error {
		// pass paths to each formatter
		for path := range pathsCh {
			for _, formatter := range formatters {
				if formatter.Wants(path) {
					formatter.Put(path)
				}
			}
		}

		// indicate no more paths for each formatter
		for _, formatter := range formatters {
			if formatter.Parent() != nil {
				// this formatter is not a root, it will be closed by a parent
				continue
			}
			formatter.Close()
		}

		// await completion
		for _, formatter := range formatters {
			formatter.AwaitCompletion()
		}

		// indicate no more completion events
		close(completedCh)

		return nil
	})

	eg.Go(func() (err error) {
		paths := Cli.Paths

		if len(paths) == 0 && Cli.Stdin {
			// read in all the paths
			scanner := bufio.NewScanner(os.Stdin)
			for scanner.Scan() {
				paths = append(paths, scanner.Text())
			}
		}

		walker, err := walk.New(Cli.Walk, Cli.TreeRoot, paths)
		if err != nil {
			return fmt.Errorf("%w: failed to create walker", err)
		}

		defer close(pathsCh)

		if Cli.NoCache {
			return walker.Walk(ctx, func(path string, info fs.FileInfo, err error) error {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
					// ignore symlinks and directories
					if !(info.IsDir() || info.Mode()&os.ModeSymlink == os.ModeSymlink) {
						pathsCh <- path
					}
					return nil
				}
			})
		}

		return cache.ChangeSet(ctx, walker, pathsCh)
	})

	// listen for shutdown and call cancel if required
	go func() {
		exit := make(chan os.Signal, 1)
		signal.Notify(exit, os.Interrupt, syscall.SIGTERM)
		<-exit
		cancel()
	}()

	return eg.Wait()
}

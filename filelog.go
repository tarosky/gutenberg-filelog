package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	stdlog "log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"github.com/fsnotify/fsnotify"
	"github.com/urfave/cli/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	log *zap.Logger
)

// This implements zapcore.WriteSyncer interface.
type lockedFileWriteSyncer struct {
	m    sync.Mutex
	f    *os.File
	path string
}

func newLockedFileWriteSyncer(path string) *lockedFileWriteSyncer {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0666)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error while creating log file: path: %s", err.Error())
		panic(err)
	}

	return &lockedFileWriteSyncer{
		f:    f,
		path: path,
	}
}

func (s *lockedFileWriteSyncer) Write(bs []byte) (int, error) {
	s.m.Lock()
	defer s.m.Unlock()

	return s.f.Write(bs)
}

func (s *lockedFileWriteSyncer) Sync() error {
	s.m.Lock()
	defer s.m.Unlock()

	return s.f.Sync()
}

func (s *lockedFileWriteSyncer) reopen() {
	s.m.Lock()
	defer s.m.Unlock()

	if err := s.f.Close(); err != nil {
		fmt.Fprintf(
			os.Stderr, "error while reopening file: path: %s, err: %s", s.path, err.Error())
	}

	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0666)
	if err != nil {
		fmt.Fprintf(
			os.Stderr, "error while reopening file: path: %s, err: %s", s.path, err.Error())
		panic(err)
	}

	s.f = f
}

func (s *lockedFileWriteSyncer) Close() error {
	s.m.Lock()
	defer s.m.Unlock()

	return s.f.Close()
}

func createLogger(ctx context.Context, logPath, errorLogPath AbsolutePath) *zap.Logger {
	enc := zapcore.NewJSONEncoder(zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		NameKey:        zapcore.OmitKey,
		CallerKey:      zapcore.OmitKey,
		FunctionKey:    zapcore.OmitKey,
		MessageKey:     "message",
		StacktraceKey:  zapcore.OmitKey,
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.CapitalLevelEncoder,
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	})

	out := newLockedFileWriteSyncer(string(logPath))
	errOut := newLockedFileWriteSyncer(string(errorLogPath))

	sigusr1 := make(chan os.Signal, 1)
	signal.Notify(sigusr1, syscall.SIGUSR1)

	go func() {
		for {
			select {
			case _, ok := <-sigusr1:
				if !ok {
					break
				}
				out.reopen()
				errOut.reopen()
			case <-ctx.Done():
				signal.Stop(sigusr1)
				// closing sigusr1 causes panic (close of closed channel)
				break
			}
		}
	}()

	return zap.New(
		zapcore.NewCore(enc, out, zap.NewAtomicLevelAt(zap.DebugLevel)),
		zap.ErrorOutput(errOut),
		zap.Development(),
		zap.WithCaller(false))
}

func setPIDFile(path string) func() {
	if path == "" {
		return func() {}
	}

	pid := []byte(strconv.Itoa(os.Getpid()))
	if err := ioutil.WriteFile(path, pid, 0644); err != nil {
		log.Panic(
			"failed to create PID file",
			zap.String("path", path),
			zap.Error(err))
	}

	return func() {
		if err := os.Remove(path); err != nil {
			log.Error(
				"failed to remove PID file",
				zap.String("path", path),
				zap.Error(err))
		}
	}
}

type AbsolutePath string

func (p *AbsolutePath) UnmarshalJSON(data []byte) error {
	var v []interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}

	path, err := filepath.Abs(string(data))
	if err != nil {
		return err
	}

	*p = AbsolutePath(path)

	return nil
}

type watch struct {
	Directory AbsolutePath `json:"directory"`
	Pattern   string       `json:"pattern"`
	LogGroup  string       `json:"loggroup"`
}

type config struct {
	Region       string       `json:"region"`
	S3Bucket     string       `json:"s3bucket"`
	LogPath      AbsolutePath `json:"logpath"`
	ErrorLogPath AbsolutePath `json:"errorlogpath"`
	PIDPath      AbsolutePath `json:"pidpath"`
	Watches      []watch      `json:"watches"`
}

func main() {
	app := cli.NewApp()
	app.Name = "filelog"
	app.Description = "Upload file-style logs to S3 and notify it using CloudWatch Logs"

	app.Flags = []cli.Flag{
		&cli.PathFlag{
			Name:     "config-file",
			Aliases:  []string{"f"},
			Required: true,
			Usage:    "File to configure filelog.",
		},
	}

	app.Action = func(c *cli.Context) error {
		mustGetAbsPath := func(name string) string {
			path, err := filepath.Abs(c.Path(name))
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to get %s: %s", name, err.Error())
				panic(err)
			}
			return path
		}

		configPath := mustGetAbsPath("config-file")
		file, err := os.Open(configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to read config file: %s", err.Error())
			panic(err)
		}

		configData, err := ioutil.ReadAll(file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to read config data: %s", err.Error())
			panic(err)
		}

		cfg := &config{}
		if err := json.Unmarshal(configData, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "malformed config file: %s", err.Error())
			panic(err)
		}

		log = createLogger(c.Context, cfg.LogPath, cfg.ErrorLogPath)
		defer log.Sync()

		removePIDFile := setPIDFile(string(cfg.PIDPath))
		defer removePIDFile()

		ctx, cancel := context.WithCancel(context.Background())

		sig := make(chan os.Signal)
		signal.Notify(sig, os.Interrupt, os.Kill, syscall.SIGTERM, syscall.SIGQUIT)
		go func() {
			defer func() {
				signal.Stop(sig)
				close(sig)
			}()

			<-sig
			cancel()
		}()

		watchLogs(ctx, cfg)

		return nil
	}

	err := app.Run(os.Args)
	if err != nil {
		stdlog.Panic("failed to run app", zap.Error(err))
	}
}

func watchLog(ctx context.Context, w watch) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Error("failed to watch", zap.Error(err))
		return err
	}

	if err := watcher.Add(string(w.Directory)); err != nil {
		log.Error("failed to add listener", zap.Error(err), zap.String("directory", string(w.Directory)))
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-watcher.Events:
			if ev.Op|fsnotify.Write != 0 {

			}
		}
	}
}

func watchLogs(ctx context.Context, cfg *config) {
	for _, w := range cfg.Watches {
		go watchLog(ctx, w)
	}
}

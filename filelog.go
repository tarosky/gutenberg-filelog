package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	stdlog "log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	cwlogs "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwlt "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/fsnotify/fsnotify"
	"github.com/urfave/cli/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
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
	var path string
	if err := json.Unmarshal(data, &path); err != nil {
		return err
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}

	*p = AbsolutePath(absPath)

	return nil
}

type watch struct {
	Directory AbsolutePath `json:"directory"`
	LogGroup  string       `json:"loggroup"`
	LogStream string       `json:"logstream"`
}

type configure struct {
	Region       string       `json:"region"`
	S3Bucket     string       `json:"s3bucket"`
	S3KeyPrefix  string       `json:"s3keyprefix"`
	LogPath      AbsolutePath `json:"logpath"`
	ErrorLogPath AbsolutePath `json:"errorlogpath"`
	PIDPath      AbsolutePath `json:"pidpath"`
	Watches      []watch      `json:"watches"`
}

type environment struct {
	configure
	wMap         watchMap
	awsConfig    *aws.Config
	s3Client     *s3.Client
	cwlogsClient *cwlogs.Client
	log          *zap.Logger
}

func createAWSConfig(ctx context.Context, cfg *configure) *aws.Config {
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(cfg.Region))
	if err != nil {
		panic(err)
	}

	return &awsCfg
}

type watchMap map[AbsolutePath]*watch

func newWatchMap() watchMap {
	return (watchMap)(map[AbsolutePath]*watch{})
}

func (t watchMap) add(w *watch) error {
	mt := (map[AbsolutePath]*watch)(t)

	if _, ok := mt[w.Directory]; ok {
		return errors.New("duplicate entry")
	}
	mt[w.Directory] = w

	return nil
}

func (t watchMap) get(path string) (*watch, error) {
	watch, ok := (map[AbsolutePath]*watch)(t)[(AbsolutePath)(filepath.Dir(path))]
	if !ok {
		return nil, errors.New("no entry exists")
	}
	return watch, nil
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
		defer panic("debugging")

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

		cfg := &configure{}
		if err := json.Unmarshal(configData, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "malformed config file: %s", err.Error())
			panic(err)
		}

		ctx, cancel := context.WithCancel(c.Context)

		awsConfig := createAWSConfig(ctx, cfg)

		e := &environment{
			configure:    *cfg,
			awsConfig:    awsConfig,
			s3Client:     s3.NewFromConfig(*awsConfig),
			cwlogsClient: cwlogs.NewFromConfig(*awsConfig),
			log:          createLogger(c.Context, cfg.LogPath, cfg.ErrorLogPath),
		}
		defer e.log.Sync()

		wMap := newWatchMap()
		for _, w := range cfg.Watches {
			w := w
			if err := wMap.add(&w); err != nil {
				e.log.Panic("failed to add", zap.String("directory", string(w.Directory)))
			}
		}

		e.wMap = wMap

		removePIDFile := setPIDFile(string(cfg.PIDPath))
		defer removePIDFile()

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

		return watchLogs(ctx, e)
	}

	err := app.Run(os.Args)
	if err != nil {
		stdlog.Panic("failed to run app", zap.Error(err))
	}
}

func generateKey(e *environment, name string) (string, error) {
	fileName := filepath.Base(name)

	var bs [32]byte
	_, err := rand.Read(bs[:])
	if err != nil {
		return "", err
	}
	dir := hex.EncodeToString(bs[:])

	return e.S3KeyPrefix + dir + "/" + fileName, nil
}

func uploadFile(ctx context.Context, e *environment, name, key string) error {
	zapPath := zap.String("path", name)

	file, err := os.Open(name)
	if err != nil {
		e.log.Error("failed to open file", zap.Error(err), zapPath)
		return err
	}
	defer func() {
		if err := file.Close(); err != nil {
			e.log.Error("failed to close file", zap.Error(err), zapPath)
		}
	}()

	if _, err := e.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:          &e.S3Bucket,
		Key:             &key,
		Body:            file,
		ContentEncoding: aws.String("application/octet-stream"),
	}); err != nil {
		e.log.Error("failed to upload file to S3", zap.Error(err), zapPath)
		return err
	}

	return nil
}

func fileURL(e *environment, key string) string {
	return fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", e.S3Bucket, e.Region, key)
}

func processLog(ctx context.Context, e *environment, w *watch, name string) {
	zapPath := zap.String("path", name)

	key, err := generateKey(e, name)
	if err != nil {
		e.log.Error("failed to generate S3 key", zap.Error(err), zapPath)
		return
	}

	if err := uploadFile(ctx, e, name, key); err != nil {
		return
	}

	finfo, err := os.Stat(name)
	if err != nil {
		e.log.Error("failed to stat file", zap.Error(err), zapPath)
		return
	}

	e.cwlogsClient.PutLogEvents(ctx, &cwlogs.PutLogEventsInput{
		LogEvents: []cwlt.InputLogEvent{
			cwlt.InputLogEvent{
				Message:   aws.String(fmt.Sprintf("New log file created. URL: %s", fileURL(e, key))),
				Timestamp: aws.Int64(finfo.ModTime().Unix()),
			},
		},
		LogGroupName:  aws.String(w.LogGroup),
		LogStreamName: aws.String(w.LogStream),
		SequenceToken: aws.String("dummy"),
	})
}

func watchLogs(ctx context.Context, e *environment) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		e.log.Error("failed to watch", zap.Error(err))
		return err
	}

	for _, w := range e.Watches {
		if err := watcher.Add(string(w.Directory)); err != nil {
			e.log.Error("failed to add listener", zap.Error(err), zap.String("directory", string(w.Directory)))
			return err
		}
	}

	for {
		select {
		case <-ctx.Done():
			e.log.Debug("done by signal")
			return nil
		case err := <-watcher.Errors:
			e.log.Error("error", zap.Error(err))
			return err
		case ev := <-watcher.Events:
			switch ev.Op {
			case fsnotify.CloseWrite, fsnotify.MoveTo:
				w, err := e.wMap.get(ev.Name)
				if err != nil {
					e.log.Error("failed to find watch", zap.String("path", ev.Name))
					continue
				}
				go processLog(ctx, e, w, ev.Name)
			}
		}
	}
}

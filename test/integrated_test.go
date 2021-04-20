package test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
	"golang.org/x/sys/unix"
)

const (
	configPath          = "work/config.json"
	testLogDir1         = "work/test-log-dir-1"
	testLogDir2         = "work/test-log-dir-2"
	filelogLogPath      = "work/filelog.log"
	filelogErrorLogPath = "work/filelog-error.log"
	filelogPIDPath      = "work/filelog.pid"
)

// fixWorkDir moves working directory to project root directory.
// https://brandur.org/fragments/testing-go-project-root
func fixWorkDir() {
	_, filename, _, _ := runtime.Caller(0)
	dir := path.Join(path.Dir(filename), ".")
	err := os.Chdir(dir)
	if err != nil {
		panic(err)
	}
}

func buildFileLog() {
	curr, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	if err := os.Chdir(path.Dir(curr)); err != nil {
		panic(err)
	}
	defer func() {
		if err := os.Chdir(curr); err != nil {
			panic(err)
		}
	}()

	cmd := exec.Command("go", "build", "-o", "test/work/filelog", ".")
	if err := cmd.Run(); err != nil {
		panic(err)
	}
}

func readTestConfig(name string) string {
	cwd, err := os.Getwd()
	if err != nil {
		panic("failed to get current working directory")
	}

	path := cwd + "/config/" + name
	val, err := ioutil.ReadFile(path)
	if err != nil {
		panic("failed to load config file: " + path + ", error: " + err.Error())
	}
	return strings.TrimSpace(string(val))
}

type jsonDict map[string]interface{}
type jsonArray []interface{}

func generateConfig() {
	bs := [8]byte{}
	if _, err := rand.Read(bs[:]); err != nil {
		panic(err)
	}

	stream := time.Now().Format("2006-01-02T150405-0700") + "-" + hex.EncodeToString(bs[:])
	cfg := jsonDict{
		"region":       "ap-northeast-1",
		"s3bucket":     readTestConfig("s3-bucket"),
		"s3keyprefix":  "gutenberg-filelog/",
		"logpath":      filelogLogPath,
		"errorlogpath": filelogErrorLogPath,
		"pidpath":      filelogPIDPath,
		"watches": jsonArray{
			jsonDict{
				"directory": testLogDir1,
				"loggroup":  readTestConfig("test-log-group-1"),
				"logstream": stream,
			},
			jsonDict{
				"directory": testLogDir2,
				"loggroup":  readTestConfig("test-log-group-2"),
				"logstream": stream,
			},
		},
	}

	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		panic(err)
	}

	if err := ioutil.WriteFile(configPath, cfgJSON, 0644); err != nil {
		panic(err)
	}
}

func initTestSuite() *TestSuite {
	fixWorkDir()
	buildFileLog()
	generateConfig()

	return &TestSuite{ctx: context.Background()}
}

// TestSuite holds configs and sessions required to execute program.
type TestSuite struct {
	suite.Suite
	ctx     context.Context
	process *os.Process
	stdout  io.Writer
	stderr  io.Writer
}

func TestFileLogSuite(t *testing.T) {
	s := initTestSuite()
	suite.Run(t, s)
}

func (s *TestSuite) SetupTest() {
	s.Require().NoError(os.RemoveAll(filelogLogPath))
	s.Require().NoError(os.RemoveAll(filelogErrorLogPath))
	s.Require().NoError(os.RemoveAll(testLogDir1))
	s.Require().NoError(os.RemoveAll(testLogDir2))
	s.Require().NoError(os.MkdirAll(testLogDir1, 0755))
	s.Require().NoError(os.MkdirAll(testLogDir2, 0755))

	cmd := exec.CommandContext(s.ctx, "work/filelog", "-f", configPath)
	cmd.Env = []string{
		fmt.Sprintf("AWS_ACCESS_KEY_ID=%s", readTestConfig("access-key-id")),
		fmt.Sprintf("AWS_SECRET_ACCESS_KEY=%s", readTestConfig("secret-access-key")),
		fmt.Sprintf("AWS_ACCOUNT_ID=%s", readTestConfig("aws-account-id")),
	}
	{
		var err error
		s.stdout, err = os.OpenFile("work/stdout.log", os.O_WRONLY|os.O_CREATE, 0644)
		s.Assert().NoError(err)
		cmd.Stdout = s.stdout
	}
	{
		var err error
		s.stderr, err = os.OpenFile("work/stderr.log", os.O_WRONLY|os.O_CREATE, 0644)
		s.Assert().NoError(err)
		cmd.Stderr = s.stderr
	}
	s.Require().NoError(cmd.Start())
	s.process = cmd.Process
}

func (s *TestSuite) TearDownTest() {
	_ = s.process.Signal(unix.SIGTERM)
	state, err := s.process.Wait()
	if err != nil && err.Error() != "os: process already finished" {
		s.Require().NoError(err)
	}
	s.Require().Equal(0, state.ExitCode())
}

func (s *TestSuite) Test_DoNothing() {
	s.Assert().True(true)
	time.Sleep(time.Second)
}

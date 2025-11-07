package test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	cwlogs "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
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

var (
	testLogGroup1 string
	testLogGroup2 string
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

	cmd := exec.Command("go", "build", "-o", "test/work/filelog", ".")
	cmd.Dir = path.Dir(curr)
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
	val, err := os.ReadFile(path)
	if err != nil {
		panic("failed to load config file: " + path + ", error: " + err.Error())
	}
	return strings.TrimSpace(string(val))
}

type jsonDict map[string]interface{}
type jsonArray []interface{}

func createAWSConfig(ctx context.Context) *aws.Config {
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("ap-northeast-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			readTestConfig("access-key-id"),
			readTestConfig("secret-access-key"),
			"")))
	if err != nil {
		panic(err)
	}

	return &awsCfg
}

func generateStreamName() string {
	bs := [8]byte{}
	if _, err := rand.Read(bs[:]); err != nil {
		panic(err)
	}

	return time.Now().Format("2006-01-02T150405-0700") + "-" + hex.EncodeToString(bs[:])
}

func generateConfig(stream string) {
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
				"loggroup":  testLogGroup1,
				"logstream": stream,
			},
			jsonDict{
				"directory": testLogDir2,
				"loggroup":  testLogGroup2,
				"logstream": stream,
			},
		},
	}

	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		panic(err)
	}

	if err := os.WriteFile(configPath, cfgJSON, 0644); err != nil {
		panic(err)
	}
}

func initTestSuite() *TestSuite {
	fixWorkDir()
	buildFileLog()
	stream := generateStreamName()

	testLogGroup1 = readTestConfig("test-log-group-1")
	testLogGroup2 = readTestConfig("test-log-group-2")

	generateConfig(stream)
	ctx := context.Background()
	awsConfig := createAWSConfig(ctx)

	return &TestSuite{
		ctx:          ctx,
		logStream:    stream,
		awsConfig:    awsConfig,
		cwlogsClient: cwlogs.NewFromConfig(*awsConfig),
	}
}

// TestSuite holds configs and sessions required to execute program.
type TestSuite struct {
	suite.Suite
	ctx          context.Context
	logStream    string
	awsConfig    *aws.Config
	cwlogsClient *cwlogs.Client
	process      *os.Process
	stdout       io.Writer
	stderr       io.Writer
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

func (s *TestSuite) Test_LogFileUploaded() {
	startTime := time.Now().UnixNano() / 1_000_000

	time.Sleep(2 * time.Second)

	logPath := testLogDir1 + "/test.log"
	file, err := os.Create(logPath)
	s.Require().NoError(err)
	file.Write([]byte("hoge"))
	file.Sync()
	file.Write([]byte("hoge"))
	s.Require().NoError(file.Close())

	time.Sleep(10 * time.Second)

	res, err := s.cwlogsClient.GetLogEvents(s.ctx, &cwlogs.GetLogEventsInput{
		LogGroupName:  aws.String(testLogGroup1),
		LogStreamName: aws.String(s.logStream),
		StartTime:     aws.Int64(startTime),
		Limit:         aws.Int32(10),
	})
	s.Require().NoError(err)
	s.Assert().Equal(1, len(res.Events))
	logEvent := map[string]string{}
	s.Assert().NoError(json.Unmarshal([]byte(*res.Events[0].Message), &logEvent))
	res2, err := http.Get(logEvent["url"])
	s.Assert().NoError(err)

	tempDir, err := os.MkdirTemp("", "")
	s.Require().NoError(err)
	defer func() {
		s.Require().NoError(os.RemoveAll(tempDir))
	}()

	downloadedLogPath := tempDir + "/log.zip"
	file2, err := os.Create(downloadedLogPath)
	s.Require().NoError(err)
	_, err = io.Copy(file2, res2.Body)
	s.Require().NoError(err)
	s.Require().NoError(file2.Close())

	cmd := exec.CommandContext(s.ctx, "unzip", "-P", logEvent["password"], downloadedLogPath)
	cmd.Dir = tempDir
	s.Require().NoError(cmd.Run())
	content, err := os.ReadFile(tempDir + "/test.log")
	s.Assert().NoError(err)
	s.Require().Equal("hogehoge", string(content))

	_, err = os.Stat(logPath)
	s.Assert().Error(err)
}

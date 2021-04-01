module github.com/tarosky/gutenberg-filelog

go 1.15

require (
	github.com/aws/aws-sdk-go-v2 v1.3.1
	github.com/aws/aws-sdk-go-v2/config v1.1.4
	github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs v1.2.1
	github.com/aws/aws-sdk-go-v2/service/s3 v1.4.0
	github.com/fsnotify/fsnotify v1.4.9
	github.com/urfave/cli/v2 v2.2.0
	go.uber.org/zap v1.16.0
	golang.org/x/sync v0.0.0-20200625203802-6e8e738ad208
	golang.org/x/tools v0.0.0-20200908211811-12e1bf57a112 // indirect
)

replace github.com/fsnotify/fsnotify => github.com/tarosky/fsnotify v0.0.0-20210402041642-f481c745fc33

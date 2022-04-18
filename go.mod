module github.com/outofforest/isolator

go 1.18

replace github.com/ridge/parallel => github.com/outofforest/parallel v0.1.2

require (
	github.com/otiai10/copy v1.7.0
	github.com/outofforest/build v1.7.10
	github.com/outofforest/buildgo v0.3.5
	github.com/outofforest/ioc/v2 v2.5.0
	github.com/outofforest/libexec v0.2.1
	github.com/outofforest/logger v0.2.0
	github.com/outofforest/run v0.2.2
	github.com/ridge/must v0.6.0
	github.com/ridge/parallel v0.1.1
	go.uber.org/zap v1.21.0
)

require (
	go.uber.org/atomic v1.9.0 // indirect
	go.uber.org/multierr v1.8.0 // indirect
)

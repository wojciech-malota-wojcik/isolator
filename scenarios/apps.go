package scenarios

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/outofforest/logger"
	"github.com/outofforest/parallel"
	"github.com/pkg/errors"
	"github.com/ridge/must"
	"go.uber.org/zap"

	"github.com/outofforest/isolator/lib/retry"
	"github.com/outofforest/isolator/lib/task"
	"github.com/outofforest/isolator/lib/tcontext"
	"github.com/outofforest/isolator/wire"
)

// LogsConfig is the configuration of remote loki logs receiver.
type LogsConfig struct {
	Address string
}

// RunAppsConfig is the config for running applications.
type RunAppsConfig struct {
	CacheDir   string
	AppsDir    string
	LogsConfig LogsConfig
}

// Application represents an app to run in isolation.
type Application interface {
	GetName() string
	GetIP() net.IP
	GetTaskFunc(config RunAppsConfig, appHosts map[string]net.IP, spawn parallel.SpawnFn, logsCh chan<- logEnvelope) task.Func
}

type logEnvelope struct {
	AppName string
	Log     wire.Log
}

// RunApps runs applications.
func RunApps(ctx context.Context, config RunAppsConfig, apps ...Application) error {
	containerHosts := map[string]net.IP{}
	for _, app := range apps {
		if app.GetName() != "" && app.GetIP() != nil {
			containerHosts[app.GetName()] = app.GetIP()
		}
	}

	return parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
		logsCh := make(chan logEnvelope)

		spawn("apps", parallel.Exit, func(ctx context.Context) error {
			defer close(logsCh)

			return parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
				spawn("start", parallel.Continue, func(ctx context.Context) error {
					return task.Run(ctx, nil, func(ctx context.Context, taskCh chan<- task.Task, doneCh <-chan task.Task) error {
						for _, app := range apps {
							select {
							case <-ctx.Done():
								return errors.WithStack(ctx.Err())
							case taskCh <- task.Task{
								ID: "app:run:" + app.GetName(),
								Do: app.GetTaskFunc(config, containerHosts, spawn, logsCh),
							}:
							}
						}

						return nil
					})
				})

				return nil
			})
		})
		spawn("logs", parallel.Fail, func(ctx context.Context) error {
			return parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
				batchCh := make(chan []logEnvelope, 10)

				spawn("collector", parallel.Fail, func(ctx context.Context) error {
					log := logger.Get(ctx)
					defer close(batchCh)

					batch := make([]logEnvelope, 0, 100)
					ticker := time.NewTicker(10 * time.Second)
					defer ticker.Stop()

					ok := true
					for ok {
						var logged logEnvelope
						select {
						case logged, ok = <-logsCh:
							if ok {
								batch = append(batch, logged)
								if len(batch) < cap(batch) {
									continue
								}
							}
						case <-ticker.C:
						}

						if len(batch) == 0 {
							continue
						}

						select {
						case batchCh <- batch:
						default:
							log.Error("Log buffer full")
						}
						batch = make([]logEnvelope, 0, 100)
					}

					return errors.WithStack(ctx.Err())
				})
				spawn("sender", parallel.Fail, func(ctx context.Context) error {
					type logKey struct {
						Level  string
						Logger string
					}
					type logValue struct {
						Time   time.Time
						Values map[string]interface{}
					}

					ctx = tcontext.Reopen(ctx)
					log := logger.Get(ctx)
					for batch := range batchCh {
						items := map[logKey][]logValue{}
						for _, logged := range batch {
							values := map[string]interface{}{}
							if err := json.Unmarshal(logged.Log.Content, &values); err != nil {
								values = map[string]interface{}{
									"msg": string(logged.Log.Content),
								}
							}
							vLevel, ok := values["level"].(string)
							if !ok {
								vLevel = "info"
							}
							vLogger, ok := values["logger"].(string)
							if ok && vLogger != "" {
								vLogger = logged.AppName + "." + vLogger
							} else {
								vLogger = logged.AppName
							}

							delete(values, "level")
							delete(values, "logger")

							key := logKey{
								Level:  vLevel,
								Logger: vLogger,
							}
							items[key] = append(items[key], logValue{
								Time:   logged.Log.Time,
								Values: values,
							})
						}

						streams := make([]map[string]interface{}, 0, len(items))
						for k, is := range items {
							sort.Slice(is, func(i, j int) bool {
								return is[i].Time.Before(is[j].Time) //nolint:scopelint
							})

							values := make([][]string, 0, len(is))
							for _, item := range is {
								values = append(values, []string{
									strconv.FormatInt(item.Time.UnixNano(), 10),
									string(must.Bytes(json.Marshal(item.Values))),
								})
							}

							streams = append(streams, map[string]interface{}{
								"stream": map[string]interface{}{
									"level":  k.Level,
									"logger": k.Logger,
								},
								"values": values,
							})
						}

						err := retry.Do(ctx, retry.FixedConfig{RetryAfter: time.Second, MaxAttempts: 5}, func() error {
							requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
							defer cancel()

							req := must.HTTPRequest(http.NewRequestWithContext(requestCtx, http.MethodPost, config.LogsConfig.Address, bytes.NewReader(must.Bytes(json.Marshal(map[string]interface{}{"streams": streams})))))
							req.Header.Set("Content-Type", "application/json")

							resp, err := http.DefaultClient.Do(req)
							if err != nil {
								return retry.Retriable(err)
							}
							defer resp.Body.Close()

							if resp.StatusCode != http.StatusNoContent {
								body, err := io.ReadAll(resp.Body)
								if err != nil {
									return retry.Retriable(err)
								}

								switch resp.StatusCode {
								case http.StatusBadRequest:
									log.Error("Received Bad Request response from Loki", zap.ByteString("body", body))
								default:
									return retry.Retriable(errors.Errorf("unexpected response from loki endpoint, code: %d, body: %s", resp.StatusCode, body))
								}
							}
							return nil
						})
						if err != nil {
							log.Error("Sending logs to loki failed", zap.Error(err))
						}
					}
					return errors.WithStack(ctx.Err())
				})
				return nil
			})
		})
		return nil
	})
}

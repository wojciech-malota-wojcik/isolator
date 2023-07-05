package docker

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/outofforest/logger"
	"github.com/outofforest/parallel"
	"github.com/pkg/errors"
	"github.com/ridge/must"
	"go.uber.org/zap"

	"github.com/outofforest/isolator/lib/retry"
)

// InflateImage downloads and inflates docker image in the current directory.
func InflateImage(ctx context.Context, c *http.Client, image, tag string, cacheDir string) error {
	return parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
		reactor := newTaskReactor()
		imageClient := newImageClient(c, reactor, image, tag, cacheDir)

		spawn("reactor", parallel.Fail, reactor.Run)
		spawn("image", parallel.Exit, func(ctx context.Context) error {
			return imageClient.Inflate(ctx)
		})

		return nil
	})
}

// Task is the task to execute in the reactor.
type Task struct {
	ID string
	Do func(ctx context.Context) error
}

type taskEnvelope struct {
	T    Task
	Ch   chan chan struct{}
	Done chan<- Task
}

type taskReactor struct {
	taskCh chan taskEnvelope
}

func newTaskReactor() *taskReactor {
	return &taskReactor{
		taskCh: make(chan taskEnvelope),
	}
}

func (c *taskReactor) AwaitTasks(ctx context.Context, done chan<- Task, tasks ...Task) error {
	type awaitedTask struct {
		T       Task
		AwaitCh chan struct{}
	}

	log := logger.Get(ctx)
	awaits := make([]awaitedTask, 0, len(tasks))

	for _, t := range tasks {
		log.Info("Enqueueing task", zap.String("taskID", t.ID))
		te := taskEnvelope{
			T:    t,
			Ch:   make(chan chan struct{}, 1),
			Done: done,
		}
		select {
		case <-ctx.Done():
			return errors.WithStack(ctx.Err())
		case c.taskCh <- te:
			select {
			case <-ctx.Done():
				return errors.WithStack(ctx.Err())
			case awaitCh := <-te.Ch:
				awaits = append(awaits, awaitedTask{
					T:       te.T,
					AwaitCh: awaitCh,
				})
			}
		}
	}

	for _, await := range awaits {
		log.Info("Awaiting task", zap.String("taskID", await.T.ID), zap.Int("taskCount", len(awaits)))

		select {
		case <-ctx.Done():
			return errors.WithStack(ctx.Err())
		case <-await.AwaitCh:
		}

		log.Info("Task completed", zap.String("taskID", await.T.ID))
	}

	return nil
}

func (c *taskReactor) Run(ctx context.Context) error {
	const workers = 5

	log := logger.Get(ctx)
	log.Info("Task reactor started")
	defer log.Info("Task reactor terminated")

	return parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
		requestedTaskCh := make(chan taskEnvelope)
		completedTaskCh := make(chan taskEnvelope)

		spawn("supervisor", parallel.Fail, func(ctx context.Context) error {
			activeTasks := map[string]chan struct{}{}

			doneFunc := func(ctx context.Context, te taskEnvelope) {
				ch := activeTasks[te.T.ID]
				delete(activeTasks, te.T.ID)
				close(ch)

				if te.Done != nil {
					select {
					case <-ctx.Done():
					case te.Done <- te.T:
					}
				}
			}

			for {
				select {
				case <-ctx.Done():
					return errors.WithStack(ctx.Err())
				case te := <-c.taskCh:
					awaitCh, exists := activeTasks[te.T.ID]
					if !exists {
						awaitCh = make(chan struct{})
						activeTasks[te.T.ID] = awaitCh
					}

					select {
					case <-ctx.Done():
						return errors.WithStack(ctx.Err())
					case te.Ch <- awaitCh:
					}

					for {
						select {
						case <-ctx.Done():
							return errors.WithStack(ctx.Err())
						case requestedTaskCh <- te:
						case te := <-completedTaskCh:
							doneFunc(ctx, te)
							continue
						}
						break
					}
				case te := <-completedTaskCh:
					doneFunc(ctx, te)
				}
			}
		})

		for i := 0; i < workers; i++ {
			spawn(fmt.Sprintf("worker-%d", i), parallel.Fail, func(ctx context.Context) error {
				for {
					var te taskEnvelope
					select {
					case <-ctx.Done():
						return errors.WithStack(ctx.Err())
					case te = <-requestedTaskCh:
					}

					if err := te.T.Do(ctx); err != nil {
						return errors.WithStack(err)
					}

					select {
					case <-ctx.Done():
						return errors.WithStack(ctx.Err())
					case completedTaskCh <- te:
					}
				}
			})
		}

		return nil
	})
}

type manifest struct {
	Config struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Layers []struct {
		Digest string `json:"digest"`
	} `json:"layers"`
}

type imageClient struct {
	c        *http.Client
	reactor  *taskReactor
	image    string
	tag      string
	cacheDir string

	mu        sync.Mutex
	authToken string
}

func newImageClient(
	c *http.Client,
	reactor *taskReactor,
	image, tag string,
	cacheDir string,
) *imageClient {
	if !strings.Contains(image, "/") {
		image = "library/" + image
	}
	return &imageClient{
		c:        c,
		reactor:  reactor,
		image:    image,
		tag:      tag,
		cacheDir: cacheDir,
	}
}

func (c *imageClient) Inflate(ctx context.Context) error {
	ctx = logger.With(ctx,
		zap.String("image", c.image+":"+c.tag),
	)
	log := logger.Get(ctx)
	log.Info("Docker image requested")

	fileName := strings.ReplaceAll(c.image, "/", ":")
	manifestPath := filepath.Join(c.cacheDir, fmt.Sprintf("%s:%s:manifest.json", fileName, c.tag))

	err := c.reactor.AwaitTasks(ctx, nil, Task{
		ID: fmt.Sprintf("docker:manifest:%s:%s", c.image, c.tag),
		Do: func(ctx context.Context) error {
			return c.fetchManifest(ctx, manifestPath)
		},
	})
	if err != nil {
		return err
	}

	f, err := os.Open(manifestPath)
	if err != nil {
		return errors.WithStack(err)
	}
	defer f.Close()

	var r io.Reader = f
	var hasher hash.Hash
	if strings.HasPrefix(c.tag, "sha256:") {
		hasher = sha256.New()
		r = io.TeeReader(r, hasher)
	}

	var manifest manifest
	if err := json.NewDecoder(r).Decode(&manifest); err != nil {
		return errors.WithStack(err)
	}

	if hasher != nil {
		computedDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
		if computedDigest != c.tag {
			return retry.Retriable(errors.Errorf("manifest digest doesn't match, expected: %s, got: %s", c.tag, computedDigest))
		}
	}

	// FIXME (wojciech): Downlaod this in parallel with layers
	err = c.reactor.AwaitTasks(ctx, nil, Task{
		ID: fmt.Sprintf("docker:config:%s:%s", c.image, manifest.Config.Digest),
		Do: func(ctx context.Context) error {
			return c.fetchBlob(ctx, manifest.Config.Digest, filepath.Join(c.cacheDir, fmt.Sprintf("%s:%s:config.json", fileName, manifest.Config.Digest)))
		},
	})
	if err != nil {
		return err
	}

	return parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
		layerTasks := make([]Task, 0, len(manifest.Layers))
		for _, l := range manifest.Layers {
			l := l
			layerTasks = append(layerTasks, Task{
				ID: fmt.Sprintf("docker:blob:%s", l.Digest),
				Do: func(ctx context.Context) error {
					return c.fetchBlob(ctx, l.Digest, filepath.Join(c.cacheDir, l.Digest+".tgz"))
				},
			})
		}

		doneCh := make(chan Task)
		spawn("tasks", parallel.Continue, func(ctx context.Context) error {
			log.Info("Fetching blobs")

			if err := c.reactor.AwaitTasks(ctx, doneCh, layerTasks...); err != nil {
				return err
			}

			log.Info("Blobs fetched")

			return nil
		})
		spawn("inflate", parallel.Exit, func(ctx context.Context) error {
			layers := manifest.Layers
			tasks := layerTasks
			completed := map[string]Task{}

			for {
				var t Task
				select {
				case <-ctx.Done():
					return errors.WithStack(ctx.Err())
				case t = <-doneCh:
				}

				completed[t.ID] = t

				for {
					if _, exists := completed[tasks[0].ID]; !exists {
						break
					}

					l := layers[0]

					blobFile := filepath.Join(c.cacheDir, l.Digest+".tgz")
					log.Info("Inflating blob", zap.String("blobFile", blobFile))

					f, err := os.Open(blobFile)
					if err != nil {
						return errors.WithStack(err)
					}
					defer f.Close()

					hasher := sha256.New()
					gr, err := gzip.NewReader(io.TeeReader(f, hasher))
					if err != nil {
						return errors.WithStack(err)
					}
					defer gr.Close()

					if err := untar(gr); err != nil {
						return err
					}

					computedDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
					if computedDigest != l.Digest {
						return errors.Errorf("blob digest doesn't match, expected: %s, got: %s", l.Digest, computedDigest)
					}

					log.Info("Blob inflated", zap.String("blobFile", blobFile))

					if len(layers) == 1 {
						return nil
					}

					layers = layers[1:]
					tasks = tasks[1:]
				}
			}
		})

		return nil
	})
}

func (c *imageClient) authorize(ctx context.Context, currentAuthToken string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if currentAuthToken != c.authToken {
		return c.authToken, nil
	}

	err := retry.Do(ctx, retry.FixedConfig{RetryAfter: 5 * time.Second}, func() error {
		resp, err := c.c.Do(must.HTTPRequest(http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			fmt.Sprintf("https://auth.docker.io/token?service=registry.docker.io&scope=repository:%s:pull", c.image),
			nil,
		)))
		if err != nil {
			return retry.Retriable(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return retry.Retriable(errors.Errorf("unexpected response status: %d", resp.StatusCode))
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return retry.Retriable(err)
		}

		data := struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"` //nolint:tagliatelle
		}{}
		if err := json.Unmarshal(body, &data); err != nil {
			return retry.Retriable(err)
		}
		if data.Token != "" {
			c.authToken = data.Token
			return nil
		}
		if data.AccessToken != "" {
			c.authToken = data.AccessToken
			return nil
		}
		return retry.Retriable(errors.New("no token in response"))
	})
	if err != nil {
		return "", err
	}
	return c.authToken, nil
}

func (c *imageClient) fetchManifest(ctx context.Context, dstFile string) (retErr error) {
	manifestURL := fmt.Sprintf("https://registry-1.docker.io/v2/%s/manifests/%s", c.image, c.tag)

	ctx = logger.With(ctx, zap.String("manifestURL", manifestURL), zap.String("dstPath", dstFile))
	log := logger.Get(ctx)
	log.Info("Fetching manifest")

	defer func() {
		if retErr == nil {
			log.Info("Manifest fetched")
		} else {
			_ = os.Remove(dstFile)
		}
	}()

	if err := os.MkdirAll(filepath.Dir(dstFile), 0o700); err != nil {
		return errors.WithStack(err)
	}

	f, err := os.OpenFile(dstFile, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return errors.WithStack(err)
	}
	defer f.Close()

	var authToken string
	return retry.Do(ctx, retry.FixedConfig{RetryAfter: 5 * time.Second}, func() error {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return errors.WithStack(err)
		}
		if err := f.Truncate(0); err != nil {
			return errors.WithStack(err)
		}

		var err error
		authToken, err = c.authorize(ctx, authToken)
		if err != nil {
			return err
		}

		req := must.HTTPRequest(http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			manifestURL,
			nil,
		))
		req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.v2+json")
		req.Header.Add("Authorization", "Bearer "+authToken)

		resp, err := c.c.Do(req)
		if err != nil {
			return retry.Retriable(err)
		}
		defer resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusUnauthorized:
			authToken = ""
			return retry.Retriable(errors.New("authorization required"))
		case http.StatusOK:
		default:
			return retry.Retriable(errors.Errorf("unexpected response status: %d", resp.StatusCode))
		}

		var r io.Reader = resp.Body
		var hasher hash.Hash
		if strings.HasPrefix(c.tag, "sha256:") {
			hasher = sha256.New()
			r = io.TeeReader(r, hasher)
		}

		_, err = io.Copy(f, r)
		if err != nil {
			return retry.Retriable(errors.WithStack(err))
		}

		if hasher != nil {
			computedDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
			if computedDigest != c.tag {
				if err := os.Remove(dstFile); err != nil {
					return errors.WithStack(err)
				}
				return retry.Retriable(errors.Errorf("digest doesn't match, expected: %s, got: %s", c.tag, computedDigest))
			}
		}

		return nil
	})
}

func (c *imageClient) fetchBlob(ctx context.Context, digest, dstFile string) (retErr error) {
	blobURL := fmt.Sprintf("https://registry-1.docker.io/v2/%s/blobs/%s", c.image, digest)

	ctx = logger.With(ctx, zap.String("blobURL", blobURL), zap.String("dstPath", dstFile))
	log := logger.Get(ctx)
	log.Info("Fetching blob")

	defer func() {
		if retErr == nil {
			log.Info("Blob fetched")
		} else {
			_ = os.Remove(dstFile)
		}
	}()

	if err := os.MkdirAll(filepath.Dir(dstFile), 0o700); err != nil {
		return errors.WithStack(err)
	}

	f, err := os.OpenFile(dstFile, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return errors.WithStack(err)
	}
	defer f.Close()

	var authToken string
	return retry.Do(ctx, retry.FixedConfig{RetryAfter: 5 * time.Second}, func() error {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return errors.WithStack(err)
		}
		if err := f.Truncate(0); err != nil {
			return errors.WithStack(err)
		}

		var err error
		authToken, err = c.authorize(ctx, authToken)
		if err != nil {
			return err
		}

		req := must.HTTPRequest(http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			blobURL,
			nil,
		))
		req.Header.Add("Authorization", "Bearer "+authToken)

		resp, err := c.c.Do(req)
		if err != nil {
			return retry.Retriable(err)
		}
		defer resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusUnauthorized:
			authToken = ""
			return retry.Retriable(errors.New("authorization required"))
		case http.StatusOK:
		default:
			return retry.Retriable(errors.Errorf("unexpected response status: %d", resp.StatusCode))
		}

		hasher := sha256.New()
		r := io.TeeReader(resp.Body, hasher)
		_, err = io.Copy(f, r)
		if err != nil {
			return retry.Retriable(errors.WithStack(err))
		}

		computedDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
		if computedDigest != digest {
			if err := os.Remove(dstFile); err != nil {
				return errors.WithStack(err)
			}
			return retry.Retriable(errors.Errorf("digest doesn't match, expected: %s, got: %s", digest, computedDigest))
		}

		return nil
	})
}

func untar(r io.Reader) error {
	tr := tar.NewReader(r)
	del := map[string]bool{}
	added := map[string]bool{}
loop:
	for {
		header, err := tr.Next()
		switch {
		case err == io.EOF:
			break loop
		case err != nil:
			return retry.Retriable(err)
		case header == nil:
			continue
		}
		if err := os.RemoveAll(header.Name); err != nil && !os.IsNotExist(err) {
			return errors.WithStack(err)
		}
		// We take mode from header.FileInfo().Mode(), not from header.Mode because they may be in different formats (meaning of bits may be different).
		// header.FileInfo().Mode() returns compatible value.
		mode := header.FileInfo().Mode()

		switch {
		case filepath.Base(header.Name) == ".wh..wh..plnk":
			// just ignore this
			continue
		case filepath.Base(header.Name) == ".wh..wh..opq":
			// It means that content in this directory created by earlier layers should not be visible,
			// so content created earlier should be deleted
			dir := filepath.Dir(header.Name)
			files, err := os.ReadDir(dir)
			if err != nil {
				return errors.WithStack(err)
			}
			for _, f := range files {
				toDelete := filepath.Join(dir, f.Name())
				if added[toDelete] {
					continue
				}
				if err := os.RemoveAll(toDelete); err != nil {
					return errors.WithStack(err)
				}
			}
			continue
		case strings.HasPrefix(filepath.Base(header.Name), ".wh."):
			// delete or mark to delete corresponding file
			toDelete := filepath.Join(filepath.Dir(header.Name), strings.TrimPrefix(filepath.Base(header.Name), ".wh."))
			delete(added, toDelete)
			if err := os.RemoveAll(toDelete); err != nil {
				if os.IsNotExist(err) {
					del[toDelete] = true
					continue
				}
				return errors.WithStack(err)
			}
			continue
		case del[header.Name]:
			delete(del, header.Name)
			delete(added, header.Name)
			continue
		case header.Typeflag == tar.TypeDir:
			if err := os.MkdirAll(header.Name, mode); err != nil {
				return errors.WithStack(err)
			}
		case header.Typeflag == tar.TypeReg:
			f, err := os.OpenFile(header.Name, os.O_CREATE|os.O_WRONLY, mode)
			if err != nil {
				return errors.WithStack(err)
			}
			_, err = io.Copy(f, tr)
			_ = f.Close()
			if err != nil {
				return errors.WithStack(err)
			}
		case header.Typeflag == tar.TypeSymlink:
			if err := os.Symlink(header.Linkname, header.Name); err != nil {
				return errors.WithStack(err)
			}
		case header.Typeflag == tar.TypeLink:
			// linked file may not exist yet, so let's create it - i will be overwritten later
			f, err := os.OpenFile(header.Linkname, os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				if !os.IsExist(err) {
					return errors.WithStack(err)
				}
			} else {
				_ = f.Close()
			}
			if err := os.Link(header.Linkname, header.Name); err != nil {
				return errors.WithStack(err)
			}
		default:
			return errors.Errorf("unsupported file type: %d", header.Typeflag)
		}

		added[header.Name] = true
		if err := os.Lchown(header.Name, header.Uid, header.Gid); err != nil {
			return errors.WithStack(err)
		}

		// Unless CAP_FSETID capability is set for the process every operation modifying the file/dir will reset
		// setuid, setgid nd sticky bits. After saving those files/dirs the mode has to be set once again to set those bits.
		// This has to be the last operation on the file/dir.
		// On linux mode is not supported for symlinks, mode is always taken from target location.
		if header.Typeflag != tar.TypeSymlink {
			if err := os.Chmod(header.Name, mode); err != nil {
				return errors.WithStack(err)
			}
		}
	}
	return nil
}

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
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/outofforest/libexec"
	"github.com/outofforest/logger"
	"github.com/outofforest/parallel"
	"github.com/pkg/errors"
	"github.com/ridge/must"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"github.com/outofforest/isolator/lib/retry"
	"github.com/outofforest/isolator/lib/task"
)

var userGroupRegExp = regexp.MustCompile("^[0-9]+(:[0-9]+)?$")

// InflateImageConfig is the configuration of docker image inflation.
type InflateImageConfig struct {
	HTTPClient *http.Client
	CacheDir   string
	Image      string
	Tag        string
}

// RunContainerConfig is the configuration of running docker container.
type RunContainerConfig struct {
	CacheDir   string
	Image      string
	Tag        string
	Name       string
	EnvVars    map[string]string
	User       string
	WorkingDir string
	Entrypoint []string
	Args       []string

	StdOut io.Writer
	StdErr io.Writer
}

// InflateImage downloads and inflates docker image in the current directory.
func InflateImage(ctx context.Context, config InflateImageConfig) error {
	imageClient := newImageClient(config.HTTPClient, config.Image, config.Tag, config.CacheDir)
	return imageClient.Inflate(ctx)
}

// RunContainer runs container based on docker image.
func RunContainer(ctx context.Context, config RunContainerConfig) error {
	imageClient := newImageClient(nil, config.Image, config.Tag, config.CacheDir)
	return imageClient.RunContainer(ctx, config)
}

type manifest struct {
	MediaType string `json:"mediaType"`
	Config    struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
	} `json:"config"`
	Layers []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
	} `json:"layers"`
}

type containerConfig struct {
	Config struct {
		User       string
		Env        []string
		Entrypoint []string
		Cmd        []string
		WorkingDir string
	} `json:"config"`
}

type imageClient struct {
	c        *http.Client
	image    string
	tag      string
	cacheDir string

	mu        sync.Mutex
	authToken string
}

func newImageClient(
	c *http.Client,
	image, tag string,
	cacheDir string,
) *imageClient {
	if !strings.Contains(image, "/") {
		image = "library/" + image
	}
	return &imageClient{
		c:        c,
		image:    image,
		tag:      tag,
		cacheDir: cacheDir,
	}
}

func (c *imageClient) Inflate(ctx context.Context) error {
	fileName := strings.ReplaceAll(c.image, "/", ":")
	manifestPath := filepath.Join(c.cacheDir, fmt.Sprintf("%s:%s:manifest.json", fileName, c.tag))

	ctx = logger.With(ctx,
		zap.String("image", c.image+":"+c.tag),
		zap.String("manifestPath", manifestPath),
	)
	log := logger.Get(ctx)
	log.Info("Docker image requested")

	return task.Run(ctx, make(chan task.Task), func(ctx context.Context, taskCh chan<- task.Task, doneCh <-chan task.Task) error {
		select {
		case <-ctx.Done():
			return errors.WithStack(ctx.Err())
		case taskCh <- task.Task{
			ID: fmt.Sprintf("docker:manifest:%s:%s", c.image, c.tag),
			Do: func(ctx context.Context) error {
				return c.fetchManifest(ctx, manifestPath)
			},
		}:
		}

		select {
		case <-ctx.Done():
			return errors.WithStack(ctx.Err())
		case <-doneCh:
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

		var m manifest
		if err := json.NewDecoder(r).Decode(&m); err != nil {
			return errors.WithStack(err)
		}

		if hasher != nil {
			computedDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
			if computedDigest != c.tag {
				return retry.Retriable(errors.Errorf("manifest digest doesn't match, expected: %s, got: %s", c.tag, computedDigest))
			}
		}

		if m.MediaType != "application/vnd.docker.distribution.manifest.v2+json" {
			return errors.Errorf("unsupprted media type %s for manifest", m.MediaType)
		}
		if m.Config.MediaType != "application/vnd.docker.container.image.v1+json" {
			return errors.Errorf("unsupprted media type %s for config", m.Config.MediaType)
		}

		return parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
			layerTasks := make([]task.Task, 0, len(m.Layers))
			for _, l := range m.Layers {
				l := l

				if l.MediaType != "application/vnd.docker.image.rootfs.diff.tar.gzip" {
					return errors.Errorf("unsupprted media type %s for layer", l.MediaType)
				}

				layerTasks = append(layerTasks, task.Task{
					ID: fmt.Sprintf("docker:blob:%s", l.Digest),
					Do: func(ctx context.Context) error {
						return c.fetchBlob(ctx, l.Digest, filepath.Join(c.cacheDir, l.Digest+".tgz"))
					},
				})
			}

			spawn("blobs", parallel.Continue, func(ctx context.Context) error {
				for _, t := range append([]task.Task{
					{
						ID: fmt.Sprintf("docker:config:%s:%s", c.image, m.Config.Digest),
						Do: func(ctx context.Context) error {
							return c.fetchBlob(ctx, m.Config.Digest, filepath.Join(c.cacheDir, fmt.Sprintf("%s:%s:config.json", fileName, m.Config.Digest)))
						},
					},
				}, layerTasks...) {
					select {
					case <-ctx.Done():
						return errors.WithStack(ctx.Err())
					case taskCh <- t:
					}
				}

				return nil
			})
			spawn("inflate", parallel.Exit, func(ctx context.Context) error {
				layers := m.Layers
				tasks := layerTasks
				completed := map[string]task.Task{}
				configDone := false

				for {
					var t task.Task
					select {
					case <-ctx.Done():
						return errors.WithStack(ctx.Err())
					case t = <-doneCh:
					}

					if strings.HasPrefix(t.ID, "docker:config:") {
						configDone = true
					} else {
						completed[t.ID] = t

						for len(layers) > 0 {
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

							layers = layers[1:]
							tasks = tasks[1:]
						}
					}

					if len(layers) == 0 && configDone {
						return nil
					}
				}
			})

			return nil
		})
	})
}

func (c *imageClient) RunContainer(ctx context.Context, config RunContainerConfig) error {
	fileName := strings.ReplaceAll(c.image, "/", ":")
	manifestPath := filepath.Join(c.cacheDir, fmt.Sprintf("%s:%s:manifest.json", fileName, c.tag))

	ctx = logger.With(ctx,
		zap.String("containerName", config.Name),
		zap.String("manifestPath", manifestPath),
	)
	log := logger.Get(ctx)
	log.Info("Starting container")

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

	var m manifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return errors.WithStack(err)
	}

	if hasher != nil {
		computedDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
		if computedDigest != c.tag {
			return retry.Retriable(errors.Errorf("manifest digest doesn't match, expected: %s, got: %s", c.tag, computedDigest))
		}
	}

	f.Close()

	configPath := filepath.Join(c.cacheDir, fmt.Sprintf("%s:%s:config.json", fileName, m.Config.Digest))
	f, err = os.Open(configPath)
	if err != nil {
		return errors.WithStack(err)
	}
	defer f.Close()

	hasher = sha256.New()
	var cc containerConfig
	if err := json.NewDecoder(io.TeeReader(f, hasher)).Decode(&cc); err != nil {
		return errors.WithStack(err)
	}

	_ = f.Close()

	computedDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if computedDigest != m.Config.Digest {
		return retry.Retriable(errors.Errorf("container config digest doesn't match, expected: %s, got: %s", m.Config.Digest, computedDigest))
	}

	if config.Entrypoint == nil {
		config.Entrypoint = cc.Config.Entrypoint
	}

	args := append([]string{}, config.Entrypoint...)
	if config.Args != nil || config.Entrypoint != nil {
		args = append(args, config.Args...)
	} else {
		args = append(args, cc.Config.Cmd...)
	}

	if len(args) == 0 {
		return errors.New("command to run hasn't been provided")
	}

	if config.WorkingDir == "" {
		config.WorkingDir = cc.Config.WorkingDir
	}

	var userID int
	var groupID int
	if config.User == "" {
		config.User = cc.Config.User
		if config.User != "" {
			if !userGroupRegExp.MatchString(config.User) {
				return errors.Errorf("invalid user: %s", config.User)
			}
			userParts := strings.SplitN(config.User, ":", 2)
			userID, _ = strconv.Atoi(userParts[0])

			if len(userParts) == 2 {
				groupID, _ = strconv.Atoi(userParts[1])
			}
		}
	}

	envVars := append(os.Environ(), cc.Config.Env...)
	for n, v := range config.EnvVars {
		envVars = append(envVars, n+"="+v)
	}

	cmd := &exec.Cmd{
		Path:   args[0],
		Args:   args,
		Env:    envVars,
		Dir:    config.WorkingDir,
		Stdout: config.StdOut,
		Stderr: config.StdErr,
		SysProcAttr: &syscall.SysProcAttr{
			Credential: &syscall.Credential{
				Uid: uint32(userID),
				Gid: uint32(groupID),
			},
		},
	}

	if err := libexec.Exec(ctx, cmd); err != nil {
		log.Error("Container exited with error", zap.Error(err))
		return err
	}

	log.Info("Container exited")

	return nil
}

func (c *imageClient) authorize(ctx context.Context, currentAuthToken string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if currentAuthToken != c.authToken {
		return c.authToken, nil
	}

	err := retry.Do(ctx, retry.FixedConfig{RetryAfter: 5 * time.Second, MaxAttempts: 10}, func() error {
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

	f, err := os.OpenFile(dstFile, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.WithStack(err)
	}
	defer f.Close()

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return errors.WithStack(err)
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN) //nolint:errcheck

	pos, err := f.Seek(0, unix.SEEK_END)
	if err != nil {
		return errors.WithStack(err)
	}

	if pos > 0 {
		return nil
	}

	var authToken string
	return retry.Do(ctx, retry.FixedConfig{RetryAfter: 5 * time.Second, MaxAttempts: 10}, func() error {
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

		if _, err := io.Copy(f, r); err != nil {
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

	f, err := os.OpenFile(dstFile, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.WithStack(err)
	}
	defer f.Close()

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return errors.WithStack(err)
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN) //nolint:errcheck

	pos, err := f.Seek(0, unix.SEEK_END)
	if err != nil {
		return errors.WithStack(err)
	}

	if pos > 0 {
		return nil
	}

	var authToken string
	return retry.Do(ctx, retry.FixedConfig{RetryAfter: 5 * time.Second, MaxAttempts: 10}, func() error {
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
		if _, err := io.Copy(f, r); err != nil {
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

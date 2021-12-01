package isolator

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wojciech-malota-wojcik/isolator/client"
	"github.com/wojciech-malota-wojcik/isolator/client/wire"
	"github.com/wojciech-malota-wojcik/isolator/generated"
)

const executorPath = ".executor"
const capSysAdmin = 21

// Start dumps executor to file, starts it, connects to it and returns client
func Start(ctx context.Context, dir string) (c *client.Client, cleanerFn func() error, err error) {
	fullExecutorPath := filepath.Join(dir, executorPath)
	var cmd *exec.Cmd
	cleanerFn = func() error {
		defer func() {
			_ = os.Remove(fullExecutorPath)
		}()
		if cmd == nil || cmd.Process == nil || (cmd.ProcessState != nil && cmd.ProcessState.Exited()) {
			return nil
		}
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			return fmt.Errorf("sending sigterm to executor failed: %w", err)
		}
		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("executor failed: %w", err)
		}
		return nil
	}
	defer func() {
		if err != nil {
			_ = cleanerFn()
		}
	}()

	if err := saveExecutor(fullExecutorPath); err != nil {
		return nil, nil, fmt.Errorf("saving executor executable failed: %w", err)
	}

	cmd = exec.Command("/" + executorPath)
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin", "LANG=en_US.UTF-8"}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Chroot:     dir,
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUSER | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS,
		// by adding CAP_SYS_ADMIN executor may mount /proc
		AmbientCaps: []uintptr{capSysAdmin},
		UidMappings: []syscall.SysProcIDMap{
			{
				HostID:      os.Getuid(),
				ContainerID: 0,
				Size:        1,
			},
		},
		GidMappings: []syscall.SysProcIDMap{
			{
				HostID:      os.Getgid(),
				ContainerID: 0,
				Size:        1,
			},
		},
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = bytes.NewReader(nil)
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("executor error: %w", err)
	}

	var conn net.Conn
	for i := 0; i < 100; i++ {
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return nil, nil, fmt.Errorf("executor exited before connection was made: %w", cmd.Wait())
		}
		conn, err = net.Dial("unix", filepath.Join(dir, wire.SocketPath))
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to executor failed: %w", err)
	}
	return client.New(conn), cleanerFn, nil
}

func saveExecutor(path string) error {
	gzr, err := gzip.NewReader(base64.NewDecoder(base64.RawStdEncoding, bytes.NewReader([]byte(generated.Executor))))
	if err != nil {
		return err
	}
	defer gzr.Close()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(file, gzr)
	return err
}

package isolator

import (
	"bytes"
	"context"
	"encoding/base64"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ridge/must"
	"github.com/wojciech-malota-wojcik/isolator/executor/wire"
	"github.com/wojciech-malota-wojcik/isolator/generated"
)

const executorPath = ".executor"
const capSysAdmin = 21

// Start dumps executor to file, starts it, connects to it and returns client
func Start(ctx context.Context, dir string) (conn net.Conn, closeFn func() error, err error) {
	fullExecutorPath := filepath.Join(dir, executorPath)
	var cmd *exec.Cmd
	closeFnTmp := func() error {
		defer func() {
			_ = os.Remove(fullExecutorPath)
		}()
		if cmd == nil || cmd.Process == nil || (cmd.ProcessState != nil && cmd.ProcessState.Exited()) {
			return nil
		}
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			return err
		}
		return cmd.Wait()
	}
	defer func() {
		if err != nil {
			_ = closeFnTmp()
		}
	}()

	if err := ioutil.WriteFile(fullExecutorPath, must.Bytes(base64.RawStdEncoding.DecodeString(generated.Executor)), 0o755); err != nil {
		return nil, nil, err
	}

	cmd = exec.Command("/" + executorPath)
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin", "LANG=en_US.UTF-8"}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Chroot:     dir,
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUSER,
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
		return nil, nil, err
	}

	for i := 0; i < 100; i++ {
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
		return nil, nil, err
	}
	return conn, closeFnTmp, err
}

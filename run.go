package isolator

import (
	"context"
	"encoding/base64"
	"io/ioutil"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/ridge/must"
	"github.com/wojciech-malota-wojcik/isolator/generated"
	"github.com/wojciech-malota-wojcik/libexec"
)

// Config is the configuration of isolator
type Config struct {
	// ExecutorPath is the file path where executor binary will be saved
	ExecutorPath string

	// Address is the address executor will listen on
	Address string

	// RootDir is the directory which will become a root of filesystem
	RootDir string
}

// Run dumps executor to file and starts it
func Run(ctx context.Context, config Config) error {
	if err := ioutil.WriteFile(config.ExecutorPath, must.Bytes(base64.RawStdEncoding.DecodeString(generated.Executor)), 0o755); err != nil {
		return err
	}

	exePath, err := filepath.Rel(config.RootDir, config.ExecutorPath)
	if err != nil {
		return err
	}

	cmd := exec.Command(exePath, "--addr", config.Address)
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Chroot: config.RootDir,
	}
	return libexec.Exec(ctx, cmd)
}

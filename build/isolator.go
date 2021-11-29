package build

import (
	"context"
	"encoding/base64"
	"io/ioutil"
	"os"
	"os/exec"

	"github.com/wojciech-malota-wojcik/build"
	"github.com/wojciech-malota-wojcik/buildgo"
	"github.com/wojciech-malota-wojcik/libexec"
)

func buildExecutor(ctx context.Context) error {
	return buildgo.GoBuildPkg(ctx, "cmd/executor", "bin/executor", false)
}

func packExecutor() error {
	if err := os.RemoveAll("generated"); err != nil && !os.IsNotExist(err) {
		return err
	}
	content, err := ioutil.ReadFile("bin/executor")
	if err != nil {
		return err
	}
	encoded := base64.RawStdEncoding.EncodeToString(content)
	if err := os.Mkdir("generated", 0o755); err != nil {
		return err
	}
	return ioutil.WriteFile("generated/executor.go", []byte("package generated\n\n// Executor holds executor binary (it's all because golang dev team constantly refuses to implement process forking)\nconst Executor = \""+encoded+"\"\n"), 0o644)
}

func generate(deps build.DepsFunc) {
	deps(buildExecutor, packExecutor)
}

func buildApp(ctx context.Context, deps build.DepsFunc) error {
	deps(generate)
	return buildgo.GoBuildPkg(ctx, "cmd/isolator", "bin/isolator-app", false)
}

func runApp(ctx context.Context, deps build.DepsFunc) error {
	deps(buildApp)
	return libexec.Exec(ctx, exec.Command("./bin/isolator-app"))
}

package build

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"os"

	"github.com/outofforest/build"
	"github.com/outofforest/buildgo"
)

func buildExecutor(ctx context.Context) error {
	return buildgo.GoBuildPkg(ctx, "cmd/executor", "bin/executor", false)
}

func packExecutor() (retErr error) {
	defer func() {
		if retErr != nil {
			_ = os.RemoveAll("generated")
		}
	}()

	if err := os.RemoveAll("generated"); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Mkdir("generated", 0o755); err != nil {
		return err
	}

	ef, err := os.Open("bin/executor")
	if err != nil {
		return err
	}
	defer ef.Close()

	of, err := os.OpenFile("generated/executor.go", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer of.Close()

	encoder := base64.NewEncoder(base64.RawStdEncoding, of)
	defer encoder.Close()

	gzw := gzip.NewWriter(encoder)
	defer gzw.Close()

	if _, err := of.WriteString("package generated\n\n// Executor holds executor binary (it's all because golang dev team constantly refuses to implement process forking)\nconst Executor = \""); err != nil {
		return err
	}
	if _, err := io.Copy(gzw, ef); err != nil {
		return err
	}
	if err := gzw.Close(); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	_, err = of.WriteString("\"\n")
	return err
}

func buildApp(ctx context.Context, deps build.DepsFunc) error {
	deps(buildExecutor, packExecutor)
	return nil
}

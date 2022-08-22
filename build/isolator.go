package build

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"os"

	"github.com/outofforest/build"
	"github.com/outofforest/buildgo"
	"github.com/pkg/errors"
)

func buildExecutor(ctx context.Context, deps build.DepsFunc) error {
	deps(buildgo.EnsureGo)
	return buildgo.GoBuildPkg(ctx, "cmd/executor", "bin/executor", false)
}

func packExecutor() (retErr error) {
	defer func() {
		if retErr != nil {
			_ = os.RemoveAll("generated")
		}
	}()

	if err := os.RemoveAll("generated"); err != nil && !os.IsNotExist(err) {
		return errors.WithStack(err)
	}
	if err := os.Mkdir("generated", 0o755); err != nil {
		return errors.WithStack(err)
	}

	ef, err := os.Open("bin/executor")
	if err != nil {
		return errors.WithStack(err)
	}
	defer ef.Close()

	//nolint:nosnakecase // Dependency
	of, err := os.OpenFile("generated/executor.go", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return errors.WithStack(err)
	}
	defer of.Close()

	encoder := base64.NewEncoder(base64.RawStdEncoding, of)
	defer encoder.Close()

	gzw := gzip.NewWriter(encoder)
	defer gzw.Close()

	if _, err := of.WriteString("package generated\n\n// Executor holds executor binary (it's all because golang dev team constantly refuses to implement process forking)\nconst Executor = \""); err != nil {
		return errors.WithStack(err)
	}
	if _, err := io.Copy(gzw, ef); err != nil {
		return errors.WithStack(err)
	}
	if err := gzw.Close(); err != nil {
		return errors.WithStack(err)
	}
	if err := encoder.Close(); err != nil {
		return errors.WithStack(err)
	}
	_, err = of.WriteString("\"\n")
	return errors.WithStack(err)
}

func buildApp(ctx context.Context, deps build.DepsFunc) error {
	deps(buildExecutor, packExecutor)
	return nil
}

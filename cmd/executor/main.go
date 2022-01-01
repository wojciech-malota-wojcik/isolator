package main

import (
	"github.com/outofforest/isolator/executor"
	"github.com/outofforest/run"
)

func main() {
	run.Tool("executor", nil, executor.Run)
}

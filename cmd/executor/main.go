package main

import (
	"github.com/wojciech-malota-wojcik/isolator/executor"
	"github.com/wojciech-malota-wojcik/run"
)

func main() {
	run.Tool("executor", nil, executor.Run)
}
